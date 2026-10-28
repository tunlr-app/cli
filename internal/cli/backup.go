package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/backup"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
)

// backupView 概述一份备份，用于 backup、restore 的输出。不含订阅地址等凭据。
type backupView struct {
	File          string          `json:"file,omitempty"`
	Host          string          `json:"host"`
	Tunlr         string          `json:"tunlr"`
	CreatedAt     time.Time       `json:"created_at"`
	Subscriptions []string        `json:"subscriptions"`
	Active        string          `json:"active,omitempty"`
	Mixins        []string        `json:"mixins"`
	Settings      bool            `json:"settings"`
	Devices       int             `json:"devices"`
	Switches      backup.Switches `json:"switches"`
}

func newBackupView(file string, b *backup.Backup) backupView {
	v := backupView{File: file, Host: b.Host, Tunlr: b.Tunlr, CreatedAt: b.CreatedAt, Active: b.Active,
		Subscriptions: []string{}, Mixins: []string{}, Settings: b.Settings != nil, Devices: len(b.Devices), Switches: b.Switches}
	for _, s := range b.Subscriptions {
		v.Subscriptions = append(v.Subscriptions, s.Name)
	}
	for _, m := range b.Mixins {
		v.Mixins = append(v.Mixins, m.Name)
	}
	return v
}

// printBackup 输出备份的内容概要。
func (a *app) printBackup(v backupView, b *backup.Backup) {
	w := a.stdout
	subs := a.style.dim("none")
	if len(v.Subscriptions) > 0 {
		names := make([]string, len(v.Subscriptions))
		for i, n := range v.Subscriptions {
			names[i] = n
			if n == v.Active {
				names[i] += " (active)"
			}
		}
		subs = strings.Join(names, ", ")
	}
	mixins := a.style.dim("none")
	if len(v.Mixins) > 0 {
		mixins = strings.Join(v.Mixins, ", ")
	}
	set := a.style.dim("defaults")
	if v.Settings {
		set = "settings.yaml"
	}
	fmt.Fprintf(w, "  Subscriptions:  %s\n", subs)
	fmt.Fprintf(w, "  Mixins:         %s\n", mixins)
	fmt.Fprintf(w, "  Tool settings:  %s\n", set)
	devices := a.style.dim("none")
	if v.Devices > 0 {
		devices = plural(v.Devices, "device", "devices") + " recorded"
	}
	fmt.Fprintf(w, "  Devices:        %s\n", devices)
	var filters []string
	if n := len(b.Filter.Sources); n > 0 {
		filters = append(filters, plural(n, "network", "networks")+" direct")
	}
	if b.Filter.QUIC {
		filters = append(filters, "QUIC blocked")
	}
	if b.Filter.Ports {
		filters = append(filters, "common ports only")
	}
	if len(filters) == 0 {
		filters = []string{a.style.dim("none")}
	}
	fmt.Fprintf(w, "  Filters:        %s\n", strings.Join(filters, " · "))
	fmt.Fprintf(w, "  Switched on:    %s\n", a.describeSwitches(v.Switches))
}

func (a *app) describeSwitches(s backup.Switches) string {
	var on []string
	switch {
	case s.Gateway != "":
		on = append(on, "TUN", "gateway ("+s.Gateway+")")
	case s.Tun:
		on = append(on, "TUN")
	}
	if s.ProxyLAN {
		on = append(on, "proxy port LAN access")
	}
	if s.PanelLAN {
		on = append(on, "panel LAN access")
	}
	if len(on) == 0 {
		return a.style.dim("nothing")
	}
	return strings.Join(on, ", ")
}

// savedBackupView 是 tunlr backup 列出的一份自动备份。
type savedBackupView struct {
	backupView
	Error string `json:"error,omitempty"`
}

// describeSaved 是自动备份的一行概要：时间、订阅、mixin 数。
func describeSaved(s setup.SavedBackup) string {
	if s.Err != nil {
		return filepath.Base(s.Path) + " (unreadable)"
	}
	b := s.Backup
	subs := "no subscriptions"
	if len(b.Subscriptions) > 0 {
		names := make([]string, len(b.Subscriptions))
		for i, sub := range b.Subscriptions {
			names[i] = sub.Name
			if sub.Name == b.Active {
				names[i] += " (active)"
			}
		}
		subs = strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s · %s · %s", b.CreatedAt.Local().Format("2006-01-02 15:04:05"), subs, plural(len(b.Mixins), "mixin", "mixins"))
}

// describeBackups 用于主菜单 Backup 分类的摘要。
func (a *app) describeBackups(n int) string {
	if n == 0 {
		return a.style.dim("no automatic backups")
	}
	return plural(n, "automatic backup", "automatic backups")
}

func (a *app) newBackupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up the settings, mixins and subscriptions, and restore them; lists the automatic backups",
		Long: "tunlr backup create saves the settings, mixins and subscriptions to a file, to restore here\n" +
			"or on another server with tunlr backup restore. Before each restore, the current contents are\n" +
			"saved to /var/lib/tunlr/backups (the last 3 are kept); tunlr backup lists them, and backup\n" +
			"restore without a file lets you choose one, to undo a restore.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			saved, err := a.installer().SavedBackups()
			if err != nil {
				return err
			}
			views := make([]savedBackupView, len(saved))
			for i, s := range saved {
				if s.Err != nil {
					views[i] = savedBackupView{backupView: backupView{File: s.Path}, Error: s.Err.Error()}
				} else {
					views[i] = savedBackupView{backupView: newBackupView(s.Path, s.Backup)}
				}
			}
			return a.render(views, func() error {
				if len(saved) == 0 {
					fmt.Fprintf(a.stdout, "No automatic backups yet (one is saved in %s before each restore).\n", a.layout.BackupsDir())
				} else {
					fmt.Fprintln(a.stdout, "Automatic backups, saved before each restore (newest first):")
					for _, s := range saved {
						fmt.Fprintf(a.stdout, "  %s\n    %s\n", describeSaved(s), a.style.dim(s.Path))
					}
				}
				fmt.Fprintln(a.stdout, "\n(tunlr backup create saves a backup to a file; tunlr backup restore restores one)")
				return nil
			})
		},
	}
	cmd.AddCommand(a.newBackupCreateCommand(), a.newBackupRestoreCommand())
	return cmd
}

func (a *app) newBackupCreateCommand() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Save the settings, mixins and subscriptions to a file, to restore here or on another server",
		Long: "Write one file (tar.gz, mode 0600) with the tool settings, the mixins, the subscriptions with\n" +
			"their last fetched content, the API secret, the recorded devices and filters, and which\n" +
			"switches are on. The kernel, geo data and configuration history are not included; they are\n" +
			"downloaded or built again. The file holds the subscription URLs with their tokens: keep it\n" +
			"private.\n\n" +
			"Without -o, the file is written to the current directory. -o - writes to standard output,\n" +
			"so a server can be moved in one go:\n\n" +
			"  ssh old-server tunlr backup create -o - | ssh new-server tunlr backup restore - --yes",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "-" && a.opts.JSON {
				return usageErrorf("-o - writes the backup to standard output and cannot be combined with --json")
			}
			b, err := a.installer().Backup(cmd.Context())
			if err != nil {
				return err
			}
			if out == "-" {
				return backup.Write(a.stdout, b)
			}
			if out == "" {
				host := b.Host
				if host == "" {
					host = "server"
				}
				out = fmt.Sprintf("tunlr-backup-%s-%s.tar.gz", host, b.CreatedAt.Local().Format("20060102-150405"))
			}
			err = backup.WriteFile(out, b)
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("%s already exists; write to another file with -o", out)
			}
			if err != nil {
				return err
			}
			if abs, err := filepath.Abs(out); err == nil {
				out = abs
			}
			v := newBackupView(out, b)
			return a.render(v, func() error {
				fmt.Fprintf(a.stdout, "Backed up to %s:\n", out)
				a.printBackup(v, b)
				fmt.Fprintf(a.stdout, "\n%s it holds the subscription URLs with their tokens and the API secret; keep it private.\n",
					a.style.warn("Note:"))
				fmt.Fprintf(a.stdout, "Restore it with: tunlr backup restore %s\n", out)
				return nil
			})
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "write the backup to this file, or - for standard output")
	return cmd
}

// restoreView 是 restore 在 --json 输出中的形式。
type restoreView struct {
	Backup  backupView           `json:"backup"`
	Plan    planView             `json:"plan"`
	Applied bool                 `json:"applied"`
	Result  *setup.RestoreResult `json:"result,omitempty"`
	// Commands 是还原后打开备份时开着的开关的命令。
	Commands []string `json:"commands,omitempty"`
}

func (a *app) newBackupRestoreCommand() *cobra.Command {
	return a.withFullDiff(&cobra.Command{
		Use:   "restore [file|-]",
		Short: "Restore a backup made with tunlr backup create (shows the diff first)",
		Long: "Replace the tool settings, the mixins, the subscriptions, the API secret, and the recorded\n" +
			"devices and filters with those in the backup (- reads it from standard input). The kernel,\n" +
			"the configuration history and the switches of this server stay as they are. Run tunlr init\n" +
			"first on a new server.\n\n" +
			"The configuration is composed and checked, and the diff is shown before you confirm. The\n" +
			"current contents are saved to /var/lib/tunlr/backups first (the last 3 are kept); if\n" +
			"publishing fails, they are put back.\n\n" +
			"Switches that were on in the backup (TUN, gateway mode, LAN access to the proxy port and\n" +
			"the panel) are not turned on by themselves: the network here may differ. Afterwards you\n" +
			"choose which to turn on, each with its usual checks; without a terminal, or with --yes,\n" +
			"the commands are listed instead.\n\n" +
			"Without a file, choose one of the automatic backups (see tunlr backup).",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !a.interactive() {
				return usageErrorf("backup restore needs the backup file (or - for standard input)")
			}
			if err := a.requireConfirmation("backup restore"); err != nil {
				return err
			}
			file := ""
			if len(args) == 1 {
				file = args[0]
			} else {
				var err error
				if file, err = a.chooseSavedBackup(cmd.Context()); file == "" || err != nil {
					return err
				}
			}
			return a.restore(cmd.Context(), file)
		},
	})
}

// chooseSavedBackup 让用户从自动备份中选一份，返回它的路径；没有自动备份或用户取消时返回空。
func (a *app) chooseSavedBackup(ctx context.Context) (string, error) {
	saved, err := a.installer().SavedBackups()
	if err != nil {
		return "", err
	}
	var usable []setup.SavedBackup
	for _, s := range saved {
		if s.Err == nil {
			usable = append(usable, s)
		}
	}
	if len(usable) == 0 {
		fmt.Fprintln(a.stdout, "No automatic backups here; restore a file with: tunlr backup restore <file>")
		return "", nil
	}
	options := make([]string, len(usable))
	for i, s := range usable {
		options[i] = describeSaved(s)
	}
	i, err := a.prompt.Choose(ctx, "Restore which automatic backup?", options, 0)
	if _, err := a.cancelled(err); err != nil || a.aborted {
		return "", err
	}
	return usable[i].Path, nil
}

func (a *app) restore(ctx context.Context, file string) error {
	b, err := backup.ReadFile(file, a.stdin)
	if err != nil {
		return err
	}
	inst := a.installer()
	v := restoreView{Backup: newBackupView("", b)}
	if file != "-" {
		v.Backup.File = file
	}
	if !a.opts.JSON {
		fmt.Fprintf(a.stdout, "Backup of %s, made %s by tunlr %s:\n", b.Host, b.CreatedAt.Local().Format("2006-01-02 15:04"), b.Tunlr)
		a.printBackup(v.Backup, b)
		fmt.Fprintln(a.stdout)
	}
	plan, err := a.plan(func() (*pipeline.Plan, error) { return inst.PlanRestore(ctx, b) })
	if err != nil {
		return err
	}
	v.Plan = newPlanView(plan, false)
	if !a.opts.JSON {
		a.printPlan(v.Plan)
	}
	if err := checkExit(plan, false); err != nil || a.opts.DryRun {
		if a.opts.JSON {
			if rerr := a.render(v, nil); rerr != nil {
				return rerr
			}
		} else if err != nil {
			fmt.Fprintln(a.stdout, "\nNot restored.")
		} else {
			fmt.Fprintln(a.stdout, "\nDry run: nothing restored.")
		}
		return err
	}
	if ok, err := a.ask(ctx, "Restore this backup?",
		"The current settings, mixins and subscriptions are saved to "+a.layout.BackupsDir()+" first.", "Not restored."); !ok {
		return err
	}
	err = a.spin("Restoring the backup", func() (err error) {
		v.Result, err = inst.Restore(ctx, b, plan)
		return err
	})
	if errors.Is(err, core.ErrRolledBack) {
		return fmt.Errorf("%w; the previous configuration and files are back in place", err)
	}
	if err != nil {
		return err
	}
	v.Applied = true

	sugg, err := inst.SuggestSwitches(ctx, b)
	if err != nil {
		return err
	}
	v.Commands = switchCommands(sugg)
	ask := sugg.Any() && a.interactive() && !a.opts.Yes
	if rerr := a.render(v, func() error {
		fmt.Fprintf(a.stdout, "\n%s\n", a.style.good("The backup is restored."))
		fmt.Fprintf(a.stdout, "The previous contents are saved in %s.\n", v.Result.Saved)
		if sugg.Any() && !ask {
			fmt.Fprintln(a.stdout, "\nThese were on in the backup and are off here; turn them on with:")
			for _, c := range v.Commands {
				fmt.Fprintf(a.stdout, "  %s\n", c)
			}
		}
		return nil
	}); rerr != nil || !ask {
		return rerr
	}
	return a.restoreSwitches(ctx, sugg)
}

// switchCommands 是打开建议的开关的命令。
func switchCommands(s setup.SwitchSuggestion) []string {
	var cmds []string
	switch {
	case s.Gateway && s.Interface != "":
		cmds = append(cmds, "tunlr tun gateway on --interface "+s.Interface)
	case s.Gateway:
		cmds = append(cmds, "tunlr tun gateway on")
	case s.Tun:
		cmds = append(cmds, "tunlr tun on")
	}
	if s.ProxyLAN {
		cmds = append(cmds, "tunlr proxy lan on")
	}
	if s.PanelLAN {
		cmds = append(cmds, "tunlr panel lan on")
	}
	return cmds
}

// restoreSwitches 让用户勾选要打开的开关（默认全选），再依次走 gateway on（或 tun on）、proxy lan on、
// panel lan on 的流程，同 init 的 initLAN：已经勾选过，不再逐一确认，但各自的检查照常进行。
func (a *app) restoreSwitches(ctx context.Context, s setup.SwitchSuggestion) error {
	type step struct {
		label, title string
		run          func() error
	}
	var steps []step
	switch {
	case s.Gateway:
		label := "Gateway mode on " + s.Interface + " (turns TUN on first)"
		if s.Interface == "" {
			label = "Gateway mode (was on " + s.Was + ", not found here; uses the default route's interface; turns TUN on first)"
		}
		steps = append(steps, step{label, "Gateway mode", func() error { return a.gatewayOn(ctx, s.Interface) }})
	case s.Tun:
		steps = append(steps, step{"Transparent proxy (TUN)", "Transparent proxy", func() error {
			warnings, err := a.installer().TunPreflight(ctx)
			if err != nil {
				return err
			}
			a.printWarnings(warnings)
			return a.tunOn(ctx, warnings)
		}})
	}
	if s.ProxyLAN {
		steps = append(steps, step{"LAN access to the proxy port", "Proxy port", func() error { return a.proxyLAN(ctx, true) }})
	}
	if s.PanelLAN {
		steps = append(steps, step{"LAN access to the panel", "Panel", func() error { return a.panelLANOn(ctx) }})
	}
	options := make([]string, len(steps))
	all := make([]int, len(steps))
	for i, st := range steps {
		options[i], all[i] = st.label, i
	}
	fmt.Fprintln(a.stdout)
	picked, err := a.prompt.ChooseMany(ctx, "These were on in the backup and are off here. Turn them on now?", options, all)
	if err != nil && !errors.Is(err, huh.ErrUserAborted) {
		return err
	}
	if len(picked) == 0 || err != nil {
		// 备份已经还原，不打开开关也不是什么都没做，不记为取消。
		fmt.Fprintln(a.stdout, "Left off; turn them on later with: "+strings.Join(switchCommands(s), ", "))
		return nil
	}
	a.opts.Yes = true
	for _, i := range picked {
		st := steps[i]
		fmt.Fprintf(a.stdout, "\n%s\n\n", a.style.title(st.title))
		if err := st.run(); err != nil {
			return fmt.Errorf("the backup is restored, but turning %s on failed: %w", strings.ToLower(st.title), err)
		}
	}
	return nil
}
