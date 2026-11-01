package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// gatewayView 是 tun gateway on 在 --json 输出中的形式。
type gatewayView struct {
	Plan    *setup.GatewayPlan   `json:"plan"`
	Applied bool                 `json:"applied"`
	Result  *setup.GatewayResult `json:"result,omitempty"`
}

func (a *app) newGatewayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Show gateway mode; turn it on or off (LAN devices route through mihomo)",
		// 全称 tunlr tun gateway，tunlr tun 也显示网关模式的状态。
		Long: "Gateway mode lets devices on the LAN use this server as their default gateway and DNS,\n" +
			"so their traffic goes through mihomo (a \"side router\"). It needs the transparent proxy\n" +
			"(TUN). When mihomo is not running, LAN devices lose internet access instead of going\n" +
			"out directly. IPv4 only.\n\n" +
			"Without a subcommand, show whether gateway mode is on and whether each part is in place.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.installer().GatewayStatus(cmd.Context())
			if err != nil {
				return err
			}
			return a.render(s, func() error { a.printGatewayStatus(s); return nil })
		},
	}
	cmd.AddCommand(a.withFullDiff(a.newGatewayOnCommand()), a.newGatewayOffCommand())
	return cmd
}

func (a *app) printGatewayStatus(s *setup.GatewayStatus) {
	w := a.stdout
	if !s.On {
		fmt.Fprintf(w, "Gateway mode:      %s\n", a.style.dim("off"))
		if s.Tun != state.TunOn {
			fmt.Fprintln(w, "                   (it needs the transparent proxy: tunlr tun on)")
		}
		return
	}
	fmt.Fprintf(w, "Gateway mode:      %s (LAN interface %s)\n", a.style.good("on"), s.Interface)
	if len(s.Addresses) > 0 {
		fmt.Fprintf(w, "LAN devices use:   gateway and DNS %s\n", strings.Join(s.Addresses, " or "))
	}
	mark := func(ok bool, good, bad string) string {
		if ok {
			return good
		}
		return a.style.bad(bad)
	}
	if s.Tun != state.TunOn {
		fmt.Fprintf(w, "Transparent proxy: %s\n", a.style.bad(s.Tun+" (gateway mode needs it on)"))
	}
	fmt.Fprintf(w, "IPv4 forwarding:   %s\n", mark(s.Forward, "on", "off"))
	fmt.Fprintf(w, "sysctl file:       %s\n", mark(s.Sysctl, "present", "absent"))
	if r := s.Rules; r != nil {
		fmt.Fprintf(w, "nftables:          inet %s %s\n", netfilter.GatewayTable, mark(r.Table, "present", "absent"))
		switch {
		case !r.DockerChain:
			fmt.Fprintf(w, "Docker:            %s\n", a.style.dim("no "+netfilter.DockerChain+" chain"))
		default:
			fmt.Fprintf(w, "Docker:            %s accept rules %s\n", netfilter.DockerChain, mark(r.DockerRules, "present", "absent"))
		}
	}
	if r := s.Rules; r != nil && r.Table {
		window := fmt.Sprintf("in the last %d minutes", int(netfilter.GatewayWindow.Minutes()))
		switch {
		case len(s.Recent) == 0:
			fmt.Fprintf(w, "LAN traffic:       %s\n", a.style.dim("no device "+window))
		case len(s.Blocks) == 0:
			fmt.Fprintf(w, "LAN traffic:       %s %s, nothing dropped\n", plural(len(s.Recent), "device", "devices"), window)
		default:
			fmt.Fprintf(w, "LAN traffic:       %s %s\n", plural(len(s.Recent), "device", "devices"), window)
		}
		for _, b := range s.Blocks {
			fmt.Fprintf(w, "                   %s\n", a.style.bad(b.Describe()))
		}
	}
	if s.Tun != state.TunOn || !s.Forward || !s.Sysctl || s.Rules == nil || !s.Rules.Complete() {
		fmt.Fprintf(w, "\n%s tunlr tun gateway on --interface %s restores the missing parts.\n", a.style.warn("Hint:"), s.Interface)
	}
	if len(s.Blocks) > 0 {
		fmt.Fprintf(w, "\n%s to let it through: %s\n", a.style.warn("Hint:"), strings.Join(setup.GatewayFixes(s.Blocks), "; "))
	}
}

func (a *app) newGatewayOnCommand() *cobra.Command {
	var iface string
	cmd := &cobra.Command{
		Use:   "on",
		Short: "Turn gateway mode on (turns the transparent proxy on first if needed)",
		Long: "Turn IPv4 forwarding on (also after reboots), and load an nftables table that drops LAN\n" +
			"traffic not going through TUN, so LAN devices lose access rather than bypass mihomo when\n" +
			"it stops. If Docker's DOCKER-USER chain exists, accept rules are added there, because\n" +
			"Docker drops forwarded traffic by default.\n\n" +
			"Without --interface, a terminal lets you choose the LAN interface when there are several;\n" +
			"otherwise (and with --yes) it is the one with the IPv4 default route. If the transparent\n" +
			"proxy is off, it is turned on first, with its checks and rollback.\n\n" +
			"Running it again restores missing parts on the same interface (or switches to another\n" +
			"--interface).",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.requireConfirmation("tun gateway on"); err != nil {
				return err
			}
			if iface == "" {
				var err error
				if iface, err = a.askGatewayInterface(ctx); err != nil {
					_, err = a.cancelled(err)
					return err
				}
			}
			return a.gatewayOn(ctx, iface)
		},
	}
	cmd.Flags().StringVar(&iface, "interface", "", "the LAN interface (default: the one with the IPv4 default route)")
	return cmd
}

// askGatewayInterface 在交互模式下、网关模式还没有开启且有多个候选时让用户选局域网网卡；其余情况
// （包括 --yes）返回空，由 GatewayPreflight 取默认路由所在的网卡或沿用正在使用的网卡。
func (a *app) askGatewayInterface(ctx context.Context) (string, error) {
	if !a.interactive() || a.opts.Yes {
		return "", nil
	}
	inst := a.installer()
	s, err := inst.GatewayStatus(ctx)
	if err != nil || s.On {
		return "", err
	}
	cands, err := inst.LANInterfaces(ctx)
	if err != nil || len(cands) < 2 {
		return "", err
	}
	c, err := a.chooseInterface(ctx, cands)
	if err != nil {
		return "", err
	}
	return c.Name, nil
}

// chooseInterface 让用户从局域网网卡的候选中选一个（见 setup.LANInterfaces，默认路由所在的排在最前），
// 列表中标出有公网地址的网卡和默认路由所在的网卡。
func (a *app) chooseInterface(ctx context.Context, cands []setup.LANInterface) (*setup.LANInterface, error) {
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
		return nil, err
	}
	return &cands[i], nil
}

// gatewayOn 是 tun gateway on 的流程：前置检查、TUN 关闭时先开启、预览、确认、开启。iface 为空时用默认路由
// 所在的网卡。init 选择局域网网关时也走它。
func (a *app) gatewayOn(ctx context.Context, iface string) error {
	inst := a.installer()
	plan, err := inst.GatewayPreflight(ctx, iface)
	if err != nil {
		return err
	}
	a.printWarnings(plan.Warnings)

	ts, err := inst.TunStatus(ctx)
	if err != nil {
		return err
	}
	if ts.Switch != state.TunOn {
		if ok, err := a.gatewayTunOn(ctx, plan.Warnings); !ok || err != nil {
			return err
		}
	}

	v := gatewayView{Plan: plan}
	if plan.InPlace() {
		return a.render(v, func() error {
			_, err := fmt.Fprintf(a.stdout, "Gateway mode is already on (%s).\n", plan.Interface)
			return err
		})
	}
	if !a.opts.JSON {
		a.printGatewayPlan(plan)
	}
	if a.opts.DryRun {
		return a.render(v, func() error {
			_, err := fmt.Fprintln(a.stdout, "\nDry run: nothing changed.")
			return err
		})
	}
	if ok, err := a.ask(ctx, "Turn gateway mode on?", "", "Gateway mode not turned on."); !ok {
		return err
	}
	var res *setup.GatewayResult
	err = a.spin("Turning gateway mode on", func() (err error) {
		res, err = inst.GatewayOn(ctx, plan.Interface)
		return err
	})
	if err != nil {
		return err
	}
	v.Applied, v.Result = true, res
	return a.render(v, func() error {
		fmt.Fprintf(a.stdout, "\n%s\n", a.style.good("Gateway mode is on."))
		addrs := strings.Join(res.Addresses, " or ")
		fmt.Fprintf(a.stdout, "On LAN devices, set the gateway and DNS server to %s\n", addrs)
		fmt.Fprintln(a.stdout, "(by hand, or in the DHCP settings of your router).")
		fmt.Fprintln(a.stdout, "The DNS server must be this server too, not the router: names asked elsewhere bypass mihomo,")
		fmt.Fprintln(a.stdout, "so the ISP can see them (tunlr status reports devices that ask no DNS here).")
		return nil
	})
}

// gatewayTunOn 在 TUN 关闭时先走 tun on 的完整流程。ok 为 false 时不再继续开启网关（用户没有同意、
// TUN 没有开启、或只是预览）；预览时 ok 为 true，接着预览网关的改动。
func (a *app) gatewayTunOn(ctx context.Context, warnings []string) (ok bool, err error) {
	if a.opts.JSON {
		// 两份 JSON 输出没法合并：要求先单独开启 TUN。
		return false, fmt.Errorf("%w: gateway mode needs the transparent proxy; turn it on first: tunlr tun on", core.ErrCheckFailed)
	}
	fmt.Fprintf(a.stdout, "Gateway mode needs the transparent proxy (TUN), which is off; turning it on first.\n\n")
	if err := a.tunOn(ctx, warnings); err != nil {
		return false, err
	}
	if a.opts.DryRun {
		fmt.Fprintln(a.stdout)
		return true, nil
	}
	ts, err := a.installer().TunStatus(ctx)
	if err != nil {
		return false, err
	}
	if ts.Switch != state.TunOn {
		fmt.Fprintln(a.stdout, "Gateway mode not turned on.")
		return false, nil
	}
	fmt.Fprintln(a.stdout)
	return true, nil
}

func (a *app) printGatewayPlan(p *setup.GatewayPlan) {
	w := a.stdout
	if c := p.Current; c != nil && c.Interface != p.Interface {
		fmt.Fprintf(w, "Gateway mode is on %s; switching it to %s.\n", c.Interface, p.Interface)
	}
	fmt.Fprintf(w, "LAN interface %s (%s). Gateway mode will:\n", p.Interface, strings.Join(p.Addresses, ", "))
	if p.Forward == "1" {
		fmt.Fprintln(w, "  - keep IPv4 forwarding on, also after reboots (/etc/sysctl.d/90-tunlr-gateway.conf)")
	} else {
		fmt.Fprintln(w, "  - turn IPv4 forwarding on, also after reboots (/etc/sysctl.d/90-tunlr-gateway.conf)")
	}
	fmt.Fprintf(w, "  - load the nftables table inet %s: traffic from %s that does not go through %s\n"+
		"    is dropped, except to private addresses, so LAN devices lose access when mihomo stops\n",
		netfilter.GatewayTable, p.Interface, netfilter.Device)
	if p.Docker {
		fmt.Fprintf(w, "  - accept forwarding between %s and %s in Docker's %s chain (iptables)\n",
			p.Interface, netfilter.Device, netfilter.DockerChain)
	}
	fmt.Fprintln(w, "This server's own traffic and SSH are not affected. tunlr tun gateway off undoes it all.")
	fmt.Fprintln(w)
}

func (a *app) newGatewayOffCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Turn gateway mode off (the transparent proxy stays on)",
		Long: "Remove the nftables table, the DOCKER-USER rules and the sysctl file. IPv4 forwarding is\n" +
			"turned off again if it was off before and Docker (which needs it) is not present.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.requireConfirmation("tun gateway off"); err != nil {
				return err
			}
			inst := a.installer()
			s, err := inst.GatewayStatus(ctx)
			if err != nil {
				return err
			}
			result := map[string]bool{"turned_off": false}
			if !s.On {
				return a.render(result, func() error {
					_, err := fmt.Fprintln(a.stdout, "Gateway mode is already off.")
					return err
				})
			}
			if !a.opts.JSON {
				fmt.Fprintf(a.stdout, "Gateway mode is on %s. Devices using this server as their gateway lose internet access.\n\n", s.Interface)
			}
			if a.opts.DryRun {
				return a.render(result, func() error {
					_, err := fmt.Fprintln(a.stdout, "Dry run: nothing changed.")
					return err
				})
			}
			if ok, err := a.ask(ctx, "Turn gateway mode off?", "", "Gateway mode not turned off."); !ok {
				return err
			}
			err = a.spin("Turning gateway mode off", func() (err error) {
				result["turned_off"], err = inst.GatewayOff(ctx)
				return err
			})
			if err != nil {
				return err
			}
			return a.render(result, func() error {
				_, err := fmt.Fprintf(a.stdout, "\n%s\n", a.style.good("Gateway mode is off; the transparent proxy stays on."))
				return err
			})
		},
	}
}
