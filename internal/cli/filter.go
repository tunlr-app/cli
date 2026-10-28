package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// filterEditView 是 filter quic/ports 在 --json 输出中的形式。
type filterEditView struct {
	Filter   state.Filter     `json:"filter"`
	Plan     *planView        `json:"plan,omitempty"`
	Applied  bool             `json:"applied"`
	Revision *config.Revision `json:"revision,omitempty"`
}

func (a *app) newFilterCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "filter",
		Short: "Show and set which traffic skips the subscription's rules (devices, QUIC, ports)",
		Long: "Filters are rules placed before all others (the managed mixin tunlr:filter), in this order:\n" +
			"  device   a LAN device set to direct goes direct for everything, unaffected by the rest\n" +
			"  source   so does traffic from an address range (e.g. a Docker network, a VPN subnet)\n" +
			"  quic     UDP port 443 is rejected, so browsers use TCP instead of QUIC (which many proxies\n" +
			"           carry poorly); sites that go direct fall back to TCP too\n" +
			"  ports    only connections to common ports are proxied; others (BT, P2P, games) go direct\n" +
			"All are off by default. A mixin can override the rules or remove them with delete-rules.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error { return a.showFilter() },
	}
	quic := &cobra.Command{
		Use:   "quic",
		Short: "Show whether QUIC is blocked; turn it on or off",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.showFilter() },
	}
	quic.AddCommand(a.withFullDiff(a.newFilterQUICCommand(true)), a.withFullDiff(a.newFilterQUICCommand(false)))
	ports := &cobra.Command{
		Use:   "ports",
		Short: "Show whether only common ports are proxied; turn it on or off, or change the ports",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.showFilter() },
	}
	ports.AddCommand(a.withFullDiff(a.newFilterPortsCommand(true)), a.withFullDiff(a.newFilterPortsCommand(false)),
		a.withFullDiff(a.newFilterPortsSetCommand()))
	cmd.AddCommand(a.withFullDiff(a.newFilterDeviceCommand()), a.newFilterSourceCommand(), quic, ports)
	return cmd
}

func (a *app) showFilter() error {
	s, err := a.installer().Filter()
	if err != nil {
		return err
	}
	return a.render(s, func() error {
		w := a.stdout
		if len(s.Direct) == 0 {
			fmt.Fprintf(w, "Direct devices:  %s (tunlr filter device <device> direct)\n", a.style.dim("none"))
		} else {
			names := make([]string, len(s.Direct))
			for i, d := range s.Direct {
				names[i] = d.IP
				if d.Name != "" {
					names[i] = fmt.Sprintf("%s (%s)", d.Name, d.IP)
				}
			}
			fmt.Fprintf(w, "Direct devices:  %s (tunlr filter device <device> proxy)\n", a.style.warn(strings.Join(names, ", ")))
		}
		if len(s.Sources) == 0 {
			fmt.Fprintf(w, "Direct sources:  %s (tunlr filter source add <network>)\n", a.style.dim("none"))
		} else {
			fmt.Fprintf(w, "Direct sources:  %s (tunlr filter source remove <network>)\n", a.style.warn(strings.Join(s.Sources, ", ")))
		}
		if s.QUIC {
			fmt.Fprintf(w, "QUIC:            %s (tunlr filter quic off)\n", a.style.good("blocked, browsers use TCP"))
		} else {
			fmt.Fprintf(w, "QUIC:            %s (tunlr filter quic on)\n", a.style.dim("allowed"))
		}
		list := strings.Join(s.PortList, ",")
		note := "tunlr filter ports set"
		if s.DefaultPorts {
			note = "default; " + note
		}
		if s.Ports {
			fmt.Fprintf(w, "Ports:           %s (tunlr filter ports off)\n", a.style.good("only common ports proxied, others go direct"))
			fmt.Fprintf(w, "                 %s (%s)\n", list, note)
		} else {
			fmt.Fprintf(w, "Ports:           %s (tunlr filter ports on)\n", a.style.dim("all proxied"))
			fmt.Fprintf(w, "                 when on: %s (%s)\n", list, note)
		}
		fmt.Fprintln(w, "\nThe rules go before all others (the managed mixin tunlr:filter; see tunlr config).")
		return nil
	})
}

// describeFilter 用于 status 和菜单的过滤项摘要，如 "2 devices direct · QUIC blocked"。
func (a *app) describeFilter(s *setup.FilterStatus) string {
	var parts []string
	switch n := len(s.Direct); n {
	case 0:
	case 1:
		parts = append(parts, "1 device direct")
	default:
		parts = append(parts, fmt.Sprintf("%d devices direct", n))
	}
	switch n := len(s.Sources); n {
	case 0:
	case 1:
		parts = append(parts, "1 source direct")
	default:
		parts = append(parts, fmt.Sprintf("%d sources direct", n))
	}
	if s.QUIC {
		parts = append(parts, "QUIC blocked")
	}
	if s.Ports {
		parts = append(parts, "common ports only")
	}
	if len(parts) == 0 {
		return a.style.dim("none")
	}
	return strings.Join(parts, a.style.dim(" · "))
}

func (a *app) newFilterDeviceCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "device [device] [proxy|direct]",
		Short: "Set whether a LAN device follows the rules (proxy) or goes direct for everything",
		Long: "The device is an address, a MAC address or a name (see tunlr devices). An address no device\n" +
			"has used yet can be set in advance. The rule matches the device's address (mihomo cannot match\n" +
			"by MAC address), so reserve the address for the device on your router. Setting it to proxy\n" +
			"again removes the rule. The configuration is published after the diff is confirmed.",
		Args: usageArgs(cobra.MaximumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 && args[1] != setup.RouteProxy && args[1] != setup.RouteDirect {
				return usageErrorf("the route must be proxy or direct, got %q", args[1])
			}
			if len(args) < 2 && !a.interactive() {
				return usageErrorf("filter device needs the device and the route (proxy or direct)")
			}
			if err := a.requireConfirmation("filter device"); err != nil {
				return err
			}
			ctx := cmd.Context()
			d, err := a.chooseDevice(ctx, "filter device", args)
			if stop, err := a.cancelled(err); stop {
				return err
			}
			route := ""
			if len(args) == 2 {
				route = args[1]
			} else {
				routes := []string{setup.RouteProxy, setup.RouteDirect}
				initial := 1
				if d.Route == setup.RouteDirect {
					initial = 0
				}
				k, err := a.prompt.Choose(ctx, "How should "+deviceLabel(*d)+" go out?", []string{
					"proxy   follow the rules (default)", "direct  everything direct, no proxies"}, initial)
				if stop, err := a.cancelled(err); stop {
					return err
				}
				route = routes[k]
			}
			return a.editDevice(ctx, d, setup.DeviceEdit{Route: &route}, "Publish the configuration with this device's route?",
				func(d setup.Device) string {
					if route == setup.RouteDirect {
						return fmt.Sprintf("%s now goes direct for everything.\n"+
							"The rule matches %s: reserve this address for the device on your router (DHCP reservation)\n"+
							"so it keeps it.", deviceLabel(d), d.IP)
					}
					return fmt.Sprintf("%s follows the rules again.", deviceLabel(d))
				})
		},
	}
}

func (a *app) newFilterSourceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "source",
		Short: "List the address ranges whose traffic goes direct; add or remove them",
		Long: "Traffic whose source is in a listed range goes direct for everything, e.g. a Docker network\n" +
			"(docker network inspect shows its subnet; containers with network_mode: host count as this\n" +
			"server) or a VPN subnet. A range is a network address with its prefix length, such as\n" +
			"172.17.0.0/16, or a single address.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.installer().Filter()
			if err != nil {
				return err
			}
			return a.render(s.Sources, func() error {
				if len(s.Sources) == 0 {
					_, err := fmt.Fprintln(a.stdout, "No address range goes direct (tunlr filter source add <network>).")
					return err
				}
				for _, p := range s.Sources {
					fmt.Fprintln(a.stdout, p)
				}
				_, err := fmt.Fprintln(a.stdout, "\n(tunlr filter source remove <network> lets one follow the rules again)")
				return err
			})
		},
	}
	add := &cobra.Command{
		Use:   "add [network]",
		Short: "Send all traffic from an address range direct",
		Long:  "The configuration is published after the diff is confirmed.",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var network string
			if len(args) == 1 {
				network = args[0]
			} else {
				if !a.interactive() {
					return usageErrorf("filter source add needs the network (e.g. 172.17.0.0/16)")
				}
				var err error
				network, err = a.prompt.Input(ctx, "Network", "A network address with its prefix length, such as 172.17.0.0/16, or a single address.",
					func(s string) error {
						_, err := pipeline.ParseSource(s)
						return err
					})
				if stop, err := a.cancelled(err); stop {
					return err
				}
			}
			p, err := pipeline.ParseSource(network)
			if err != nil {
				return usageErrorf("%v", err)
			}
			return a.editFilter(ctx, "filter source add", setup.FilterEdit{AddSource: p}, "Publish the configuration with this rule?",
				fmt.Sprintf("Traffic from %s now goes direct for everything.", p))
		},
	}
	remove := &cobra.Command{
		Use:   "remove [network]",
		Short: "Let traffic from an address range follow the rules again",
		Long:  "The configuration is published after the diff is confirmed.",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var network string
			if len(args) == 1 {
				network = args[0]
			} else {
				if !a.interactive() {
					return usageErrorf("filter source remove needs the network")
				}
				s, err := a.installer().Filter()
				if err != nil {
					return err
				}
				if len(s.Sources) == 0 {
					return usageErrorf("no address range goes direct")
				}
				k, err := a.prompt.Choose(ctx, "Which range?", s.Sources, 0)
				if stop, err := a.cancelled(err); stop {
					return err
				}
				network = s.Sources[k]
			}
			p, err := pipeline.ParseSource(network)
			if err != nil {
				return usageErrorf("%v", err)
			}
			return a.editFilter(ctx, "filter source remove", setup.FilterEdit{RemoveSource: p}, "Publish the configuration without this rule?",
				fmt.Sprintf("Traffic from %s follows the rules again.", p))
		},
	}
	cmd.AddCommand(a.withFullDiff(add), a.withFullDiff(remove))
	return cmd
}
func (a *app) newFilterQUICCommand(on bool) *cobra.Command {
	use, short := "off", "Allow QUIC again"
	if on {
		use, short = "on", "Block QUIC (UDP 443) so browsers use TCP"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			question, done := "Allow QUIC again?", "QUIC is allowed again."
			if on {
				question, done = "Block QUIC?", "QUIC is blocked; browsers use TCP instead."
			}
			return a.editFilter(cmd.Context(), "filter quic "+use, setup.FilterEdit{QUIC: &on}, question, done)
		},
	}
}

func (a *app) newFilterPortsCommand(on bool) *cobra.Command {
	use, short := "off", "Proxy all ports again"
	if on {
		use, short = "on", "Proxy only common ports; others go direct"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			question, done := "Proxy all ports again?", "All ports follow the rules again."
			if on {
				question, done = "Proxy only these ports?", "Only common ports are proxied now; others go direct."
			}
			return a.editFilter(cmd.Context(), "filter ports "+use, setup.FilterEdit{Ports: &on}, question, done)
		},
	}
}

func (a *app) newFilterPortsSetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set [ports|default]",
		Short: "Change the ports that are proxied when only common ports are",
		Long: "Ports and ranges separated by commas, e.g. 22,80,443,8000-9000; default restores the list\n" +
			"(" + strings.Join(pipeline.DefaultPorts, ",") + "). The list is kept while the filter is off.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var spec string
			if len(args) == 1 {
				spec = args[0]
			} else {
				if !a.interactive() {
					return usageErrorf("filter ports set needs the ports (e.g. 22,80,443,8000-9000) or default")
				}
				s, err := a.installer().Filter()
				if err != nil {
					return err
				}
				spec, err = a.prompt.Input(ctx, "Ports to proxy", "Now: "+strings.Join(s.PortList, ",")+
					". Separate with commas; ranges like 8000-9000; default restores the default list.", func(s string) error {
					if s == "default" {
						return nil
					}
					_, err := pipeline.ParsePorts(s)
					return err
				})
				if stop, err := a.cancelled(err); stop {
					return err
				}
			}
			list := pipeline.DefaultPorts
			if spec != "default" {
				var err error
				if list, err = pipeline.ParsePorts(spec); err != nil {
					return usageErrorf("%v", err)
				}
			}
			done := "Proxied ports: " + strings.Join(list, ",") + "."
			return a.editFilter(ctx, "filter ports set", setup.FilterEdit{PortList: list}, "Publish the configuration with these ports?", done)
		},
	}
}

// editFilter 是修改全局过滤项的流程：已经是目标状态时只报告；配置没有变化时直接保存，否则预览、确认后与
// 过滤项一起发布。
func (a *app) editFilter(ctx context.Context, command string, edit setup.FilterEdit, question, done string) error {
	inst := a.installer()
	var before, after state.Filter
	plan, err := a.plan(func() (p *pipeline.Plan, err error) {
		before, after, p, err = inst.PlanFilter(ctx, edit)
		return p, err
	})
	if err != nil {
		return err
	}
	v := filterEditView{Filter: after}
	if setup.SameFilter(before, after) && !plan.Changed() {
		return a.render(v, func() error {
			_, err := fmt.Fprintln(a.stdout, "Already so; nothing to change.")
			return err
		})
	}
	if !plan.Changed() {
		if a.opts.DryRun {
			return a.render(v, func() error {
				_, err := fmt.Fprintln(a.stdout, "Dry run: nothing changed (the configuration stays the same).")
				return err
			})
		}
		if _, err := inst.SaveFilter(ctx, after, nil); err != nil {
			return err
		}
		return a.render(v, func() error {
			msg := done
			if edit.PortList != nil && !after.Ports {
				msg += " They take effect with tunlr filter ports on."
			}
			_, err := fmt.Fprintln(a.stdout, msg)
			return err
		})
	}
	pv := newPlanView(plan, false)
	v.Plan = &pv
	if !a.opts.JSON {
		a.printPlan(pv)
	}
	if err := checkExit(plan, false); err != nil || a.opts.DryRun {
		if a.opts.JSON {
			if rerr := a.render(v, nil); rerr != nil {
				return rerr
			}
		} else if err != nil {
			fmt.Fprintln(a.stdout, "\nNot changed.")
		} else {
			fmt.Fprintln(a.stdout, "\nDry run: nothing changed.")
		}
		return err
	}
	if err := a.requireConfirmation(command); err != nil {
		return err
	}
	if ok, err := a.ask(ctx, question, "", "Not changed."); !ok {
		return err
	}
	err = a.spin("Publishing the configuration", func() (err error) {
		v.Revision, err = inst.SaveFilter(ctx, after, plan)
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
		fmt.Fprintf(a.stdout, "\n%s\n", a.style.good(done))
		if v.Revision != nil {
			fmt.Fprintf(a.stdout, "Published revision %d.\n", v.Revision.Number)
		}
		return nil
	})
}
