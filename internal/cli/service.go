package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/setup"
)

func (a *app) installer() *setup.Installer {
	return &setup.Installer{Layout: a.layout, Version: a.info.Version, Download: a.download, Log: a.log}
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

func (a *app) newInitCommand() *cobra.Command {
	var mirror, sub, version string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Install or repair tunlr: kernel, systemd service and daily update",
		Long: "Set up this server: copy tunlr to /usr/local/bin, create the user tunlr that runs\n" +
			"mihomo, install the latest mihomo release (checksum verified), install and enable\n" +
			"tunlr-core.service and the daily tunlr-update.timer, add a subscription, publish the\n" +
			"configuration and start mihomo. Running it again repairs the installation.\n\n" +
			"Interactively it asks for a GitHub mirror and a subscription (both optional);\n" +
			"otherwise pass --mirror and --sub <name>=<url>.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			if err := setup.CheckEnvironment(); err != nil {
				return err
			}
			if a.interactive() {
				if err := a.askInit(cmd, &opts); err != nil {
					return err
				}
			}
			if !a.opts.JSON {
				opts.Progress = func(step string) { fmt.Fprintf(a.stdout, "==> %s\n", step) }
			}
			res, err := a.installer().Init(cmd.Context(), opts)
			if err != nil {
				return err
			}
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
					fmt.Fprintln(w, "Open the panel through an SSH tunnel to 127.0.0.1:9090 (tunlr status shows the details).")
				} else {
					fmt.Fprintf(w, "mihomo is not started: %s.\n", res.NotStarted)
				}
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&mirror, "mirror", "", `GitHub download mirror prefix, e.g. "https://gh-proxy.com/" ("" for direct)`)
	f.StringVar(&sub, "sub", "", "add a subscription, as <name>=<url>")
	f.StringVar(&version, "version", "", "mihomo version to install when none is installed (default: latest release)")
	return cmd
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
			h, err := k.Start(cmd.Context())
			if err != nil {
				return err
			}
			return a.renderHealth(h)
		},
	}
}

func (a *app) newRestartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart mihomo and check that it is healthy",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := a.kernels()
			if err != nil {
				return err
			}
			h, err := k.Restart(cmd.Context())
			if err != nil {
				return err
			}
			return a.renderHealth(h)
		},
	}
}

func (a *app) renderHealth(h *mihomo.Health) error {
	return a.render(h, func() error {
		_, err := fmt.Fprintf(a.stdout, "mihomo %s is running (pid %d) and passed the health check.\n", h.Version, h.MainPID)
		return err
	})
}

func (a *app) newStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop mihomo",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := a.kernels()
			if err != nil {
				return err
			}
			if err := k.Stop(cmd.Context()); err != nil {
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
			return sd.Logs(cmd.Context(), lines, follow, a.stdout, a.stderr)
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
			return a.render(s, func() error { a.printStatus(s); return nil })
		},
	}
}

func (a *app) printStatus(s *setup.Status) {
	w := a.stdout
	switch {
	case !s.Installed:
		fmt.Fprintln(w, "Service:       not installed (run: tunlr init)")
	case s.Service.Active():
		fmt.Fprintf(w, "Service:       %s (%s), pid %d\n", s.Service.ActiveState, s.Service.SubState, s.Service.MainPID)
	default:
		fmt.Fprintf(w, "Service:       %s (%s)\n", s.Service.ActiveState, s.Service.SubState)
	}
	k := s.Kernel
	switch {
	case s.Running != "":
		note := ""
		if !k.KnownGood {
			note = " (not yet a known-good version)"
		}
		fmt.Fprintf(w, "mihomo:        %s running%s\n", s.Running, note)
	case !k.Installed:
		fmt.Fprintln(w, "mihomo:        not installed")
	case k.KnownGood:
		fmt.Fprintf(w, "mihomo:        %s, not running\n", k.Version)
	default:
		fmt.Fprintln(w, "mihomo:        installed, not running")
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
		fmt.Fprintln(w, "Subscription:  none (add one with: tunlr sub add)")
	}
	if s.Revision != nil {
		fmt.Fprintf(w, "Configuration: revision %d, published %s\n", s.Revision.Number, s.Revision.CreatedAt.Local().Format(time.DateTime))
	}
	if !s.NextUpdate.IsZero() {
		fmt.Fprintf(w, "Next update:   %s\n", s.NextUpdate.Local().Format(time.DateTime))
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
			if a.opts.DryRun {
				return usageErrorf("uninstall does not support --dry-run")
			}
			if !a.opts.Yes && !a.interactive() {
				return usageErrorf("uninstall needs confirmation; pass --yes to proceed without asking")
			}
			question := "Uninstall tunlr (mihomo stops; settings and subscriptions are kept)?"
			if purge {
				question = "Uninstall tunlr and delete all settings, subscriptions and history?"
			}
			if ok, err := a.confirm(cmd.Context(), question); err != nil || !ok {
				if err == nil {
					fmt.Fprintln(a.stdout, "Not uninstalled.")
				}
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
			if a.opts.DryRun {
				return usageErrorf("upgrade core does not support --dry-run")
			}
			if version != "" {
				v, err := mihomo.NormalizeVersion(version)
				if err != nil {
					return err
				}
				version = v
			}
			k, err := a.kernels()
			if err != nil {
				return err
			}
			var res *mihomo.UpgradeResult
			if rollback {
				res, err = k.Rollback(cmd.Context())
			} else {
				res, err = k.Upgrade(cmd.Context(), version)
			}
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

// newUpdateCommand 是 tunlr-update.service 每天执行的内部命令。
func (a *app) newUpdateCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "_update",
		Hidden: true,
		Args:   usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := a.installer().Update(cmd.Context())
			if res == nil {
				return err
			}
			rerr := a.render(res, func() error {
				for _, s := range res.Steps {
					fmt.Fprintf(a.stdout, "%-8s %-13s %s\n", s.Status, s.Name, s.Message)
				}
				return nil
			})
			if err != nil {
				return renderedError{err}
			}
			return rerr
		},
	}
}
