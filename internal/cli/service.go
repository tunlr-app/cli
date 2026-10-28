package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
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
	// gateway 不为空时开启网关模式（tunlr gateway on），"auto" 表示默认路由所在的网卡。网关模式也开启 proxy。
	gateway string
	// panel 让局域网设备打开面板（tunlr panel lan on）。
	panel bool
}

func (u lanUsage) any() bool { return u.proxy || u.gateway != "" || u.panel }

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
		steps = append(steps, "Let LAN devices use the proxy port (private addresses only)")
	}
	if u.panel {
		steps = append(steps, "Let LAN devices open the panel (mihomo restarts)")
	}
	return steps
}

func (a *app) newInitCommand() *cobra.Command {
	var mirror, sub, version string
	var usage lanUsage
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Install or repair tunlr: kernel, systemd service and scheduled updates",
		Long: "Set up this server: copy tunlr to /usr/local/bin, create the user tunlr that runs\n" +
			"mihomo, install the latest mihomo release (checksum verified), install and enable\n" +
			"tunlr-core.service and tunlr-update.timer (see: tunlr update show), add a subscription,\n" +
			"publish the configuration and start mihomo. Running it again repairs the installation.\n\n" +
			"Interactively it asks for a GitHub mirror and a subscription (both optional), and what\n" +
			"this server is for: only itself, a proxy for LAN devices (tunlr proxy lan on), or their\n" +
			"gateway (tunlr gateway on); otherwise pass --mirror, --sub <name>=<url>, --lan-proxy,\n" +
			"--gateway and --lan-panel.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if usage.gateway != "" {
				usage.proxy = true
			}
			if usage.any() && a.opts.JSON {
				return usageErrorf("--lan-proxy, --gateway and --lan-panel cannot be combined with --json; run init, then tunlr proxy lan on, tunlr gateway on or tunlr panel lan on with --json")
			}
			opts := setup.InitOptions{KernelVersion: version}
			if cmd.Flags().Changed("mirror") {
				opts.Mirror = &mirror
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
			inst := a.installer()
			if err := inst.CheckSupport(ctx); err != nil {
				return err
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
	f.StringVar(&version, "version", "", "mihomo version to install when none is installed (default: latest release)")
	f.BoolVar(&usage.proxy, "lan-proxy", false, "then let LAN devices use the proxy port (tunlr proxy lan on)")
	f.StringVar(&usage.gateway, "gateway", "", `then make this server the LAN gateway on this interface, or "auto" (tunlr gateway on; implies --lan-proxy)`)
	f.BoolVar(&usage.panel, "lan-panel", false, "then let LAN devices open the panel (tunlr panel lan on)")
	return cmd
}

// initLAN 在 init 成功之后开启选定的局域网功能，依次走 gateway on、proxy lan on、panel lan on 的流程。
// 这些步骤已经在 init 的确认中列出，不再逐一确认。失败时说明 tunlr 已经装好，保留原错误的退出码。
func (a *app) initLAN(ctx context.Context, res *setup.InitResult, u lanUsage) error {
	if res.Started == nil {
		return fmt.Errorf("%w: tunlr is installed, but mihomo is not running, so the LAN setup is skipped; "+
			"once it runs, use tunlr proxy lan on, tunlr gateway on or tunlr panel lan on", core.ErrCheckFailed)
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
	if u.proxy {
		if err := step("Proxy port", "opening the proxy port to the LAN", func() error { return a.proxyLAN(ctx, true) }); err != nil {
			return err
		}
	}
	if u.panel {
		return step("Panel", "opening the panel to the LAN", func() error { return a.panelLANOn(ctx) })
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
			rows := make([][]string, len(cands))
			for i, c := range cands {
				note := ""
				switch {
				case len(c.PublicAddresses) > 0:
					note = "public address"
				case c.Default:
					note = "default route"
				}
				rows[i] = []string{c.Name, strings.Join(c.Addresses, ", "), note}
			}
			i, err := a.prompt.Choose(ctx, "Which interface is on the LAN?", columns(rows), 0)
			if err != nil {
				return err
			}
			iface = &cands[i]
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
				"through mihomo; tunlr proxy lan on or tunlr gateway on serves devices on the LAN.")
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
		desc := "Downloads from GitHub are slow from mainland China; a mirror such as https://gh-proxy.com/\n" +
			"only carries the bytes, tunlr still verifies the official sha256. "
		if set.GitHubMirror != "" {
			desc += "Current: " + set.GitHubMirror + " (leave empty to keep it; --mirror \"\" removes it)."
		} else {
			desc += "Leave empty to download directly."
		}
		m, err := a.prompt.Input(ctx, "GitHub mirror (optional)", desc, func(s string) error {
			if s == "" {
				return nil
			}
			return settings.ValidateMirror(s)
		})
		if err != nil {
			return err
		}
		if m != "" || set.GitHubMirror == "" {
			opts.Mirror = &m
		}
	}
	if opts.SubName == "" {
		entries, err := a.subscriptions().List()
		if err != nil || len(entries) > 0 {
			return err
		}
		url, err := a.prompt.Input(ctx, "Subscription URL (optional)",
			"The URL your provider gives for Clash/mihomo. Leave empty to add one later with: tunlr sub add", nil)
		if err != nil || url == "" {
			return err
		}
		opts.SubName, opts.SubURL = "default", url
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
			if err := a.requireYes("restart"); err != nil {
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
			if err := a.requireYes("stop"); err != nil {
				return err
			}
			k, err := a.kernels()
			if err != nil {
				return err
			}
			// 没有安装时不问，直接报告（见 Stop）。
			if !a.opts.Yes && k.Service.Installed() {
				desc := "Nothing goes through the proxy until you start it again (tunlr start, or at boot)."
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
				_, err := fmt.Fprintln(a.stdout, "mihomo is stopped. It starts again at boot; run tunlr start to start it now.")
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
				return usageErrorf("logs does not support --json; use journalctl -u %s -o json", service.CoreUnit)
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
	if s.Subscription != "" {
		fmt.Fprintf(w, "Subscription:  %s\n", s.Subscription)
	} else {
		fmt.Fprintf(w, "Subscription:  %s (add one with: tunlr sub add)\n", a.style.warn("none"))
	}
	if s.Revision != nil {
		fmt.Fprintf(w, "Configuration: revision %d, published %s\n", s.Revision.Number, s.Revision.CreatedAt.Local().Format(time.DateTime))
	}
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
	}
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
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the mihomo kernel (upgrade core)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(a.newUpgradeCoreCommand())
	return cmd
}

func (a *app) newUpgradeCoreCommand() *cobra.Command {
	var version string
	var rollback bool
	cmd := &cobra.Command{
		Use:   "core",
		Short: "Upgrade mihomo to the latest release, or --rollback to the previous known-good one",
		Long: "Download mihomo (the latest release, or --version), verify it against the official\n" +
			"sha256, check the published configuration with it (mihomo -t) and replace the running\n" +
			"kernel. If the new kernel does not pass the health check, the previous one is put back\n" +
			"(exit code 4). --rollback switches to the previous known-good version the same way.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if rollback && version != "" {
				return usageErrorf("--rollback and --version cannot be used together")
			}
			if version != "" {
				v, err := mihomo.NormalizeVersion(version)
				if err != nil {
					return err
				}
				version = v
			}
			if err := a.requireYes("upgrade core"); err != nil {
				return err
			}
			k, err := a.kernels()
			if err != nil {
				return err
			}
			if !a.opts.Yes {
				if ok, err := a.confirmUpgrade(cmd.Context(), k, rollback, &version); !ok {
					return err
				}
			}
			var res *mihomo.UpgradeResult
			title := "Upgrading mihomo (download, verify, check, restart)"
			if rollback {
				title = "Switching back to the previous known-good mihomo"
			}
			err = a.spin(title, func() (err error) {
				if rollback {
					res, err = k.Rollback(cmd.Context())
				} else {
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
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "install this version instead of the latest release, e.g. v1.19.33")
	cmd.Flags().BoolVar(&rollback, "rollback", false, "switch back to the previous known-good version")
	return cmd
}

// confirmUpgrade 在替换内核前确认，问题中给出现在和将换成的版本。升级时先查询目标版本并写回 version，
// 确认的是哪个版本就装哪个；已经是该版本时不问，交给 Upgrade 报告。
func (a *app) confirmUpgrade(ctx context.Context, k *mihomo.Manager, rollback bool, version *string) (bool, error) {
	ks, err := k.Status()
	if err != nil {
		return false, err
	}
	var target string
	if rollback {
		kg, err := k.RollbackTarget()
		if err != nil {
			return false, err
		}
		target = kg.Version
	} else {
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
