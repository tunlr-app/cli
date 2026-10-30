package cli

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/setup"
)

func (a *app) installer() *setup.Installer {
	return &setup.Installer{
		Layout: a.layout, Version: a.info.Version, Download: a.download, Log: a.log,
		Validator: a.validator, Reloader: a.reloader,
	}
}

func (a *app) kernels() (*mihomo.Manager, error) {
	owner, err := service.KernelUser(a.layout)
	if err != nil {
		return nil, err
	}
	return &mihomo.Manager{
		Layout: a.layout, Service: &service.Systemd{Layout: a.layout},
		Download: a.download, Owner: owner, Log: a.log,
	}, nil
}

// lanUsage 是 init 之后要开启的局域网功能（init 询问的“这台服务器的用途”，或 --lan-proxy 等参数）。
type lanUsage struct {
	// proxy 让局域网设备使用代理端口（tunlr proxy lan on）。
	proxy bool
	// gateway 不为空时开启网关模式（tunlr tun gateway on），"auto" 表示默认路由所在的网卡。网关模式也开启 proxy。
	gateway string
	// panel 让局域网设备打开面板（tunlr panel lan on）。
	panel bool
	// interfaces 是代理端口和面板放行的网卡（--lan-interface），为 nil 时取默认值；public 是询问用途时
	// 确认过的、有公网地址的网卡，在默认值之外另外放行。
	interfaces []string
	public     string
}

func (u lanUsage) any() bool { return u.proxy || u.gateway != "" || u.panel }

// choice 是 proxy lan on、panel lan on 放行的入口。
func (u lanUsage) choice() lanChoice {
	c := lanChoice{interfaces: u.interfaces}
	if u.public != "" {
		c.also = []string{u.public}
	}
	return c
}

// through 是步骤说明中放行的网卡，取默认值时为空。
func (u lanUsage) through() string {
	switch {
	case u.interfaces != nil:
		return ", through " + cmp.Or(strings.Join(u.interfaces, ", "), "no interface")
	case u.public != "":
		return ", also through " + u.public
	}
	return ""
}

// steps 是确认问题中列出的、init 之后要做的步骤。
func (u lanUsage) steps() []string {
	var steps []string
	if u.gateway != "" {
		iface := u.gateway
		if iface == "auto" {
			iface = "the interface with the IPv4 default route"
		}
		steps = append(steps, "Turn the transparent proxy (TUN) on, checked with automatic rollback",
			"Turn gateway mode on, LAN interface "+iface)
	}
	if u.proxy {
		steps = append(steps, "Let LAN devices use the proxy port (private addresses only"+u.through()+")")
	}
	if u.panel {
		steps = append(steps, "Let LAN devices open the panel (private addresses only"+u.through()+"; mihomo restarts)")
	}
	return steps
}

func (a *app) newInitCommand() *cobra.Command {
	var mirror, sub, version, kernelFile string
	var include, exclude []string
	var usage lanUsage
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Install or repair tunlr: kernel, systemd service and scheduled updates",
		Long: "Set up this server: copy tunlr to /usr/local/bin, create the user tunlr that runs\n" +
			"mihomo, install the latest mihomo release (checksum verified), install and enable\n" +
			"tunlr-core.service, tunlr-update.timer (see: tunlr update) and tunlr-aftercare.path\n" +
			"(checks mihomo after the panel upgrades it), add a subscription,\n" +
			"publish the configuration and start mihomo. Running it again repairs the installation.\n\n" +
			"Interactively it asks for a GitHub mirror and a subscription with its proxy filter (all\n" +
			"optional), and what this server is for: only itself, a proxy for LAN devices (tunlr proxy\n" +
			"lan on), or their gateway (tunlr tun gateway on); otherwise pass --mirror, --sub\n" +
			"<name>=<url>, --include, --exclude, --lan-proxy, --gateway and --lan-panel (with\n" +
			"--lan-interface).",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if usage.gateway != "" {
				usage.proxy = true
			}
			if cmd.Flags().Changed("lan-interface") {
				if !usage.proxy && !usage.panel {
					return usageErrorf("--lan-interface chooses the interfaces for --lan-proxy, --gateway or --lan-panel")
				}
				if usage.interfaces == nil {
					usage.interfaces = []string{}
				}
			}
			if usage.any() && a.opts.JSON {
				return usageErrorf("--lan-proxy, --gateway and --lan-panel cannot be combined with --json; run init, then tunlr proxy lan on, tunlr tun gateway on or tunlr panel lan on with --json")
			}
			opts := setup.InitOptions{KernelVersion: version, KernelFile: kernelFile}
			if cmd.Flags().Changed("mirror") {
				opts.Mirror = &mirror
			}
			if kernelFile != "" && version != "" {
				return usageErrorf("--kernel-file installs the version in the file's name; it cannot be combined with --version")
			}
			if version != "" {
				v, err := mihomo.NormalizeVersion(version)
				if err != nil {
					return err
				}
				opts.KernelVersion = v
			}
			if sub != "" {
				name, url, ok := strings.Cut(sub, "=")
				if !ok || name == "" || url == "" {
					return usageErrorf("--sub must be <name>=<url>")
				}
				opts.SubName, opts.SubURL = name, url
			}
			opts.SubFilter, opts.SubExclude = strings.Join(include, "`"), strings.Join(exclude, "`")
			if (opts.SubFilter != "" || opts.SubExclude != "") && sub == "" {
				return usageErrorf("--include and --exclude filter the subscription given with --sub")
			}
			inst := a.installer()
			if err := inst.CheckSupport(ctx); err != nil {
				return err
			}
			// 写错的网卡名在动手之前拒绝，免得装好内核后才在开启局域网访问时失败。
			if len(usage.interfaces) > 0 {
				if err := inst.CheckInterfaces(inst.LANAddresses(ctx, os.Getenv("SSH_CONNECTION")), usage.interfaces...); err != nil {
					return err
				}
			}
			if err := a.requireYes("init"); err != nil {
				return err
			}
			// 已经开启过透明代理、网关或代理端口局域网访问的不再问用途，也不再提示。
			configured, err := inst.LANConfigured()
			if err != nil {
				return err
			}
			if a.interactive() {
				if _, err := a.cancelled(a.askInit(cmd, &opts)); err != nil || a.aborted {
					return err
				}
				if !usage.any() && !configured {
					if _, err := a.cancelled(a.askLANUsage(ctx, &opts, &usage)); err != nil || a.aborted {
						return err
					}
				}
			}
			if !a.opts.Yes {
				steps, err := inst.InitSteps(ctx, opts)
				if err != nil {
					return err
				}
				steps = append(steps, usage.steps()...)
				if ok, err := a.ask(ctx, "Set up tunlr now?", bullets(steps), "Nothing changed."); !ok {
					return err
				}
			}
			var done func(error)
			opts.Progress, done = a.stepper()
			res, err := inst.Init(ctx, opts)
			done(err)
			if err != nil {
				return err
			}
			hint := !usage.any() && !configured && res.Started != nil
			if err := a.renderInit(res, hint, usage.panel); err != nil || !usage.any() {
				return err
			}
			return a.initLAN(ctx, res, usage)
		},
	}
	f := cmd.Flags()
	f.StringVar(&mirror, "mirror", "", `GitHub download mirror prefix, e.g. "https://gh-proxy.com/" ("" for direct)`)
	f.StringVar(&sub, "sub", "", "add a subscription, as <name>=<url>")
	f.StringArrayVar(&include, "include", nil, "with --sub: keep only proxies whose names match this regular expression (repeatable)")
	f.StringArrayVar(&exclude, "exclude", nil, "with --sub: drop proxies whose names match this regular expression (repeatable)")
	f.StringVar(&version, "version", "", "mihomo version to install when none is installed (default: latest release)")
	f.StringVar(&kernelFile, "kernel-file", "", "install mihomo from this downloaded release asset instead (e.g. mihomo-linux-amd64-v3-v1.19.33.gz;\n"+
		"still verified against the official sha256)")
	f.BoolVar(&usage.proxy, "lan-proxy", false, "then let LAN devices use the proxy port (tunlr proxy lan on)")
	f.StringVar(&usage.gateway, "gateway", "", `then make this server the LAN gateway on this interface, or "auto" (tunlr tun gateway on; implies --lan-proxy)`)
	f.BoolVar(&usage.panel, "lan-panel", false, "then let LAN devices open the panel (tunlr panel lan on)")
	f.StringSliceVar(&usage.interfaces, "lan-interface", nil, "with --lan-proxy, --gateway or --lan-panel: let in through these interfaces only, comma-separated\n"+
		"(--interface of tunlr proxy lan on; default: those without a public address)")
	return cmd
}

// initLAN 在 init 成功之后开启选定的局域网功能，依次走 tun gateway on、proxy lan on、panel lan on 的流程。
// 这些步骤已经在 init 的确认中列出，不再逐一确认。失败时说明 tunlr 已经装好，保留原错误的退出码。
func (a *app) initLAN(ctx context.Context, res *setup.InitResult, u lanUsage) error {
	if res.Started == nil {
		return fmt.Errorf("%w: tunlr is installed, but mihomo is not running, so the LAN setup is skipped; "+
			"once it runs, use tunlr proxy lan on, tunlr tun gateway on or tunlr panel lan on", core.ErrCheckFailed)
	}
	a.opts.Yes = true
	step := func(title, what string, run func() error) error {
		fmt.Fprintf(a.stdout, "\n%s\n\n", a.style.title(title))
		if err := run(); err != nil {
			return fmt.Errorf("tunlr is installed, but %s failed: %w", what, err)
		}
		return nil
	}
	if u.gateway != "" {
		iface := u.gateway
		if iface == "auto" {
			iface = ""
		}
		if err := step("Gateway mode", "turning gateway mode on", func() error { return a.gatewayOn(ctx, iface) }); err != nil {
			return err
		}
	}
	choice := u.choice()
	if u.proxy {
		if err := step("Proxy port", "opening the proxy port to the LAN", func() error { return a.proxyLAN(ctx, true, choice) }); err != nil {
			return err
		}
	}
	if u.panel {
		return step("Panel", "opening the panel to the LAN", func() error { return a.panelLANOn(ctx, choice) })
	}
	return nil
}

// askLANUsage 询问这台服务器的用途。没有订阅（也没有在 askInit 中添加）时不问：内核不会启动，
// 开启不了这些功能。
func (a *app) askLANUsage(ctx context.Context, opts *setup.InitOptions, u *lanUsage) error {
	if opts.SubName == "" {
		entries, err := a.subscriptions().List()
		if err != nil || len(entries) == 0 {
			return err
		}
	}
	cands, err := a.installer().LANInterfaces(ctx)
	if err != nil {
		return err
	}
	return a.askUsage(ctx, cands, u)
}

// askUsage 询问这台服务器的用途，cands 是局域网网卡的候选（见 setup.LANInterfaces）。选择网关且有多个
// 候选时再选网卡，网卡有公网地址时再确认一次；选择局域网代理或网关时再问是否让局域网设备打开面板。
func (a *app) askUsage(ctx context.Context, cands []setup.LANInterface, u *lanUsage) error {
	options := []string{
		"Only this server (e.g. a VPS; LAN devices do not use it)",
		"Proxy only for LAN devices: they set this server as their HTTP/SOCKS5 proxy",
		"Gateway and proxy for LAN devices: as their router and DNS, or as their proxy",
	}
	for {
		choice, err := a.prompt.Choose(ctx, "What is this server for?", options, 0)
		if err != nil || choice == 0 {
			return err
		}
		var iface *setup.LANInterface
		switch {
		case len(cands) == 0 && choice == 2:
			fmt.Fprintln(a.stdout, "No interface is up with an IPv4 address, so gateway mode is not possible.")
			continue
		case len(cands) == 0:
		case choice == 2 && len(cands) > 1:
			if iface, err = a.chooseInterface(ctx, cands); err != nil {
				return err
			}
		default:
			iface = &cands[0]
		}
		if iface != nil && len(iface.PublicAddresses) > 0 {
			ok, err := a.prompt.Confirm(ctx, fmt.Sprintf("%s has the public address %s. Use it for the LAN anyway?",
				iface.Name, strings.Join(iface.PublicAddresses, ", ")),
				"This server is probably reachable from the internet rather than on a LAN you use. Only private\n"+
					"addresses are let in, but a server that is not on your LAN should be \"only this server\".")
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			// 有公网地址的网卡默认不放行：确认过的，代理端口和面板另外放行从它进来的连接（网关的局域网网卡
			// 总是放行，不用另加）。
			if choice == 1 {
				u.public = iface.Name
			}
		}
		u.proxy = true
		if choice == 2 {
			u.gateway = "auto"
			if iface != nil {
				u.gateway = iface.Name
			}
		}
		panel, err := a.prompt.Choose(ctx, "Let LAN devices open the panel (zashboard) too?", []string{
			"Yes: phones and laptops at home open it directly (the API secret travels unencrypted)",
			"No: open it through an SSH tunnel (tunlr panel shows how)",
		}, 0)
		if err != nil {
			return err
		}
		u.panel = panel == 0
		return nil
	}
}

// renderInit 输出 init 的结果。hint 为 true 时提示以后怎样让本机全部流量或局域网设备经过代理；
// panelNext 为 true 时接下来开启面板的局域网访问，那一步会给出链接，这里不提示怎样打开面板。
func (a *app) renderInit(res *setup.InitResult, hint, panelNext bool) error {
	return a.render(res, func() error {
		w := a.stdout
		fmt.Fprintln(w)
		if res.Kernel != nil {
			fmt.Fprintf(w, "Installed mihomo %s (%s, checksum verified).\n", res.Kernel.To, res.Kernel.Asset)
		}
		if res.SubscriptionAdded != "" {
			fmt.Fprintf(w, "Added the subscription %q.\n", res.SubscriptionAdded)
		}
		if res.Revision > 0 {
			fmt.Fprintf(w, "Published revision %d.\n", res.Revision)
		}
		if res.Started != nil {
			fmt.Fprintf(w, "mihomo %s is running (pid %d) and passed the health check.\n", res.Started.Version, res.Started.MainPID)
			switch {
			case panelNext:
			case res.PanelLAN:
				fmt.Fprintln(w, "Open the panel (zashboard): LAN access is on; tunlr panel shows the links.")
			default:
				fmt.Fprintln(w, "Open the panel (zashboard): tunlr panel shows the SSH tunnel and the link.")
			}
		} else {
			fmt.Fprintf(w, "mihomo is not started: %s.\n", res.NotStarted)
		}
		if hint {
			fmt.Fprintln(w, "Only this server uses the proxy port (tunlr proxy). tunlr tun on sends all its traffic\n"+
				"through mihomo; tunlr proxy lan on or tunlr tun gateway on serves devices on the LAN.")
		}
		return nil
	})
}

// askInit 在交互模式下询问参数中没有给出的 GitHub 镜像和订阅。
func (a *app) askInit(cmd *cobra.Command, opts *setup.InitOptions) error {
	ctx := cmd.Context()
	if opts.Mirror == nil {
		set, err := settings.Load(a.layout.SettingsFile())
		if err != nil {
			return err
		}
		// 输入框预填当前的设置，还没有时预填建议的镜像；清空即直连。
		desc := "Downloads from GitHub are slow from mainland China; a mirror only carries the bytes,\n" +
			"tunlr still verifies the official sha256. Clear it to download from github.com directly."
		m, err := a.prompt.Input(ctx, "GitHub mirror (optional)", desc, cmp.Or(set.GitHubMirror, settings.SuggestedMirror), func(s string) error {
			if s == "" {
				return nil
			}
			return settings.ValidateMirror(s)
		})
		if err != nil {
			return err
		}
		opts.Mirror = &m
	}
	if opts.SubName == "" {
		entries, err := a.subscriptions().List()
		if err != nil || len(entries) > 0 {
			return err
		}
		url, err := a.prompt.Input(ctx, "Subscription URL (optional)",
			"The URL your provider gives for Clash/mihomo. Leave empty to add one later with: tunlr sub add", "", nil)
		if err != nil || url == "" {
			return err
		}
		opts.SubName, opts.SubURL = "default", url
		if !cmd.Flags().Changed("include") && !cmd.Flags().Changed("exclude") {
			if opts.SubFilter, opts.SubExclude, err = a.askFilter(cmd, opts.SubName); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *app) newStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start mihomo and check that it is healthy",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := a.kernels()
			if err != nil {
				return err
			}
			var h *mihomo.Health
			err = a.spin("Starting mihomo and checking its health", func() (err error) {
				h, err = k.Start(cmd.Context())
				return err
			})
			if err != nil {
				return err
			}
			return a.renderHealth(h)
		},
	}
}

// sectionList 是“ in 有变化的顶层键”，太多时只列前几个；只有文件开头的注释变了时为空。
func sectionList(keys []string) string {
	const max = 4
	switch {
	case len(keys) == 0:
		return ""
	case len(keys) > max:
		return " in " + strings.Join(keys[:max], ", ") + fmt.Sprintf(" and %d more", len(keys)-max)
	}
	return " in " + strings.Join(keys, ", ")
}

// bullets 把每项写成一行，用作确认问题下的说明。
func bullets(items []string) string {
	return "• " + strings.Join(items, "\n• ")
}

func (a *app) newRestartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart mihomo and check that it is healthy",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireYes("core restart"); err != nil {
				return err
			}
			k, err := a.kernels()
			if err != nil {
				return err
			}
			// 没有安装时不问，直接报告（见 Restart）。
			if k.Service.Installed() {
				if ok, err := a.ask(cmd.Context(), "Restart mihomo?", "Open connections drop for a few seconds.", "Not restarted."); !ok {
					return err
				}
			}
			var h *mihomo.Health
			err = a.spin("Restarting mihomo and checking its health", func() (err error) {
				h, err = k.Restart(cmd.Context())
				return err
			})
			if err != nil {
				return err
			}
			return a.renderHealth(h)
		},
	}
}

func (a *app) renderHealth(h *mihomo.Health) error {
	return a.render(h, func() error {
		_, err := fmt.Fprintf(a.stdout, "%s (pid %d) and passed the health check.\n", a.style.good(fmt.Sprintf("mihomo %s is running", h.Version)), h.MainPID)
		return err
	})
}

func (a *app) newStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop mihomo",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.requireYes("core stop"); err != nil {
				return err
			}
			k, err := a.kernels()
			if err != nil {
				return err
			}
			// 没有安装时不问，直接报告（见 Stop）。
			if !a.opts.Yes && k.Service.Installed() {
				desc := "Nothing goes through the proxy until you start it again (tunlr core start, or at boot)."
				if s, err := a.installer().TunStatus(ctx); err == nil && s.Switch != "off" {
					desc += "\nTUN is on: this server's own traffic goes out directly meanwhile."
				}
				if ok, err := a.ask(ctx, "Stop mihomo?", desc, "Not stopped."); !ok {
					return err
				}
			}
			if err := k.Stop(ctx); err != nil {
				return err
			}
			return a.render(map[string]bool{"stopped": true}, func() error {
				_, err := fmt.Fprintln(a.stdout, "mihomo is stopped. It starts again at boot; run tunlr core start to start it now.")
				return err
			})
		},
	}
}

func (a *app) newLogsCommand() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show the mihomo log (journalctl -u tunlr-core.service)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.opts.JSON {
				return usageErrorf("core logs does not support --json; use journalctl -u %s -o json", service.CoreUnit)
			}
			if lines < 0 {
				return usageErrorf("--lines must not be negative")
			}
			sd := &service.Systemd{Layout: a.layout}
			if follow || a.rows() == 0 {
				return sd.Logs(cmd.Context(), lines, follow, a.stdout, a.stderr)
			}
			// 在终端里先收集起来，超过一屏时交给分页器，从最新的一条开始显示。
			var buf strings.Builder
			if err := sd.Logs(cmd.Context(), lines, false, &buf, a.stderr); err != nil {
				return err
			}
			return a.page(buf.String(), true)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new entries")
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "number of recent entries to show")
	return cmd
}

func (a *app) newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the service, kernel, subscription and configuration status",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.installer().Status(cmd.Context())
			if err != nil {
				return err
			}
			return a.render(s, func() error {
				a.printStatus(s)
				// 快速诊断（不联网、不运行 mihomo -t），只在有失败项时提示；诊断本身出错时不影响 status。
				r, _ := a.installer().Doctor(cmd.Context(), setup.DoctorOptions{Quick: true})
				a.printProblems(r)
				return nil
			})
		},
	}
}

func (a *app) printStatus(s *setup.Status) {
	w := a.stdout
	a.printCore(&setup.CoreStatus{Installed: s.Installed, Service: s.Service, Running: s.Running, Kernel: s.Kernel})
	if n := describeAvailable(s.UpdateCheck); n != "" {
		fmt.Fprintf(w, "Available:     %s\n", a.style.warn(n))
	}
	if s.Subscription != "" {
		fmt.Fprintf(w, "Subscription:  %s\n", s.Subscription)
	} else {
		fmt.Fprintf(w, "Subscription:  %s (add one with: tunlr sub add)\n", a.style.warn("none"))
	}
	a.printRevision(s.Revision, s.Drift)
	for i, u := range s.Updates {
		label := "Updates:"
		if i > 0 {
			label = ""
		}
		fmt.Fprintf(w, "%-15s%s\n", label, a.describeSchedule(u))
	}
	if s.Tun != nil {
		fmt.Fprintf(w, "TUN:           %s\n", a.describeTun(s.Tun))
	}
	if s.Installed {
		fmt.Fprintf(w, "Panel:         %s (open it with: tunlr panel)\n", a.describePanelLAN(s.PanelLAN))
		fmt.Fprintf(w, "Proxy port:    %s (details: tunlr proxy)\n", a.describeProxyLAN(s.ProxyLAN))
		if s.LANAccess != nil {
			fmt.Fprintf(w, "Let in:        %s\n", a.describeAccess(*s.LANAccess))
		}
	}
	if s.Devices != nil {
		fmt.Fprintf(w, "LAN devices:   %s (list them: tunlr devices)\n", a.describeDevices(s.Devices))
	}
	if s.Filter != nil {
		fmt.Fprintf(w, "Filters:       %s (details: tunlr filter)\n", a.describeFilter(s.Filter))
	}
}

// describeAvailable 是 status 里可用的更新和装它们的命令，如 "tunlr 0.2.0 (tunlr upgrade) · mihomo v1.19.34
// (tunlr core upgrade)"；没有更新（或还没有查询过）时为空。
func describeAvailable(c *setup.UpdateCheck) string {
	if c == nil {
		return ""
	}
	var parts []string
	if c.Tool != "" {
		parts = append(parts, fmt.Sprintf("tunlr %s (tunlr upgrade)", c.Tool))
	}
	if c.Kernel != "" {
		parts = append(parts, fmt.Sprintf("mihomo %s (tunlr core upgrade)", c.Kernel))
	}
	return strings.Join(parts, " · ")
}

// printCore 输出内核服务的状态：服务、运行中的版本、已知可用的版本（status 与 core 共用）。
func (a *app) printCore(s *setup.CoreStatus) {
	w := a.stdout
	switch {
	case !s.Installed:
		fmt.Fprintf(w, "Service:       %s (run: tunlr init)\n", a.style.warn("not installed"))
	case s.Service.Active():
		fmt.Fprintf(w, "Service:       %s, pid %d\n", a.style.good(fmt.Sprintf("%s (%s)", s.Service.ActiveState, s.Service.SubState)), s.Service.MainPID)
	case s.Service.ActiveState == "failed":
		fmt.Fprintf(w, "Service:       %s\n", a.style.bad(fmt.Sprintf("%s (%s)", s.Service.ActiveState, s.Service.SubState)))
	default:
		fmt.Fprintf(w, "Service:       %s\n", a.style.warn(fmt.Sprintf("%s (%s)", s.Service.ActiveState, s.Service.SubState)))
	}
	k := s.Kernel
	switch {
	case s.Running != "":
		note := ""
		if !k.KnownGood {
			note = " (not yet a known-good version)"
		}
		fmt.Fprintf(w, "mihomo:        %s%s\n", a.style.good(s.Running+" running"), a.style.warn(note))
	case !k.Installed:
		fmt.Fprintf(w, "mihomo:        %s\n", a.style.warn("not installed"))
	case k.KnownGood:
		fmt.Fprintf(w, "mihomo:        %s, %s\n", k.Version, a.style.warn("not running"))
	default:
		fmt.Fprintf(w, "mihomo:        installed, %s\n", a.style.warn("not running"))
	}
	if len(k.Kernels) > 0 {
		vs := make([]string, len(k.Kernels))
		for i, kg := range k.Kernels {
			vs[i] = kg.Version
		}
		fmt.Fprintf(w, "Known-good:    %s\n", strings.Join(vs, ", "))
	}
}

// printRevision 输出生效配置的修订和漂移（status 与 config 共用）。
func (a *app) printRevision(rev *config.Revision, d *pipeline.Drift) {
	w := a.stdout
	if rev != nil {
		fmt.Fprintf(w, "Configuration: revision %d, published %s\n", rev.Number, rev.CreatedAt.Local().Format(time.DateTime))
	}
	if d == nil {
		return
	}
	switch d.State {
	case pipeline.DriftPending:
		fmt.Fprintf(w, "               %s%s (tunlr config plan shows them)\n", a.style.warn("changes not published"), sectionList(d.Sections))
	case pipeline.DriftEdited:
		fmt.Fprintf(w, "Configuration: %s%s (tunlr config apply overwrites it)\n", a.style.warn("edited outside tunlr"), sectionList(d.Sections))
	case pipeline.DriftError:
		msg, _, _ := strings.Cut(d.Error, "\n")
		fmt.Fprintf(w, "               %s: %s (details: tunlr config plan)\n", a.style.bad("cannot compose a new one"), msg)
	}
}

// describeDevices 用于 status 和菜单的设备摘要，如 "3 online, 1 idle"。
func (a *app) describeDevices(d *setup.DeviceSummary) string {
	parts := []string{fmt.Sprintf("%d online", d.Online)}
	if d.Idle > 0 {
		parts = append(parts, fmt.Sprintf("%d idle", d.Idle))
	}
	s := strings.Join(parts, ", ")
	if d.Online > 0 {
		s = a.style.good(s)
	}
	return s
}

func (a *app) newUninstallCommand() *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop mihomo and remove the service, kernels and the user tunlr",
		Long: "Stop and disable the mihomo service and the daily update, remove their units,\n" +
			"the kernels, mihomo's working directory and the user tunlr. Settings, mixins,\n" +
			"subscriptions, history and /usr/local/bin/tunlr are kept so tunlr init can set\n" +
			"everything up again; --purge removes them too.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireYes("uninstall"); err != nil {
				return err
			}
			question := "Uninstall tunlr (mihomo stops; settings and subscriptions are kept)?"
			if purge {
				question = "Uninstall tunlr and delete all settings, subscriptions and history?"
			}
			if ok, err := a.ask(cmd.Context(), question, "", "Not uninstalled."); !ok {
				return err
			}
			res, err := a.installer().Uninstall(cmd.Context(), purge)
			if err != nil {
				return err
			}
			return a.render(res, func() error {
				for _, p := range res.Removed {
					fmt.Fprintf(a.stdout, "Removed %s\n", p)
				}
				if len(res.Kept) > 0 {
					fmt.Fprintf(a.stdout, "Kept %s (remove them with --purge).\n", strings.Join(res.Kept, ", "))
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete settings, mixins, subscriptions, history and /usr/local/bin/tunlr")
	return cmd
}

func (a *app) newUpgradeCommand() *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade tunlr itself to the latest release (or --version)",
		Long: "Download the tunlr release for this platform (the latest, or --version), verify it against\n" +
			"the official sha256, check that it runs and replace /usr/local/bin/tunlr; the new tunlr then\n" +
			"runs init --yes to refresh the systemd units. mihomo is not touched (that is tunlr core\n" +
			"upgrade). To go back, pass the earlier version with --version. A build without a release\n" +
			"version (dev) must be given --version.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if version != "" {
				v, err := setup.NormalizeToolVersion(version)
				if err != nil {
					return err
				}
				version = v
			}
			if err := a.requireYes("upgrade"); err != nil {
				return err
			}
			ctx := cmd.Context()
			inst := a.installer()
			if !a.opts.Yes {
				var target string
				err := a.spin("Looking up the tunlr release", func() (err error) {
					target, err = inst.UpgradeTarget(ctx, version)
					return err
				})
				if err != nil {
					return err
				}
				if target != strings.TrimPrefix(a.info.Version, "v") {
					desc := "The running tunlr keeps working until it exits; afterwards the new one runs init --yes\n" +
						"to refresh the systemd units (mihomo restarts only if its configuration changes)."
					if ok, err := a.ask(ctx, fmt.Sprintf("Replace tunlr %s with %s?", a.info.Version, target), desc, "tunlr not changed."); !ok {
						return err
					}
				}
			}
			var res *setup.UpgradeResult
			err := a.spin("Upgrading tunlr (download, verify, replace, init)", func() (err error) {
				res, err = inst.Upgrade(ctx, version)
				return err
			})
			if err != nil {
				return err
			}
			return a.render(res, func() error {
				w := a.stdout
				switch {
				case !res.Changed:
					fmt.Fprintf(w, "tunlr %s is already installed.\n", res.To)
				case res.Reinstalled:
					fmt.Fprintf(w, "tunlr %s is installed at %s (%s); it refreshed the service units.\n", res.To, res.Path, res.Asset)
				default:
					fmt.Fprintf(w, "tunlr %s is installed at %s (%s).\n", res.To, res.Path, res.Asset)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "install this version instead of the latest release, e.g. v0.2.0")
	return cmd
}

func (a *app) newCoreCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "core",
		Short: "Show the mihomo kernel and its service; start, stop, restart, logs, upgrade, rollback",
		Long: "Without a subcommand, show the state of tunlr-core.service, the running mihomo version\n" +
			"and the known-good versions kept for rollback.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.installer().CoreStatus(cmd.Context())
			if err != nil {
				return err
			}
			return a.render(s, func() error { a.printCore(s); return nil })
		},
	}
	cmd.AddCommand(
		a.newStartCommand(),
		a.newStopCommand(),
		a.newRestartCommand(),
		a.newLogsCommand(),
		a.newCoreUpgradeCommand(),
		a.newCoreRollbackCommand(),
	)
	return cmd
}

func (a *app) newCoreUpgradeCommand() *cobra.Command {
	var version, file string
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade mihomo to the latest release (or --version, or from --file)",
		Long: "Download mihomo (the latest release, or --version), verify it against the official\n" +
			"sha256, check the published configuration with it (mihomo -t) and replace the running\n" +
			"kernel. If the new kernel does not pass the health check, the previous one is put back\n" +
			"(exit code 4). tunlr core rollback goes back to the previous known-good version.\n\n" +
			"--file installs a release asset downloaded elsewhere (its name gives the version, e.g.\n" +
			"mihomo-linux-amd64-v3-v1.19.33.gz); only the release data is fetched to verify its sha256.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if file != "" && version != "" {
				return usageErrorf("--file installs the version in the file's name; it cannot be combined with --version")
			}
			if version != "" {
				v, err := mihomo.NormalizeVersion(version)
				if err != nil {
					return err
				}
				version = v
			}
			return a.changeKernel(cmd, "core upgrade", false, version, file)
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "install this version instead of the latest release, e.g. v1.19.33")
	cmd.Flags().StringVar(&file, "file", "", "install this downloaded release asset instead, e.g. mihomo-linux-amd64-v3-v1.19.33.gz")
	return cmd
}

func (a *app) newCoreRollbackCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback",
		Short: "Switch back to the previous known-good mihomo version",
		Long: "Switch to the previous known-good mihomo kept on disk (tunlr core lists them). Nothing is\n" +
			"downloaded; it is checked against the published configuration (mihomo -t) and goes through\n" +
			"the same health check as an upgrade, with the current one put back if it fails (exit code 4).",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.changeKernel(cmd, "core rollback", true, "", "")
		},
	}
}

// changeKernel 是 core upgrade 与 core rollback 共同的流程：确认、替换内核、健康检查，失败时换回原来的。
// file 不为空时内核来自这个事先下载好的资产文件。
func (a *app) changeKernel(cmd *cobra.Command, name string, rollback bool, version, file string) error {
	if err := a.requireYes(name); err != nil {
		return err
	}
	k, err := a.kernels()
	if err != nil {
		return err
	}
	if !a.opts.Yes {
		if ok, err := a.confirmUpgrade(cmd.Context(), k, rollback, &version, file); !ok {
			return err
		}
	}
	var res *mihomo.UpgradeResult
	title := "Upgrading mihomo (download, verify, check, restart)"
	switch {
	case rollback:
		title = "Switching back to the previous known-good mihomo"
	case file != "":
		title = "Upgrading mihomo from " + file + " (verify, check, restart)"
	}
	err = a.spin(title, func() (err error) {
		switch {
		case rollback:
			res, err = k.Rollback(cmd.Context())
		case file != "":
			res, err = k.UpgradeFrom(cmd.Context(), file)
		default:
			res, err = k.Upgrade(cmd.Context(), version)
		}
		return err
	})
	if err != nil {
		return err
	}
	return a.render(res, func() error {
		w := a.stdout
		switch {
		case !res.Changed:
			fmt.Fprintf(w, "mihomo %s is already installed.\n", res.To)
		case res.Restarted:
			fmt.Fprintf(w, "mihomo %s (%s) is running and passed the health check.\n", res.To, res.Asset)
		default:
			fmt.Fprintf(w, "Installed mihomo %s (%s); it is not running and will be used when it starts.\n", res.To, res.Asset)
		}
		return nil
	})
}

// confirmUpgrade 在替换内核前确认，问题中给出现在和将换成的版本。升级时先查询目标版本并写回 version，
// 确认的是哪个版本就装哪个；已经是该版本时不问，交给 Upgrade 报告。
func (a *app) confirmUpgrade(ctx context.Context, k *mihomo.Manager, rollback bool, version *string, file string) (bool, error) {
	ks, err := k.Status()
	if err != nil {
		return false, err
	}
	var target string
	switch {
	case rollback:
		kg, err := k.RollbackTarget()
		if err != nil {
			return false, err
		}
		target = kg.Version
	case file != "":
		// 版本在文件名里，不用查询；校验在确认之后。
		if _, target, err = mihomo.ParseKernelAsset(filepath.Base(file)); err != nil {
			return false, err
		}
		if ks.KnownGood && ks.Version == target {
			return true, nil
		}
	default:
		err := a.spin("Looking up the mihomo release", func() (err error) {
			target, err = k.UpgradeTarget(ctx, *version)
			return err
		})
		if err != nil {
			return false, err
		}
		*version = target
		if ks.KnownGood && ks.Version == target {
			return true, nil
		}
	}
	var question string
	switch {
	case !ks.Installed:
		question = fmt.Sprintf("Install mihomo %s?", target)
	case rollback && ks.KnownGood:
		question = fmt.Sprintf("Switch mihomo %s back to %s?", ks.Version, target)
	case ks.KnownGood:
		question = fmt.Sprintf("Replace mihomo %s with %s?", ks.Version, target)
	default:
		question = fmt.Sprintf("Replace the installed mihomo with %s?", target)
	}
	desc := "It is checked against the published configuration first. A running mihomo restarts (open\n" +
		"connections drop), and the previous one is put back if the new one is not healthy."
	return a.ask(ctx, question, desc, "mihomo not changed.")
}

// newRescueCommand 是 tunlr-rescue.service 执行的内部命令，内核服务多次启动失败后由 systemd 触发。
func (a *app) newRescueCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "_rescue",
		Hidden: true,
		Args:   usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := a.kernels()
			if err != nil {
				return err
			}
			res, err := k.Rescue(cmd.Context())
			if err != nil {
				return err
			}
			return a.render(res, func() error {
				_, err := fmt.Fprintf(a.stdout, "Switched back to the known-good mihomo %s; it is running again.\n", res.To)
				return err
			})
		},
	}
}
