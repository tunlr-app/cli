package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// tunView 是 tun on/off 在 --json 输出中的形式。
type tunView struct {
	Plan     planView         `json:"plan"`
	Warnings []string         `json:"warnings,omitempty"`
	Applied  bool             `json:"applied"`
	Result   *setup.TunResult `json:"result,omitempty"`
}

func (a *app) newTunCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tun",
		Short: "Show the transparent proxy (TUN) status; turn it on or off",
		Long: "Without a subcommand, show whether the transparent proxy is on, what mihomo actually\n" +
			"runs (the panel can switch TUN temporarily) and whether the network rules are in place.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.installer().TunStatus(cmd.Context())
			if err != nil {
				return err
			}
			return a.render(s, func() error { a.printTunStatus(s); return nil })
		},
	}
	cmd.AddCommand(a.newTunOnCommand(), a.newTunOffCommand())
	return cmd
}

func (a *app) printTunStatus(s *setup.TunStatus) {
	w := a.stdout
	switch s.Switch {
	case state.TunPending:
		left := time.Until(s.Deadline).Round(time.Second)
		if left > 0 {
			fmt.Fprintf(w, "Transparent proxy: %s (turned off in %s unless confirmed)\n", a.style.warn("being checked"), left)
		} else {
			fmt.Fprintf(w, "Transparent proxy: %s (it is turned off when mihomo starts or tunlr-check runs)\n", a.style.warn("never confirmed"))
		}
	case state.TunOn:
		fmt.Fprintf(w, "Transparent proxy: %s\n", a.style.good("on"))
	default:
		fmt.Fprintf(w, "Transparent proxy: %s\n", a.style.dim("off"))
	}
	switch {
	case s.Running == nil:
		fmt.Fprintf(w, "mihomo:            %s\n", a.style.warn("not responding"))
	case *s.Running:
		fmt.Fprintln(w, "mihomo:            TUN enabled")
	default:
		fmt.Fprintln(w, "mihomo:            TUN disabled")
	}
	if s.Running != nil && *s.Running != (s.Switch != "off") {
		fmt.Fprintln(w, "                   (switched in the panel; tunlr tun on/off sets it back to the switch)")
	}
	yes := func(b bool) string {
		if b {
			return "present"
		}
		return a.style.dim("absent")
	}
	fmt.Fprintf(w, "Interface:         %s %s\n", netfilter.Device, yes(s.Device))
	fmt.Fprintf(w, "nftables:          inet %s %s, inet %s %s\n", netfilter.Table, yes(s.Rules), netfilter.KernelTable, yes(s.KernelRules))
}

func (a *app) newTunOnCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "on",
		Short: "Turn the transparent proxy (TUN) on, with automatic rollback",
		Long: "Publish the configuration with TUN enabled (shown as a diff first), then check that\n" +
			"DNS, HTTP and the SSH session you are running in still work. If a check fails, TUN is\n" +
			"turned off again (exit code 4). If tunlr is cut off before the checks finish, a\n" +
			"systemd timer turns TUN off after 3 minutes, and so does a reboot.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.requireConfirmation("tun on"); err != nil {
				return err
			}
			inst := a.installer()
			warnings, err := inst.TunPreflight(ctx)
			if err != nil {
				return err
			}
			if !a.opts.JSON {
				for _, w := range warnings {
					fmt.Fprintf(a.stdout, "%s %s\n", a.style.warn("Warning:"), w)
				}
			}
			s, err := inst.TunStatus(ctx)
			if err != nil {
				return err
			}
			plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().PlanTun(ctx, true) })
			if err != nil {
				return err
			}
			v := tunView{Plan: newPlanView(plan, false), Warnings: warnings}
			if s.Switch == state.TunOn && !plan.Changed() {
				return a.render(v, func() error {
					_, err := fmt.Fprintln(a.stdout, "The transparent proxy is already on.")
					return err
				})
			}
			if !a.opts.JSON {
				a.printPlan(v.Plan)
			}
			if err := checkExit(plan, false); err != nil || a.opts.DryRun {
				if a.opts.JSON {
					if rerr := a.render(v, nil); rerr != nil {
						return rerr
					}
				} else if err != nil {
					fmt.Fprintln(a.stdout, "\nTUN not turned on.")
				} else {
					fmt.Fprintln(a.stdout, "\nDry run: nothing changed.")
				}
				return err
			}
			if ok, err := a.confirm(ctx, "Turn the transparent proxy (TUN) on?", ""); err != nil || !ok {
				if err == nil {
					fmt.Fprintln(a.stdout, "TUN not turned on.")
				}
				return err
			}

			opts := a.sshProbe()
			if !a.opts.JSON {
				fmt.Fprintln(a.stdout)
			}
			var done func(error)
			opts.Progress, done = a.stepper()
			res, err := inst.TunOn(ctx, plan, opts)
			// 检测失败时 TunOn 已经关闭了 TUN，最后一步（关闭）本身是成功的，失败的项在下面列出。
			if res == nil {
				done(err)
			} else {
				done(nil)
			}
			v.Applied, v.Result = res != nil && err == nil, res
			if res == nil {
				return err
			}
			rerr := a.render(v, func() error {
				for _, c := range res.Checks {
					a.printCheck(c.Status, c.Name, 6, c.Message)
				}
				if err == nil {
					fmt.Fprintf(a.stdout, "\n%s\n", a.style.good("The transparent proxy is on."))
				}
				return nil
			})
			if err != nil {
				if a.opts.JSON {
					return renderedError{err}
				}
				return err
			}
			return rerr
		},
	}
}

// sshProbe 给出 SSH 检测需要的参数：$SSH_CONNECTION，以及向用户的终端输出文字的 Probe。
// 标准错误不是终端时无从检测；在 tmux、screen 中时输出不一定经过 $SSH_CONNECTION 那条连接，跳过。
func (a *app) sshProbe() setup.TunOptions {
	opts := setup.TunOptions{SSHConnection: os.Getenv("SSH_CONNECTION")}
	if opts.SSHConnection == "" {
		return opts
	}
	switch {
	case os.Getenv("TMUX") != "":
		opts.SkipSSH = "running inside tmux"
	case os.Getenv("STY") != "":
		opts.SkipSSH = "running inside screen"
	}
	f, ok := a.stderr.(*os.File)
	if !ok || !term.IsTerminal(f.Fd()) {
		return opts
	}
	opts.Probe = func() {
		// 进行中的步骤显示着 spinner 时经由它输出，否则直接写到终端。
		var w io.Writer = f
		if p := a.live.Load(); p != nil && a.style.on {
			w = p
		}
		fmt.Fprintln(w, a.style.dim("Checking that this SSH session still gets through (no input needed)"))
	}
	return opts
}

func (a *app) newTunOffCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Turn the transparent proxy (TUN) off",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.requireConfirmation("tun off"); err != nil {
				return err
			}
			inst := a.installer()
			s, err := inst.TunStatus(ctx)
			if err != nil {
				return err
			}
			plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().PlanTun(ctx, false) })
			if err != nil {
				return err
			}
			v := tunView{Plan: newPlanView(plan, false)}
			// 还没有发布过配置时也不必发布。
			if s.Switch == "off" && (!plan.Changed() || plan.Current == nil) {
				return a.render(v, func() error {
					_, err := fmt.Fprintln(a.stdout, "The transparent proxy is already off.")
					return err
				})
			}
			if !a.opts.JSON {
				a.printPlan(v.Plan)
			}
			if err := checkExit(plan, true); err != nil || a.opts.DryRun {
				if a.opts.JSON {
					if rerr := a.render(v, nil); rerr != nil {
						return rerr
					}
				} else if err != nil {
					fmt.Fprintln(a.stdout, "\nTUN not turned off.")
				} else {
					fmt.Fprintln(a.stdout, "\nDry run: nothing changed.")
				}
				return err
			}
			if ok, err := a.confirm(ctx, "Turn the transparent proxy (TUN) off?", ""); err != nil || !ok {
				if err == nil {
					fmt.Fprintln(a.stdout, "TUN not turned off.")
				}
				return err
			}
			var res *setup.TunResult
			err = a.spin("Publishing the configuration", func() (err error) {
				res, err = inst.TunOff(ctx, plan)
				return err
			})
			if errors.Is(err, core.ErrRolledBack) {
				return fmt.Errorf("%w; the previous configuration is back in place", err)
			}
			if err != nil {
				return err
			}
			v.Applied, v.Result = true, res
			return a.render(v, func() error {
				_, err := fmt.Fprintf(a.stdout, "\n%s\n", a.style.good("The transparent proxy is off."))
				return err
			})
		},
	}
}

// newPreStartCommand 是 tunlr-core.service 启动内核前（ExecStartPre，以 root）执行的内部命令。
func (a *app) newPreStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "_prestart",
		Hidden: true,
		Args:   usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.installer().PreStart(cmd.Context())
		},
	}
}

// newPostStopCommand 是内核退出后（ExecStopPost，以 root）执行的内部命令。
func (a *app) newPostStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "_poststop",
		Hidden: true,
		Args:   usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.installer().PostStop(cmd.Context())
		},
	}
}

// newTunGuardCommand 是回滚定时器（tunlr-tun-guard）执行的内部命令。
func (a *app) newTunGuardCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "_tun-guard",
		Hidden: true,
		Args:   usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			reverted, err := a.installer().TunGuard(cmd.Context())
			if err != nil {
				return err
			}
			return a.render(map[string]bool{"reverted": reverted}, func() error {
				msg := "TUN was confirmed or turned off already; nothing to do."
				if reverted {
					msg = "Turned TUN off: the checks did not pass in time."
				}
				_, err := fmt.Fprintln(a.stdout, msg)
				return err
			})
		},
	}
}

// newCheckCommand 是 tunlr-check.timer 每 5 分钟执行的内部命令。
func (a *app) newCheckCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "_check",
		Hidden: true,
		Args:   usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := a.installer().Check(cmd.Context())
			if res == nil {
				return err
			}
			rerr := a.render(res, func() error {
				if len(res.Actions) == 0 && err == nil {
					fmt.Fprintln(a.stdout, "Everything is in place.")
				}
				for _, act := range res.Actions {
					fmt.Fprintf(a.stdout, "Fixed: %s\n", act)
				}
				return nil
			})
			if err != nil {
				return err
			}
			return rerr
		},
	}
}

// describeTun 用于 status 的一行透明代理状态。
func (a *app) describeTun(s *setup.TunStatus) string {
	var b strings.Builder
	switch s.Switch {
	case state.TunPending:
		b.WriteString(a.style.warn("being checked"))
	case state.TunOn:
		b.WriteString(a.style.good("on"))
	default:
		b.WriteString(a.style.dim("off"))
	}
	if s.Running != nil && *s.Running != (s.Switch != "off") {
		if *s.Running {
			b.WriteString(" (mihomo runs with TUN enabled: switched in the panel)")
		} else {
			b.WriteString(" (mihomo runs with TUN disabled: switched in the panel)")
		}
	}
	return b.String()
}
