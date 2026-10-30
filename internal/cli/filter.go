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
		Short: "Show and set which traffic skips the subscription's rules (networks, QUIC, ports)",
		Long: "Filters are rules placed before all others (the managed mixin tunlr:filter), in this order:\n" +
			"  device   a LAN device set to direct (tunlr devices route) goes direct for everything,\n" +
			"           unaffected by the rest\n" +
			"  source   so does traffic from an address range (e.g. a Docker network, a VPN subnet)\n" +
			"  quic     UDP port 443 is rejected, so browsers use TCP instead of QUIC (which many proxies\n" +
			"           carry poorly); sites that go direct fall back to TCP too\n" +
			"  ports    only connections to some ports (common ones by default) are proxied; others (BT, P2P,\n" +
			"           games) go direct\n" +
			"All are off by default. A mixin can override the rules or remove them with tunlr.rules.delete.",
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
		Short: "Show whether only some ports are proxied; turn it on or off, or change the ports",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.showFilter() },
	}
	ports.AddCommand(a.withFullDiff(a.newFilterPortsCommand(true)), a.withFullDiff(a.newFilterPortsCommand(false)),
		a.withFullDiff(a.newFilterPortsSetCommand()))
	cmd.AddCommand(a.newFilterSourceCommand(), quic, ports)
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
			fmt.Fprintf(w, "Direct devices:  %s (tunlr devices route <device> direct)\n", a.style.dim("none"))
		} else {
			names := make([]string, len(s.Direct))
			for i, d := range s.Direct {
				names[i] = d.IP
				if d.Name != "" {
					names[i] = fmt.Sprintf("%s (%s)", d.Name, d.IP)
				}
			}
			fmt.Fprintf(w, "Direct devices:  %s (tunlr devices route <device> proxy)\n", a.style.warn(strings.Join(names, ", ")))
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
			fmt.Fprintf(w, "Ports:           %s (tunlr filter ports off)\n", a.style.good("only these ports proxied, others go direct"))
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
		parts = append(parts, "some ports only")
	}
	if len(parts) == 0 {
		return a.style.dim("none")
	}
	return strings.Join(parts, a.style.dim(" · "))
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
				network, err = a.prompt.Input(ctx, "Network", "A network address with its prefix length, such as 172.17.0.0/16, or a single address.", "",
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

// portsHelp 是端口列表的写法，用于 filter ports on、set 的说明。
var portsHelp = "Ports and ranges separated by commas, e.g. 22,80,443,8000-9000; default restores the list\n" +
	"(" + strings.Join(pipeline.DefaultPorts, ",") + ")."

func (a *app) newFilterPortsCommand(on bool) *cobra.Command {
	if !on {
		return &cobra.Command{
			Use:   "off",
			Short: "Proxy all ports again",
			Long:  "The list of ports is kept for the next tunlr filter ports on.",
			Args:  usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.editFilter(cmd.Context(), "filter ports off", setup.FilterEdit{Ports: &on},
					"Proxy all ports again?", "All ports follow the rules again.")
			},
		}
	}
	return &cobra.Command{
		Use:   "on [ports|default]",
		Short: "Proxy only some ports (common ones by default); others go direct",
		Long: portsHelp + " Without ports, the saved list is used\n" +
			"(asked in a terminal, filled in with it).",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			list, ok, err := a.portList(ctx, args, a.interactive())
			if !ok {
				return err
			}
			return a.editFilter(ctx, "filter ports on", setup.FilterEdit{Ports: &on, PortList: list}, "Proxy only these ports?",
				"Only these ports are proxied now: "+strings.Join(list, ",")+"; others go direct.")
		},
	}
}

func (a *app) newFilterPortsSetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set [ports|default]",
		Short: "Change the ports that are proxied when only some are",
		Long:  portsHelp + " The list is kept while the filter is off.",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !a.interactive() {
				return usageErrorf("filter ports set needs the ports (e.g. 22,80,443,8000-9000) or default")
			}
			ctx := cmd.Context()
			list, ok, err := a.portList(ctx, args, true)
			if !ok {
				return err
			}
			return a.editFilter(ctx, "filter ports set", setup.FilterEdit{PortList: list}, "Publish the configuration with these ports?",
				"Proxied ports: "+strings.Join(list, ",")+".")
		},
	}
}

// portList 返回参数给出的端口列表。没有参数时 ask 为真则询问（预先填好保存的列表），否则用保存的列表。
// ok 为假时不再继续：err 为 nil 表示用户取消了。
func (a *app) portList(ctx context.Context, args []string, ask bool) (list []string, ok bool, err error) {
	var spec string
	if len(args) == 1 {
		spec = args[0]
	} else {
		s, err := a.installer().Filter()
		if err != nil {
			return nil, false, err
		}
		if !ask {
			return s.PortList, true, nil
		}
		spec, err = a.prompt.Input(ctx, "Ports to proxy", "Others go direct. Separate with commas; ranges like 8000-9000;\n"+
			"default restores the default list.", strings.Join(s.PortList, ","), func(s string) error {
			if s == "default" {
				return nil
			}
			_, err := pipeline.ParsePorts(s)
			return err
		})
		if stop, err := a.cancelled(err); stop {
			return nil, false, err
		}
	}
	if spec == "default" {
		return pipeline.DefaultPorts, true, nil
	}
	if list, err = pipeline.ParsePorts(spec); err != nil {
		return nil, false, usageErrorf("%v", err)
	}
	return list, true, nil
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
