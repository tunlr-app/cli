package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
)

// IPv6 分两层（DESIGN.md 8.3）：流量（tunlr ipv6）和回答（tunlr dns ipv6）。ipv6Traffic、ipv6Answers 是两层的
// 取值，顺序与选择时的选项（ipv6TrafficOptions、ipv6AnswerOptions）一致。
var (
	ipv6Traffic = []string{setup.IPv6Rules, setup.IPv6Bypass}
	ipv6Answers = []string{setup.IPv6Off, setup.IPv6On, setup.IPv6Default}
)

func (a *app) newIPv6Command() *cobra.Command {
	return a.withFullDiff(&cobra.Command{
		Use:       "ipv6 [rules|bypass]",
		Short:     "Show or set whether connections to IPv6 addresses go through mihomo: rules or bypass",
		ValidArgs: ipv6Traffic,
		Long: "How this server's IPv6 is handled, in two layers. They matter with TUN on, when this server's\n" +
			"programs look names up through mihomo and their traffic goes through it.\n\n" +
			"Traffic (tunlr ipv6): connections to IPv6 addresses\n" +
			"  rules    through mihomo, by the rules (the default). IPv6 DNS servers and IPv6 addresses written\n" +
			"           into programs go through mihomo too, so nothing goes around it, also once this server\n" +
			"           gets IPv6.\n" +
			"  bypass   directly, by the system's routes: not through the proxy and not by the rules. mihomo\n" +
			"           leaves IPv6 alone (ipv6: false) and uses none itself, so names get IPv4 addresses only,\n" +
			"           and this server's DNS queries to IPv6 servers are rejected so that they go to mihomo too.\n" +
			"           If names cannot be looked up then (IPv6 DNS servers only), tunlr goes back. Without IPv6\n" +
			"           on this server nothing seems to change, until it gets IPv6.\n" +
			"Answers (tunlr dns ipv6): whether names get IPv6 addresses: off (tunlr's fallback), on, or\n" +
			"  default; see tunlr dns ipv6 --help. Not used with bypass; they apply again with rules.\n\n" +
			"To keep IPv6 from going around the proxy, use rules with the answers off (the defaults).\n\n" +
			"The traffic layer is kept in tunlr's state; the top-level ipv6 key is tunlr's: mixins cannot\n" +
			"set it. The configuration is published after you confirm. With TUN off this server's programs\n" +
			"do not use mihomo, and the setting only affects mihomo's own connections (from the proxy port,\n" +
			"to the nodes).\n\n" +
			"Without a value, show both layers; in a terminal, then choose: first the traffic, then with rules\n" +
			"the answers (Esc there goes back to the first question).",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			switch {
			case len(args) == 1 && slices.Contains(ipv6Answers, args[0]):
				return usageErrorf("whether names get IPv6 addresses is set with tunlr dns ipv6 %s; tunlr ipv6 takes rules or bypass", args[0])
			case len(args) == 1 && !slices.Contains(ipv6Traffic, args[0]):
				return usageErrorf("ipv6 takes rules or bypass, got %q", args[0])
			case len(args) == 1:
				return a.setIPv6(ctx, setup.IPv6Change{Traffic: args[0]})
			}
			return a.showIPv6(ctx, a.chooseIPv6)
		},
	})
}

func (a *app) newDNSIPv6Command() *cobra.Command {
	return &cobra.Command{
		Use:       "ipv6 [on|off|default]",
		Short:     "Set whether names get IPv6 addresses: on, off, or default",
		ValidArgs: ipv6Answers,
		Long: "Whether mihomo's DNS answers IPv6 addresses (dns.ipv6) to the queries TUN catches:\n" +
			"  off      tunlr's fallback. Names get IPv4 addresses only, so programs that look names up\n" +
			"           connect over IPv4.\n" +
			"  on       names get IPv6 addresses too. In fake-ip mode they are fake ones from\n" +
			"           dns.fake-ip-range6 (written as " + setup.FakeIPRange6 + " unless set), so names with IPv6 only\n" +
			"           become reachable even without IPv6 on this server; in redir-host mode they get real\n" +
			"           ones, and direct connections to those need IPv6 on this server. The gateway forwards\n" +
			"           only IPv4, so with the gateway on, LAN devices that have IPv6 through the router send\n" +
			"           those connections to the router.\n" +
			"  default  the configuration decides again: tunlr's fallback (off), or the subscription's dns\n" +
			"           with tunlr sub set <sub> --dns sub.\n\n" +
			"Whether connections to IPv6 addresses go through mihomo is tunlr ipv6 (rules or bypass). With\n" +
			"bypass mihomo answers no IPv6 addresses whatever this says; the setting is kept and applies\n" +
			"again with tunlr ipv6 rules.\n\n" +
			"The setting is written to the mixin file that now sets dns.ipv6, or else mixin.d/" + setup.DNSMixinFile + "\n" +
			"(default removes it from the shared mixins and those of the active subscription). The\n" +
			"configuration is published after you confirm. Without a value, show it; in a terminal, then\n" +
			"choose it.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			switch {
			case len(args) == 1 && slices.Contains(ipv6Traffic, args[0]):
				return usageErrorf("whether connections to IPv6 addresses go through mihomo is set with tunlr ipv6 %s; tunlr dns ipv6 takes on, off or default", args[0])
			case len(args) == 1 && !slices.Contains(ipv6Answers, args[0]):
				return usageErrorf("dns ipv6 takes on, off or default, got %q", args[0])
			case len(args) == 1:
				return a.setIPv6(ctx, setup.IPv6Change{Answers: args[0]})
			}
			return a.showIPv6(ctx, a.chooseIPv6Answers)
		},
	}
}

// showIPv6 是不带取值的 ipv6、dns ipv6：显示两层的设置，在终端中接着用 choose 选择。
func (a *app) showIPv6(ctx context.Context, choose func(context.Context, *setup.IPv6Status) error) error {
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
		return choose(ctx, s)
	}
	return a.render(s, func() error {
		print()
		return nil
	})
}

// printIPv6 输出 IPv6 的两层设置，每层一行，括号中是来源和效果：只写 off 容易被当成完全不用 IPv6。
func (a *app) printIPv6(w io.Writer, s *setup.IPv6Status) {
	if s.Bypass() {
		fmt.Fprintf(w, "IPv6 traffic:      %s %s\n", a.style.warn(setup.IPv6Bypass),
			a.style.dim("(set by tunlr ipv6 bypass; connections to IPv6 addresses go out directly, not through mihomo)"))
	} else {
		fmt.Fprintf(w, "IPv6 traffic:      %s %s\n", setup.IPv6Rules, a.style.dim("(connections to IPv6 addresses go through mihomo, by the rules)"))
	}
	fmt.Fprintf(w, "IPv6 answers:      %s %s\n", s.Answers.Value(), a.style.dim("("+ipv6AnswerSource(s)+"; "+ipv6AnswerEffect(s)+")"))
}

// ipv6AnswerSource 说明 dns.ipv6 来自哪里。
func ipv6AnswerSource(s *setup.IPv6Status) string {
	d := s.Answers
	if !d.Set {
		return "not set; mihomo's default"
	}
	return describeDNSSource(d.From, d.Fallback, d.Set, d.Subscription)
}

// ipv6AnswerEffect 用几个词说明回答这一层的效果；bypass 时它不起作用。
func ipv6AnswerEffect(s *setup.IPv6Status) string {
	switch {
	case !s.Answers.Active:
		return "not used while IPv6 bypasses mihomo"
	case s.Answers.On:
		return "names get IPv6 addresses too"
	}
	return "names get IPv4 addresses only"
}

// describeIPv6 是 DNS 菜单中 IPv6 一项的现状，如 "rules, answers off"、"bypass; answers off, not used"。
func describeIPv6(s *setup.IPv6Status) string {
	if !s.Answers.Active {
		return s.Traffic + "; answers " + s.Answers.Value() + ", not used"
	}
	return s.Traffic + ", answers " + s.Answers.Value()
}

// chooseIPv6 是终端中不带取值的 ipv6 和主菜单 DNS 中的 IPv6：先选流量；选 rules 时再选回答，在这一步按 Esc 回到
// 第一步。两层一起改时合成一份计划，只确认、发布一次。
func (a *app) chooseIPv6(ctx context.Context, s *setup.IPv6Status) error {
	ch, ok, err := a.pickIPv6(ctx, s)
	if !ok {
		return err
	}
	return a.setIPv6(ctx, ch)
}

// pickIPv6 是 chooseIPv6 中的两步选择，返回要改的；用户取消时 ok 为 false。
func (a *app) pickIPv6(ctx context.Context, s *setup.IPv6Status) (ch setup.IPv6Change, ok bool, err error) {
	for {
		options, initial := ipv6TrafficOptions(s)
		k, err := a.prompt.Choose(ctx, "How should connections to IPv6 addresses go?", options, initial)
		if stop, err := a.cancelled(err); stop {
			return ch, false, err
		}
		ch.Traffic = ipv6Traffic[k]
		if ch.Traffic == setup.IPv6Bypass {
			return ch, true, nil
		}
		options, initial = ipv6AnswerOptions(s)
		k, err = a.prompt.Choose(ctx, "Do names get IPv6 addresses?", options, initial)
		if errors.Is(err, huh.ErrUserAborted) {
			continue
		}
		if err != nil {
			return ch, false, err
		}
		ch.Answers = ipv6Answers[k]
		return ch, true, nil
	}
}

// chooseIPv6Answers 是终端中不带取值的 dns ipv6：只选回答这一层。
func (a *app) chooseIPv6Answers(ctx context.Context, s *setup.IPv6Status) error {
	if s.Bypass() {
		fmt.Fprintln(a.stdout, a.style.dim("IPv6 bypasses mihomo now, so mihomo answers no IPv6 addresses; what you choose applies with tunlr ipv6 rules."))
	}
	options, initial := ipv6AnswerOptions(s)
	k, err := a.prompt.Choose(ctx, "Do names get IPv6 addresses?", options, initial)
	if stop, err := a.cancelled(err); stop {
		return err
	}
	return a.setIPv6(ctx, setup.IPv6Change{Answers: ipv6Answers[k]})
}

// ipv6TrafficOptions 是选择流量这一层的选项（与 ipv6Traffic 对应）和光标的初始位置（现在的设置）。
func ipv6TrafficOptions(s *setup.IPv6Status) ([]string, int) {
	now := func(v string) string {
		if s.Traffic == v {
			return " (now)"
		}
		return ""
	}
	return []string{
		"By the rules: through mihomo, so IPv6 does not go around it" + now(setup.IPv6Rules),
		"Bypass: directly, not through mihomo or the rules; names get IPv4 addresses only" + now(setup.IPv6Bypass),
	}, max(slices.Index(ipv6Traffic, s.Traffic), 0)
}

// ipv6AnswerOptions 是选择回答这一层的选项（与 ipv6Answers 对应）和光标的初始位置：dns.ipv6 由 mixin 写定时停在
// 它上面，否则停在“按配置”，同 dnsModeOptions。bypass 时标出的是留着、换回 rules 时用的设置。
func ipv6AnswerOptions(s *setup.IPv6Status) ([]string, int) {
	pinned, value := s.Answers.Pinned(), s.Answers.Value()
	now := func(v string) string {
		if pinned && value == v {
			return " (now)"
		}
		return ""
	}
	decide := "Let the configuration decide"
	if !pinned {
		decide += " (now: " + value + ")"
	}
	initial := 2
	if i := slices.Index(ipv6Answers, value); pinned && i >= 0 {
		initial = i
	}
	return []string{
		"Off: IPv4 addresses only" + now(setup.IPv6Off),
		"On: IPv6 addresses too" + now(setup.IPv6On),
		decide,
	}, initial
}

// ipv6View 是 ipv6、dns ipv6 在 --json 输出中的形式。
type ipv6View struct {
	*setup.IPv6Edit
	publishedView
}

// setIPv6 是 ipv6 rules|bypass、dns ipv6 on|off|default 和两步选择的流程，同 setDNSMode：流量这一层只改 tunlr 的
// 状态，回答这一层改 mixin 文件；两层一起改时合成一份计划。
func (a *app) setIPv6(ctx context.Context, ch setup.IPv6Change) error {
	inst := a.installer()
	var edit *setup.IPv6Edit
	plan, err := a.plan(func() (p *pipeline.Plan, err error) {
		edit, p, err = inst.PlanIPv6(ctx, ch)
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
	command := "ipv6 " + ch.Traffic
	switch {
	case ch.Answers != "" && ch.Traffic != "":
		command = "ipv6"
	case ch.Answers != "":
		command = "dns ipv6 " + ch.Answers
	}
	return a.publishEdit(ctx, mixinEdit{
		command: command, question: "Publish the configuration with this setting?", done: ipv6Done(edit),
		changes: edit.Changes, plan: plan, view: v, pv: &v.publishedView,
		save:  func(p *pipeline.Plan) (*config.Revision, error) { return inst.SaveIPv6(ctx, edit, p) },
		notes: func() { a.printIPv6Notes(edit) },
	})
}

// ipv6Done 是改完 IPv6 后的一句话：流量这一层改了什么，回答这一层现在的效果。
func ipv6Done(edit *setup.IPv6Edit) string {
	var parts []string
	switch before := edit.Before.Bypass(); {
	case edit.Bypass && !before:
		parts = append(parts, "IPv6 bypasses mihomo now.")
	case !edit.Bypass && before:
		parts = append(parts, "IPv6 goes through mihomo again, by the rules.")
	}
	answers := "IPv4 addresses only"
	if edit.After {
		answers = "IPv6 addresses too"
	}
	switch {
	case edit.Unused:
		parts = append(parts, "The IPv6 answers are saved; they apply once IPv6 goes through mihomo again (tunlr ipv6 rules).")
	case edit.Bypass:
	case edit.Set.Answers == setup.IPv6Default && len(edit.Changes) > 0:
		parts = append(parts, "The configuration decides again: names get "+answers+".")
	default:
		parts = append(parts, "Names get "+answers+" now.")
	}
	return strings.Join(parts, " ")
}

// printIPv6Notes 说明改为 bypass 后的效果（按 TUN 开关和本机有没有 IPv6 出口），bypass 时改回答不起作用；打开 IPv6
// 的回答后，提醒网关只转发 IPv4，以及不是 fake-ip 模式时本机没有 IPv6 出口。
func (a *app) printIPv6Notes(edit *setup.IPv6Edit) {
	bypassing := edit.Bypass && !edit.Before.Bypass()
	switch {
	case bypassing && !edit.Tun():
		fmt.Fprintf(a.stdout, "\n%s TUN is off, so this server's programs do not use mihomo anyway; mihomo itself stops using IPv6.\n"+
			"With TUN on, names will get IPv4 addresses only, and connections to IPv6 addresses go out directly.\n",
			a.style.warn("Note:"))
	case bypassing && !edit.Route:
		fmt.Fprintf(a.stdout, "\n%s this server has no IPv6 route to the internet now, so nothing seems to change;\n"+
			"once it has one, connections to IPv6 addresses go out directly, not through the proxy.\n",
			a.style.warn("Note:"))
	case bypassing:
		fmt.Fprintf(a.stdout, "\n%s names still get IPv4 addresses only and go through mihomo: this server's DNS queries to IPv6\n"+
			"servers are rejected so that they go to mihomo too. Connections to IPv6 addresses go out directly,\n"+
			"not through the proxy and not by the rules.\n",
			a.style.warn("Note:"))
	case edit.Unused:
		fmt.Fprintf(a.stdout, "\n%s IPv6 bypasses mihomo (tunlr ipv6 bypass), so mihomo answers no IPv6 addresses whatever dns.ipv6\n"+
			"says; the setting is saved and applies with tunlr ipv6 rules.\n",
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
