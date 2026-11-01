// Package cli 定义 tunlr 的 cobra 命令。这里只做参数解析、调用 pkg/core、渲染输出，
// 不放任何业务逻辑。
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
)

// rootEnv 把整套磁盘布局移到给定目录下，仅供测试使用。
const rootEnv = "TUNLR_ROOT"

// BuildInfo 是构建时注入的版本信息。
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// globalOptions 对应所有命令共享的全局参数。
type globalOptions struct {
	JSON    bool
	Yes     bool
	DryRun  bool
	Verbose bool
}

// app 保存一次命令执行所需的上下文，各子命令通过闭包访问。
type app struct {
	info   BuildInfo
	opts   globalOptions
	layout paths.Layout
	stdin  *os.File
	stdout io.Writer
	stderr io.Writer
	log    *slog.Logger
	// style、errStyle 是标准输出、标准错误的样式（不是终端时不加样式）。
	style, errStyle styles
	// live 是正在显示 spinner 的进度，日志经由它输出。
	live atomic.Pointer[progress]
	// session 是不随 Ctrl-C 取消的 context（只随 SIGTERM），主菜单在其中循环，
	// Ctrl-C 只中断菜单里正在执行的那条命令。
	session context.Context
	// aborted 表示用户取消了提问或在确认时回答否，什么都没做（见 cancelled、ask），主菜单显示为已取消。
	aborted bool
	// fullDiff 对应 --full-diff：差异很长时也完整显示，而不是按段汇总。
	fullDiff bool

	// 以下供测试替换，为空时使用默认实现。
	validator pipeline.Validator
	reloader  pipeline.Reloader
	download  *download.Client
	// terminal 报告标准输入是否为终端。
	terminal func() bool
	// editor 让用户编辑文件，默认运行 $VISUAL 或 $EDITOR。
	editor func(path string) error
	// prompt 向用户提问，默认使用 huh。
	prompt prompter
	// screenSize 返回标准输出所在终端的列数和行数，不是终端时为 0（不清屏、不分页）。
	screenSize func() (cols, rows int)
}

// Main 执行命令并返回进程退出码。收到 SIGINT 或 SIGTERM 时取消命令的 context。
func Main(info BuildInfo, args []string) int {
	a := &app{
		info:   info,
		layout: paths.Layout{Root: os.Getenv(rootEnv)},
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		log:    slog.New(slog.DiscardHandler),
	}
	// 不能只看是否为字符设备：/dev/null 也是（systemd 运行的命令、< /dev/null 都会误判为终端）。
	a.terminal = func() bool { return term.IsTerminal(a.stdin.Fd()) }
	a.style, a.errStyle = detectStyles(os.Stdout), detectStyles(os.Stderr)
	a.editor = a.runEditor
	a.prompt = huhPrompter{a}
	a.screenSize = func() (int, int) {
		if !term.IsTerminal(os.Stdout.Fd()) {
			return 0, 0
		}
		cols, rows, err := term.GetSize(os.Stdout.Fd())
		if err != nil {
			return 0, 0
		}
		return cols, rows
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	return a.execute(ctx, args)
}

// execute 执行命令并返回进程退出码。
func (a *app) execute(ctx context.Context, args []string) int {
	code, _ := a.run(ctx, args)
	return code
}

// run 执行一条命令，返回退出码，以及命令是否被 Ctrl-C（SIGINT）中断。Ctrl-C 取消的只是这条
// 命令的 context，ctx 本身不受影响。
func (a *app) run(ctx context.Context, args []string) (code int, interrupted bool) {
	if a.session == nil {
		a.session = ctx
	}
	a.aborted = false
	cctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	root := a.newRootCommand()
	root.SetArgs(args)
	err := root.ExecuteContext(cctx)
	interrupted = cctx.Err() != nil && ctx.Err() == nil
	if err == nil {
		return exitOK, interrupted
	}
	code = exitCode(err)
	a.printError(err, code)
	return code, interrupted
}

func (a *app) newRootCommand() *cobra.Command {
	var showVersion bool
	root := &cobra.Command{
		Use:   "tunlr",
		Short: "Operate mihomo on Linux servers",
		Long: "tunlr manages mihomo on Linux servers: kernel, systemd service, config pipeline\n" +
			"and transparent proxy. Runtime interaction is left to the web panel (zashboard).",
		// 根命令接收任意参数，以便把未知子命令报告为用法错误（退出码 2）。
		Args: cobra.ArbitraryArgs,
		// cobra 只在自己报告未知命令时设置默认值，这里由 RunE 报告，需要显式设置。
		SuggestionsMinimumDistance: 2,
		SilenceErrors:              true,
		SilenceUsage:               true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			level := slog.LevelInfo
			if a.opts.Verbose {
				level = slog.LevelDebug
			}
			a.log = slog.New(slog.NewTextHandler(logWriter{a}, &slog.HandlerOptions{Level: level}))
			cmd.SetContext(download.WithProgress(cmd.Context(), a.downloadProgress))
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion && len(args) == 0 {
				return a.printVersion()
			}
			if len(args) == 0 {
				// 在终端里只输入 tunlr 时进入主菜单，否则显示帮助。
				if a.interactive() {
					return a.runMenu(cmd)
				}
				return cmd.Help()
			}
			hint := ""
			if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
				hint = "; did you mean " + strings.Join(s, ", ") + "?"
			}
			return usageErrorf("unknown command %q%s", args[0], hint)
		},
	}

	// 版本只有 tunlr --version，没有 version 子命令；不用 cobra 的版本参数，以便支持 --json。
	root.Flags().BoolVar(&showVersion, "version", false, "print the tunlr version")
	pf := root.PersistentFlags()
	pf.BoolVar(&a.opts.JSON, "json", false, "output machine-readable JSON")
	pf.BoolVarP(&a.opts.Yes, "yes", "y", false, "assume yes for all confirmations")
	pf.BoolVar(&a.opts.DryRun, "dry-run", false, "show what would change without changing anything")
	pf.BoolVarP(&a.opts.Verbose, "verbose", "v", false, "enable debug logging")

	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usageError{err}
	})

	// shell 补全和 help 子命令照常可用，只是不显示在帮助中（cobra 的模板总是列出 help，这里去掉）。
	root.CompletionOptions.HiddenDefaultCmd = true
	root.SetUsageTemplate(strings.ReplaceAll(root.UsageTemplate(), `(or .IsAvailableCommand (eq .Name "help"))`, ".IsAvailableCommand"))
	root.SetHelpCommand(&cobra.Command{Use: "help [command]", Hidden: true, GroupID: "overview",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := root.Find(args)
			if err != nil || c == nil {
				return usageErrorf("unknown help topic %q", strings.Join(args, " "))
			}
			return c.Help()
		}})

	// 命令树见 docs/DESIGN.md 第 10 节：顶层只放领域名词和总览命令，--help 按分组显示。主菜单分得更细，
	// 每个分类的命令都在同一个分组中（Maintenance 除外）。
	groups := []struct {
		id, title string
		cmds      []*cobra.Command
	}{
		{"overview", "Overview:", []*cobra.Command{a.newStatusCommand(), a.newDoctorCommand()}},
		{"setup", "Setup:", []*cobra.Command{a.newInitCommand(), a.newUpgradeCommand(), a.newUninstallCommand()}},
		{"config", "Configuration:", []*cobra.Command{a.newSubCommand(), a.newConfigCommand(), a.newUpdateCommand()}},
		{"network", "Network:", []*cobra.Command{a.newTunCommand(), a.newProxyCommand(), a.newDNSCommand(), a.newIPv6Command(), a.newFilterCommand(),
			a.newDevicesCommand()}},
		{"kernel", "Kernel and panel:", []*cobra.Command{a.newCoreCommand(), a.newPanelCommand()}},
		{"maintenance", "Maintenance:", []*cobra.Command{a.newSettingsCommand(), a.newBackupCommand()}},
	}
	for _, g := range groups {
		root.AddGroup(&cobra.Group{ID: g.id, Title: g.title})
		for _, c := range g.cmds {
			c.GroupID = g.id
			root.AddCommand(c)
		}
	}
	// systemd 执行的内部命令，不显示在帮助中。
	root.AddCommand(
		a.newRescueCommand(),
		a.newInternalUpdateCommand(),
		a.newInternalAftercareCommand(),
		a.newPreStartCommand(),
		a.newPostStopCommand(),
		a.newTunGuardCommand(),
		a.newCheckCommand(),
		a.newFirewallCommand(),
	)
	return root
}

type versionData struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func (a *app) printVersion() error {
	d := versionData{
		Version: a.info.Version,
		Commit:  a.info.Commit,
		Date:    a.info.Date,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
	return a.render(d, func() error {
		_, err := fmt.Fprintf(a.stdout, "tunlr %s (commit %s, built %s, %s %s/%s)\n",
			d.Version, d.Commit, d.Date, d.Go, d.OS, d.Arch)
		return err
	})
}

// interactive 报告能否向用户提问。标准输入不是终端，或要求 JSON 输出时，
// 进入非交互模式：问答必须全部由参数给出，否则报错退出。
func (a *app) interactive() bool {
	return !a.opts.JSON && a.terminal()
}
