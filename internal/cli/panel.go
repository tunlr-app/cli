package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// panelLANView 是 panel lan on/off 在 --json 输出中的形式。Access 是开启时保存的放行入口。
type panelLANView struct {
	Plan      planView              `json:"plan"`
	Preflight *setup.PanelLANPlan   `json:"preflight,omitempty"`
	Access    *state.LANAccess      `json:"access,omitempty"`
	Applied   bool                  `json:"applied"`
	Result    *setup.PanelLANResult `json:"result,omitempty"`
}

func (a *app) newPanelCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "panel",
		Short: "Show how to open the panel (SSH tunnel and link); lan, reset-secret",
		Long: "The panel (zashboard) talks to mihomo's API, which listens on 127.0.0.1 only. Show the SSH\n" +
			"command that forwards it to your computer, and the link that opens the panel already\n" +
			"connected (it carries the API secret). Run the SSH command on your computer, not here.\n" +
			"With LAN access on (tunlr panel lan on), also show the links for devices on the LAN.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.panel(cmd.Context())
			if err != nil {
				return err
			}
			return a.render(p, func() error { a.printPanel(p); return nil })
		},
	}
	cmd.AddCommand(a.newPanelLANCommand(), a.withFullDiff(a.newPanelResetSecretCommand()))
	return cmd
}

func (a *app) newPanelResetSecretCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "reset-secret",
		Short: "Replace the API secret with a new random one (mihomo restarts)",
		Long: "Publish the configuration with a new random API secret (shown as a diff first, the secret\n" +
			"itself hidden); mihomo restarts, and panels opened with the old link have to connect again\n" +
			"with the new one. Use it when the secret may have leaked.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error { return a.resetSecret(cmd) },
	}
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
		fmt.Fprintf(w, "%s mihomo is not running; start it with: tunlr core start\n\n", a.style.warn("Note:"))
	}
	if p.UIDir != "" && !p.UIInstalled {
		fmt.Fprintf(w, "%s the panel is not in %s yet; mihomo downloads it from external-ui-url when it\nloads the configuration (through github-mirror if set; see: tunlr core logs).\n\n", a.style.warn("Note:"), p.UIDir)
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
		fmt.Fprintln(w, "\nThe links carry the API secret; keep them private (tunlr panel reset-secret replaces it).")
	}
}

// printPanelLAN 输出局域网设备直接打开面板的链接，以及与开关不一致的情况。
func (a *app) printPanelLAN(p *setup.Panel) {
	w := a.stdout
	switch {
	case p.LAN && !p.LANRules:
		fmt.Fprintf(w, "%s the nftables table inet %s is missing, so the panel accepts connections from any\n"+
			"address; tunlr panel lan on restores it.\n\n", a.style.bad("Warning:"), netfilter.AccessTable)
	case !p.LAN:
		fmt.Fprintf(w, "%s the API listens on %s, reachable from the network (edited outside tunlr?);\n"+
			"anyone who can reach it and knows the secret controls mihomo.\n\n", a.style.warn("Warning:"), p.Controller)
	}
	if p.Access != nil {
		a.printAccess(*p.Access, "panel")
	}
	if len(p.LANLinks) == 1 {
		fmt.Fprintln(w, "From devices on the LAN, open:")
		fmt.Fprintf(w, "  %s\n", p.LANLinks[0].URL)
		fmt.Fprintf(w, "The browser remembers it as %q; later just open %s\n", p.Label, p.LANLinks[0].Bookmark)
		return
	}
	fmt.Fprintln(w, "From other devices, open the link with this server's address on their network:")
	for _, l := range p.LANLinks {
		fmt.Fprintf(w, "  %s\n    %s\n", a.style.dim(lanNote(l.Interface, l.Default, l.Gateway, l.SSH)+":"), l.URL)
	}
	fmt.Fprintf(w, "The browser remembers it as %q; later just open http://<that address>:%d/ui/\n", p.Label, p.Port)
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
	if err := a.requireConfirmation("panel reset-secret"); err != nil {
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
	for _, l := range p.LANLinks {
		fmt.Fprintf(a.stdout, "  %s\n", l.URL)
	}
	fmt.Fprintf(a.stdout, "zashboard keeps the old backend %q with the old secret beside the new one;\n"+
		"delete it in the panel's settings (Backend).\n", p.Label)
	return nil
}

// privateRanges 列出 tunlr 视为局域网的地址段（netfilter.PrivateIPv4、PrivateIPv6），用于帮助文字。
func privateRanges() string {
	return strings.Join(slices.Concat(netfilter.PrivateIPv4, netfilter.PrivateIPv6), ", ")
}

// privateIncludes 说明私有地址段比家里的局域网宽：从放行的网卡进来的 VPN 设备同样放行。
const privateIncludes = "Private addresses also cover\ndevices on VPNs such as Tailscale (100.64.0.0/10) through an interface let in."

func (a *app) newPanelLANCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lan",
		Short: "Show whether devices on the LAN can open the panel; turn it on or off",
		Long: "By default the panel is only reachable from this server (through an SSH tunnel). With LAN\n" +
			"access on, mihomo's API listens on all addresses, and the nftables table inet " + netfilter.AccessTable + "\n" +
			"only lets in connections through the chosen interfaces from private addresses\n" +
			"(" + privateRanges() + "), so phones and tablets at home can\n" +
			"open it. " + privateIncludes + "\n" +
			"By default the interfaces without a public address are let in; choose them with tunlr panel\n" +
			"lan on --interface (shared with the proxy port). The API secret still protects the panel,\n" +
			"but travels unencrypted over the LAN.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.installer().Status(cmd.Context())
			if err != nil {
				return err
			}
			v := struct {
				On     bool             `json:"on"`
				Access *setup.LANAccess `json:"access,omitempty"`
			}{s.PanelLAN, s.LANAccess}
			return a.render(v, func() error {
				fmt.Fprintf(a.stdout, "Panel LAN access: %s\n", a.describePanelLAN(s.PanelLAN))
				if s.PanelLAN {
					a.printAccess(*s.LANAccess, "panel")
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
	cmd := &cobra.Command{
		Use:   "on",
		Short: "Let devices on the LAN open the panel (mihomo restarts)",
		Long: "Choose the interfaces to let in (interactively, or with --interface), load the nftables table\n" +
			"that only lets private addresses in through them, then publish the configuration with\n" +
			"external-controller: " + pipeline.LANController + " (shown as a diff first); mihomo restarts. If an\n" +
			"interface let in has a public address, you are asked twice. Running it again changes the\n" +
			"interfaces (shared with the proxy port) and restores a missing nftables table.",
		Args: usageArgs(cobra.NoArgs),
	}
	flags := addLANAccessFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := a.requireConfirmation("panel lan on"); err != nil {
			return err
		}
		return a.panelLANOn(cmd.Context(), flags.choice())
	}
	return cmd
}

// panelLANOn 是 panel lan on 的流程：前置检查、选择放行的入口、预览、确认、发布；已经开启时只更新放行的
// 入口。init 选择让局域网设备打开面板、还原备份时也走它。
func (a *app) panelLANOn(ctx context.Context, choice lanChoice) error {
	inst := a.installer()
	pre, err := inst.PanelLANPreflight(ctx, os.Getenv("SSH_CONNECTION"))
	if err != nil {
		return err
	}
	plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().PlanPanelLAN(ctx, true) })
	if err != nil {
		return err
	}
	v := panelLANView{Plan: newPlanView(plan, false), Preflight: pre}
	if v.Access, err = a.chooseLANAccess(ctx, pre.Interfaces, pre.Access, choice); v.Access == nil {
		return err
	}
	if pre.On && !plan.Changed() {
		// 配置已经是开启的样子：只更新放行的入口，补上可能缺少的表。
		changed, err := a.updateLANAccess(ctx, pre.Interfaces, pre.Access, v.Access, "the panel", "Interfaces not changed.")
		if err != nil || a.aborted {
			return err
		}
		v.Applied = changed && !a.opts.DryRun
		return a.render(v, func() error {
			fmt.Fprintln(a.stdout, "LAN access to the panel is already on.")
			if !v.Applied {
				a.printAccessUpdate(changed, pre.Access, v.Access, "panel")
				fmt.Fprintln(a.stdout, "(tunlr panel shows the links)")
			} else if p, err := a.panel(ctx); err == nil {
				fmt.Fprintf(a.stdout, "%s\n", a.style.good("Interfaces updated."))
				a.printPanelLAN(p)
			}
			return nil
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
		fmt.Fprintf(a.stdout, "\nDevices with a private address can open the panel at port %d, through: %s.\n"+
			"Other connections are dropped by the nftables table inet %s. %s\n"+
			"The API secret travels unencrypted over the LAN.\n",
			pipeline.APIPort, a.describeAccess(pre.Access.With(v.Access)), netfilter.AccessTable, privateIncludes)
	}
	declined := "LAN access not turned on."
	if ok, err := a.confirmPublic(ctx, newlyPublic(pre.Interfaces, pre.Access, pre.On, v.Access), "the panel", declined); !ok {
		return err
	}
	if ok, err := a.ask(ctx, "Let devices on the LAN open the panel?", "mihomo restarts", declined); !ok {
		return err
	}
	err = a.spin("Publishing the configuration", func() (err error) {
		v.Result, err = inst.PanelLANOn(ctx, plan, v.Access)
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
