package cli

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
)

// ipv6Values 是 ipv6 的取值，顺序与选择时的选项（ipv6Options）一致：bypass 让 IPv6 不经代理，排在 off、on 之后。
var ipv6Values = []string{setup.IPv6Off, setup.IPv6On, setup.IPv6Bypass, setup.IPv6Default}

func (a *app) newIPv6Command() *cobra.Command {
	return a.withFullDiff(&cobra.Command{
		Use:       "ipv6 [off|on|bypass|default]",
		Short:     "Show or set how IPv6 is handled: off, on, bypass, or default",
		ValidArgs: ipv6Values,
		Long: "How this server's IPv6 is handled. It matters with TUN on, when this server's programs look\n" +
			"names up through mihomo and their traffic goes through it:\n\n" +
			"           IPv6 addresses for names   connections to IPv6 addresses\n" +
			"  off      no                         through mihomo, by the rules\n" +
			"  on       yes                        through mihomo, by the rules\n" +
			"  bypass   no                         directly, not through mihomo\n\n" +
			"  off      tunlr's fallback (dns.ipv6: false). Programs that look names up connect over IPv4;\n" +
			"           IPv6 DNS servers and IPv6 addresses written into programs go through mihomo too, so\n" +
			"           nothing leaks.\n" +
			"  on       dns.ipv6: true. In fake-ip mode names get fake IPv6 addresses from dns.fake-ip-range6\n" +
			"           (written as " + setup.FakeIPRange6 + " unless set), so names with IPv6 only become\n" +
			"           reachable even without IPv6 on this server; in redir-host mode they get real ones, and\n" +
			"           direct connections to those need IPv6 on this server. The gateway forwards only IPv4, so\n" +
			"           with the gateway on, LAN devices that have IPv6 through the router send those\n" +
			"           connections to the router.\n" +
			"  bypass   mihomo leaves IPv6 alone (ipv6: false) and uses none itself. Names still get IPv4\n" +
			"           addresses only: mihomo answers no IPv6 ones, and this server's DNS queries to IPv6\n" +
			"           servers are rejected so that they go to mihomo too. Only connections to IPv6 addresses\n" +
			"           go out directly by the system's routes, not through the proxy and not by the rules.\n" +
			"           If names cannot be looked up then (IPv6 DNS servers only), tunlr goes back. Without\n" +
			"           IPv6 on this server it is much the same as off.\n" +
			"  default  Ends bypass; whether names get IPv6 addresses goes back to the configuration: tunlr's\n" +
			"           fallback (off), or the subscription's dns with tunlr sub set <sub> --dns sub.\n\n" +
			"With TUN off this server's programs do not use mihomo, and the setting only affects mihomo's\n" +
			"own connections (from the proxy port, to the nodes).\n\n" +
			"bypass is kept in tunlr's state; off, on and default write dns.ipv6 to the mixin file that now\n" +
			"sets it, or else mixin.d/" + setup.DNSMixinFile + " (default removes it from the shared mixins and those\n" +
			"of the active subscription). The top-level ipv6 key is tunlr's: mixins cannot set it. The\n" +
			"configuration is published after you confirm.\n\n" +
			"Without a value, show the setting; in a terminal, then choose it.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			switch {
			case len(args) == 1 && !slices.Contains(ipv6Values, args[0]):
				return usageErrorf("ipv6 takes off, on, bypass or default, got %q", args[0])
			case len(args) == 1:
				return a.setIPv6(ctx, args[0])
			}
			var s *setup.IPv6Status
			if err := a.spin("Composing the configuration", func() (err error) {
				s, err = a.installer().IPv6(ctx)
				return err
			}); err != nil {
				return err
			}
			print := func() {
				a.printIPv6(a.stdout, s)
				if !s.Tun {
					fmt.Fprintln(a.stdout, a.style.dim("TUN is off, so this server's programs do not use mihomo now; the setting only affects mihomo's own connections."))
				}
			}
			if a.interactive() {
				print()
				fmt.Fprintln(a.stdout)
				return a.chooseIPv6(ctx, s)
			}
			return a.render(s, func() error {
				print()
				return nil
			})
		},
	})
}

// ipv6Source 说明 IPv6 的设置来自哪里：bypass 来自 tunlr 的状态，off、on 来自决定 dns.ipv6 的地方。
func ipv6Source(s *setup.IPv6Status) string {
	if s.Mode == setup.IPv6Bypass {
		return "set by tunlr ipv6 bypass"
	}
	d := s.Answers
	if !d.Set {
		return "not set; mihomo's default"
	}
	return describeDNSSource(d.From, d.Fallback, d.Set, d.Subscription)
}

// printIPv6 输出 IPv6 的设置和来源，下面一行说明它的效果（TUN 开着时的）：只写 off 容易被当成完全不用 IPv6。
func (a *app) printIPv6(w io.Writer, s *setup.IPv6Status) {
	fmt.Fprintf(w, "IPv6:              %s %s\n", s.Mode, a.style.dim("("+ipv6Source(s)+")"))
	fmt.Fprintf(w, "                   %s\n", ipv6Effect(s.Mode))
}

// ipv6Effect 用一句话说明 IPv6 的设置 mode 的效果。
func ipv6Effect(mode string) string {
	switch mode {
	case setup.IPv6Bypass:
		return "Names get IPv4 addresses only; connections to IPv6 addresses go out directly, not through mihomo."
	case setup.IPv6On:
		return "Names get IPv6 addresses too; connections to IPv6 addresses go through mihomo, by the rules."
	}
	return "Names get IPv4 addresses only; connections to IPv6 addresses go through mihomo, by the rules."
}

// chooseIPv6 让用户选择 IPv6 的设置，s 是现在的设置。
func (a *app) chooseIPv6(ctx context.Context, s *setup.IPv6Status) error {
	options, initial := ipv6Options(s)
	k, err := a.prompt.Choose(ctx, "How should IPv6 be handled?", options, initial)
	if stop, err := a.cancelled(err); stop {
		return err
	}
	return a.setIPv6(ctx, ipv6Values[k])
}

// ipv6Options 是选择 IPv6 的设置的选项（与 ipv6Values 对应）和光标的初始位置：bypass 或 dns.ipv6 由 mixin 写定时
// 停在它上面，否则停在“按配置”，同 dnsModeOptions。
func ipv6Options(s *setup.IPv6Status) ([]string, int) {
	pinned := s.Mode == setup.IPv6Bypass || s.Answers.Pinned()
	now := func(mode string) string {
		if pinned && s.Mode == mode {
			return " (now)"
		}
		return ""
	}
	decide := "Let the configuration decide whether names get IPv6 addresses"
	switch {
	case s.Mode == setup.IPv6Bypass:
		decide += " (ends bypass)"
	case !pinned:
		decide += " (now: " + s.Mode + ")"
	}
	initial := 3
	if i := slices.Index(ipv6Values, s.Mode); pinned && i >= 0 {
		initial = i
	}
	return []string{
		"Off: names get IPv4 addresses only; IPv6 goes through mihomo" + now(setup.IPv6Off),
		"On: names get IPv6 addresses too; IPv6 goes through mihomo" + now(setup.IPv6On),
		"Bypass: IPv6 goes out directly, not through mihomo; names get IPv4 addresses only" + now(setup.IPv6Bypass),
		decide,
	}, initial
}

// ipv6View 是 ipv6 在 --json 输出中的形式。
type ipv6View struct {
	*setup.IPv6Edit
	publishedView
}

// setIPv6 是 ipv6 off|bypass|on|default 的流程，同 setDNSMode；bypass 只改 tunlr 的状态，不改 mixin 文件。
func (a *app) setIPv6(ctx context.Context, value string) error {
	inst := a.installer()
	var edit *setup.IPv6Edit
	plan, err := a.plan(func() (p *pipeline.Plan, err error) {
		edit, p, err = inst.PlanIPv6(ctx, value)
		return p, err
	})
	if err != nil {
		return err
	}
	v := &ipv6View{IPv6Edit: edit}
	if !edit.Changed {
		return a.render(v, func() error {
			_, err := fmt.Fprintln(a.stdout, "Already so; nothing to change.")
			return err
		})
	}
	answers := "IPv4 addresses only"
	if edit.After {
		answers = "IPv6 addresses too"
	}
	done := "Names get " + answers + " now; IPv6 goes through mihomo."
	switch value {
	case setup.IPv6Bypass:
		done = "IPv6 bypasses mihomo now."
	case setup.IPv6Default:
		done = "The configuration decides again: " + onOff(edit.After) + "; names get " + answers + ", and IPv6 goes through mihomo."
	}
	return a.publishEdit(ctx, mixinEdit{
		command: "ipv6 " + value, question: "Publish the configuration with this setting?", done: done,
		changes: edit.Changes, plan: plan, view: v, pv: &v.publishedView,
		save:  func(p *pipeline.Plan) (*config.Revision, error) { return inst.SaveIPv6(ctx, edit, p) },
		notes: func() { a.printIPv6Notes(edit) },
	})
}

// printIPv6Notes 说明改为 bypass 后的效果（按 TUN 开关和本机有没有 IPv6 出口）；打开 IPv6 的回答后，提醒网关只转发
// IPv4，以及不是 fake-ip 模式时本机没有 IPv6 出口。
func (a *app) printIPv6Notes(edit *setup.IPv6Edit) {
	switch {
	case edit.Bypass && !edit.Tun():
		fmt.Fprintf(a.stdout, "\n%s TUN is off, so this server's programs do not use mihomo anyway; mihomo itself stops using IPv6.\n"+
			"With TUN on, names will get IPv4 addresses only, and connections to IPv6 addresses go out directly.\n",
			a.style.warn("Note:"))
	case edit.Bypass && !edit.Route:
		fmt.Fprintf(a.stdout, "\n%s this server has no IPv6 route to the internet now, so this is much the same as off;\n"+
			"once it has one, connections to IPv6 addresses go out directly, not through the proxy.\n",
			a.style.warn("Note:"))
	case edit.Bypass:
		fmt.Fprintf(a.stdout, "\n%s names still get IPv4 addresses only and go through mihomo: this server's DNS queries to IPv6\n"+
			"servers are rejected so that they go to mihomo too. Connections to IPv6 addresses go out directly,\n"+
			"not through the proxy and not by the rules.\n",
			a.style.warn("Note:"))
	}
	if edit.Gateway != "" {
		fmt.Fprintf(a.stdout, "\n%s %s.\n", a.style.warn("Warning:"), edit.Gateway)
	}
	if edit.NoRoute {
		mode := edit.DNSMode
		if mode == "" {
			mode = setup.DNSRedirHost
		}
		fmt.Fprintf(a.stdout, "\n%s in %s mode programs get real IPv6 addresses, and this server has no IPv6 route: direct\n"+
			"connections to them fail, unless sniffing finds the name. Proxied ones need nodes that reach IPv6.\n",
			a.style.warn("Note:"), mode)
	}
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
