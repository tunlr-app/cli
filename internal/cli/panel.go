package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
)

// panelLANView 是 panel lan on/off 在 --json 输出中的形式。
type panelLANView struct {
	Plan      planView              `json:"plan"`
	Preflight *setup.PanelLANPlan   `json:"preflight,omitempty"`
	Applied   bool                  `json:"applied"`
	Result    *setup.PanelLANResult `json:"result,omitempty"`
}

func (a *app) newPanelCommand() *cobra.Command {
	var reset bool
	cmd := &cobra.Command{
		Use:   "panel",
		Short: "Show how to open the panel (SSH tunnel and link); --reset-secret replaces the API secret",
		Long: "The panel (zashboard) talks to mihomo's API, which listens on 127.0.0.1 only. Show the SSH\n" +
			"command that forwards it to your computer, and the link that opens the panel already\n" +
			"connected (it carries the API secret). Run the SSH command on your computer, not here.\n" +
			"With LAN access on (tunlr panel lan on), also show the links for devices on the LAN.\n\n" +
			"--reset-secret publishes the configuration with a new random API secret; mihomo restarts,\n" +
			"and panels opened with the old link have to connect again with the new one.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if reset {
				return a.resetSecret(cmd)
			}
			if a.opts.DryRun {
				return usageErrorf("panel changes nothing; --dry-run only applies to --reset-secret")
			}
			p, err := a.panel(cmd.Context())
			if err != nil {
				return err
			}
			return a.render(p, func() error { a.printPanel(p); return nil })
		},
	}
	cmd.Flags().BoolVar(&reset, "reset-secret", false, "publish the configuration with a new API secret (mihomo restarts)")
	cmd.AddCommand(a.newPanelLANCommand())
	return cmd
}

func (a *app) panel(ctx context.Context) (*setup.Panel, error) {
	return a.installer().Panel(ctx, setup.PanelOptions{
		SSHConnection: os.Getenv("SSH_CONNECTION"),
		User:          cmp.Or(os.Getenv("SUDO_USER"), os.Getenv("USER")),
	})
}

func (a *app) printPanel(p *setup.Panel) {
	w := a.stdout
	if p.Running == "" {
		fmt.Fprintf(w, "%s mihomo is not running; start it with: tunlr start\n\n", a.style.warn("Note:"))
	}
	if p.UIDir != "" && !p.UIInstalled {
		fmt.Fprintf(w, "%s the panel is not installed in %s yet; tunlr apply installs it.\n\n", a.style.warn("Note:"), p.UIDir)
	}
	if p.Public {
		a.printPanelLAN(p)
		fmt.Fprintln(w, "\nFrom elsewhere, through an SSH tunnel:")
	} else {
		fmt.Fprintf(w, "The API listens on %s, only reachable from this server", p.Controller)
		if !p.LAN {
			fmt.Fprint(w, " (LAN devices: tunlr panel lan on)")
		}
		fmt.Fprintln(w, ".")
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "1. On your computer, keep this SSH tunnel open:")
	fmt.Fprintf(w, "     %s\n", p.SSHCommand)
	if p.SSHHostGuessed {
		fmt.Fprintln(w, "   (the address is the one this SSH session came in on; use the host name or alias you")
		fmt.Fprintln(w, "   normally log in with if that differs)")
	} else {
		fmt.Fprintln(w, "   (replace <server> with how you log in to this server)")
	}
	fmt.Fprintln(w, "   Or add it to the host's entry in ~/.ssh/config, so every login opens the tunnel:")
	fmt.Fprintf(w, "     %s\n\n", p.LocalForward)
	fmt.Fprintln(w, "2. Then open in your browser:")
	fmt.Fprintf(w, "     %s\n", p.URL)
	fmt.Fprintf(w, "   The browser remembers it as %q; later just open %s\n", p.Label+" (SSH tunnel)", p.Bookmark)
	if p.Secret != "" {
		fmt.Fprintln(w, "\nThe links carry the API secret; keep them private (tunlr panel --reset-secret replaces it).")
	}
}

// printPanelLAN 输出局域网设备直接打开面板的链接，以及与开关不一致的情况。
func (a *app) printPanelLAN(p *setup.Panel) {
	w := a.stdout
	switch {
	case p.LAN && !p.LANRules:
		fmt.Fprintf(w, "%s the nftables table inet %s is missing, so the panel accepts connections from any\n"+
			"address; tunlr panel lan on restores it.\n\n", a.style.bad("Warning:"), netfilter.PanelTable)
	case !p.LAN:
		fmt.Fprintf(w, "%s the API listens on %s, reachable from the network (edited outside tunlr?);\n"+
			"anyone who can reach it and knows the secret controls mihomo.\n\n", a.style.warn("Warning:"), p.Controller)
	}
	fmt.Fprintln(w, "From devices on the LAN, open:")
	for _, u := range p.LANURLs {
		fmt.Fprintf(w, "  %s\n", u)
	}
	fmt.Fprintf(w, "The browser remembers it as %q; later just open %s\n", p.Label, strings.Join(p.LANBookmarks, " or "))
}

// describePanelLAN 用于 status 和菜单的面板访问状态。
func (a *app) describePanelLAN(on bool) string {
	if on {
		return a.style.good("LAN access on")
	}
	return a.style.dim("this server only")
}

// resetSecret 经流水线换一个新的 API secret 并发布，成功后输出新的面板链接。
func (a *app) resetSecret(cmd *cobra.Command) error {
	if err := a.requireConfirmation("panel --reset-secret"); err != nil {
		return err
	}
	plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().PlanResetSecret(cmd.Context()) })
	if err != nil {
		return err
	}
	if err := a.publish(cmd, plan, "Replace the API secret?"); err != nil {
		return err
	}
	// --json 只输出发布结果，避免两份 JSON；新的链接用 tunlr panel --json 取。
	if a.opts.JSON || a.opts.DryRun || a.aborted {
		return nil
	}
	p, err := a.panel(cmd.Context())
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "\nBrowsers that opened the old link have to connect again with the new one:")
	fmt.Fprintf(a.stdout, "  %s\n", p.URL)
	for _, u := range p.LANURLs {
		fmt.Fprintf(a.stdout, "  %s\n", u)
	}
	fmt.Fprintf(a.stdout, "zashboard keeps the old backend %q with the old secret beside the new one;\n"+
		"delete it in the panel's settings (Backend).\n", p.Label)
	return nil
}

func (a *app) newPanelLANCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lan",
		Short: "Show whether devices on the LAN can open the panel; turn it on or off",
		Long: "By default the panel is only reachable from this server (through an SSH tunnel). With LAN\n" +
			"access on, mihomo's API listens on all addresses, and an nftables table drops\n" +
			"connections to it from addresses that are not private (10/8, 172.16/12, 192.168/16,\n" +
			"100.64/10 including Tailscale, 169.254/16; IPv6 link-local and fc00::/7), so phones\n" +
			"and tablets at home can open it.\n" +
			"The API secret still protects it, but travels unencrypted over the LAN.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.installer().Status(cmd.Context())
			if err != nil {
				return err
			}
			result := map[string]bool{"on": s.PanelLAN}
			return a.render(result, func() error {
				fmt.Fprintf(a.stdout, "Panel LAN access: %s\n", a.describePanelLAN(s.PanelLAN))
				if s.PanelLAN {
					fmt.Fprintln(a.stdout, "(tunlr panel shows the links; tunlr panel lan off turns it off)")
				} else {
					fmt.Fprintln(a.stdout, "(tunlr panel lan on lets devices on the LAN open it)")
				}
				return nil
			})
		},
	}
	cmd.AddCommand(a.withFullDiff(a.newPanelLANOnCommand()), a.withFullDiff(a.newPanelLANOffCommand()))
	return cmd
}

func (a *app) newPanelLANOnCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "on",
		Short: "Let devices on the LAN open the panel (mihomo restarts)",
		Long: "Load the nftables table that only lets private addresses reach the API port, then publish\n" +
			"the configuration with external-controller: " + pipeline.LANController + " (shown as a diff first);\n" +
			"mihomo restarts. If this server has a public address, you are asked twice.\n" +
			"Running it again restores a missing nftables table.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireConfirmation("panel lan on"); err != nil {
				return err
			}
			return a.panelLANOn(cmd.Context())
		},
	}
}

// panelLANOn 是 panel lan on 的流程：前置检查、预览、确认、发布。init 选择让局域网设备打开面板时也走它。
func (a *app) panelLANOn(ctx context.Context) error {
	inst := a.installer()
	pre, err := inst.PanelLANPreflight(ctx)
	if err != nil {
		return err
	}
	plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().PlanPanelLAN(ctx, true) })
	if err != nil {
		return err
	}
	v := panelLANView{Plan: newPlanView(plan, false), Preflight: pre}
	if pre.On && !plan.Changed() {
		// 配置已经是开启的样子：只补上可能缺少的表，不需要确认。
		if !a.opts.DryRun {
			if v.Result, err = inst.PanelLANOn(ctx, plan); err != nil {
				return err
			}
		}
		return a.render(v, func() error {
			_, err := fmt.Fprintln(a.stdout, "LAN access to the panel is already on (tunlr panel shows the links).")
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
			fmt.Fprintln(a.stdout, "\nLAN access not turned on.")
		} else {
			fmt.Fprintln(a.stdout, "\nDry run: nothing changed.")
		}
		return err
	}
	if !a.opts.JSON {
		fmt.Fprintf(a.stdout, "\nDevices with a private address can open the panel at port %d; others are dropped by the\n"+
			"nftables table inet %s. The API secret travels unencrypted over the LAN.\n", pipeline.APIPort, netfilter.PanelTable)
	}
	if len(pre.PublicAddresses) > 0 {
		if !a.opts.JSON {
			fmt.Fprintf(a.stdout, "\n%s %s has the public address %s: this server is probably reachable from the\n"+
				"internet. Only private addresses can reach the panel, but if the server is not on a LAN you\n"+
				"use, an SSH tunnel (tunlr panel) is the safer way.\n",
				a.style.warn("Warning:"), pre.Interface, strings.Join(pre.PublicAddresses, ", "))
		}
		if ok, err := a.ask(ctx, "This server has a public address. Open the panel to the LAN anyway?", "", "LAN access not turned on."); !ok {
			return err
		}
	}
	if ok, err := a.ask(ctx, "Let devices on the LAN open the panel?", "mihomo restarts", "LAN access not turned on."); !ok {
		return err
	}
	err = a.spin("Publishing the configuration", func() (err error) {
		v.Result, err = inst.PanelLANOn(ctx, plan)
		return err
	})
	if errors.Is(err, core.ErrRolledBack) {
		return fmt.Errorf("%w; the previous configuration is back in place", err)
	}
	if err != nil {
		return err
	}
	v.Applied = true
	return a.render(v, func() error {
		fmt.Fprintf(a.stdout, "\n%s\n", a.style.good("LAN access to the panel is on."))
		if p, err := a.panel(ctx); err == nil {
			a.printPanelLAN(p)
		}
		fmt.Fprintln(a.stdout, "If a device cannot open it, check other firewalls on this server (they can still block the port).")
		return nil
	})
}

func (a *app) newPanelLANOffCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Let only this server reach the panel again (mihomo restarts)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.requireConfirmation("panel lan off"); err != nil {
				return err
			}
			inst := a.installer()
			s, err := inst.Status(ctx)
			if err != nil {
				return err
			}
			plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().PlanPanelLAN(ctx, false) })
			if err != nil {
				return err
			}
			v := panelLANView{Plan: newPlanView(plan, false)}
			if !s.PanelLAN && (!plan.Changed() || plan.Current == nil) {
				return a.render(v, func() error {
					_, err := fmt.Fprintln(a.stdout, "LAN access to the panel is already off.")
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
					fmt.Fprintln(a.stdout, "\nLAN access not turned off.")
				} else {
					fmt.Fprintln(a.stdout, "\nDry run: nothing changed.")
				}
				return err
			}
			if ok, err := a.ask(ctx, "Stop LAN access to the panel?", "mihomo restarts; panels on other devices disconnect", "LAN access not turned off."); !ok {
				return err
			}
			err = a.spin("Publishing the configuration", func() (err error) {
				v.Result, err = inst.PanelLANOff(ctx, plan)
				return err
			})
			if errors.Is(err, core.ErrRolledBack) {
				return fmt.Errorf("%w; the previous configuration is back in place", err)
			}
			if err != nil {
				return err
			}
			v.Applied = true
			return a.render(v, func() error {
				_, err := fmt.Fprintf(a.stdout, "\n%s Open it through an SSH tunnel: tunlr panel\n",
					a.style.good("LAN access to the panel is off."))
				return err
			})
		},
	}
}
