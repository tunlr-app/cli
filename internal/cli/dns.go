package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// setSubDNS 是 sub set <sub> --dns tunlr|sub：订阅用谁的 DNS 和嗅探（写进它的 mixin），订阅启用时随后发布。from 为
// 空时（菜单的“Change a sub”）让用户选择。
func (a *app) setSubDNS(cmd *cobra.Command, sub, from string) error {
	w, err := a.writeSubDNS(cmd, sub, from)
	if err != nil || w == nil {
		return err
	}
	if len(w.Changes) == 0 {
		return a.render(w, func() error {
			_, err := fmt.Fprintf(a.stdout, "Already so for %q; nothing to change.\n", sub)
			return err
		})
	}
	return a.publishSubChange(cmd, sub, w.Active, w)
}

// writeSubDNS 是 setSubDNS 中发布之前的部分；与 --geo-* 一起给出时只写入，由 setGeoUpdate 一并发布。用户取消时
// 返回 nil。
func (a *app) writeSubDNS(cmd *cobra.Command, sub, from string) (*setup.DNSWrite, error) {
	ctx := cmd.Context()
	if a.opts.DryRun {
		return nil, usageErrorf("sub set --dns does not support --dry-run")
	}
	if from != "" && from != setup.DNSFromTunlr && from != setup.DNSFromSubscription {
		return nil, usageErrorf("--dns takes %s (tunlr's DNS and sniffer, the default) or %s (the subscription's), got %q",
			setup.DNSFromTunlr, setup.DNSFromSubscription, from)
	}
	entries, err := a.subscriptions().List()
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(entries, func(e subscription.Entry) bool { return e.Name == sub }) {
		return nil, fmt.Errorf("%w: %s", subscription.ErrNotFound, sub)
	}
	inst := a.installer()
	if from == "" {
		now, _, err := inst.SubscriptionDNS(sub)
		if err != nil {
			return nil, err
		}
		options := []string{
			"tunlr's (default): Chinese domains asked directly, others by the rules; sniffing on",
			"The subscription's: the dns and sniffer it sets, tunlr's where it sets none",
		}
		initial := 0
		if now == setup.DNSFromSubscription {
			initial = 1
		}
		k, err := a.prompt.Choose(ctx, fmt.Sprintf("Which DNS should %q use?", sub), options, initial)
		if stop, err := a.cancelled(err); stop {
			return nil, err
		}
		from = []string{setup.DNSFromTunlr, setup.DNSFromSubscription}[k]
	}
	if err := a.requireYes("sub set --dns"); err != nil {
		return nil, err
	}
	w, err := inst.SetSubscriptionDNS(ctx, sub, from)
	if err != nil {
		return nil, err
	}
	if !a.opts.JSON && len(w.Changes) > 0 {
		a.printMixinChanges(w.Changes, true)
		if from == setup.DNSFromTunlr {
			fmt.Fprintf(a.stdout, "%q uses tunlr's DNS and sniffer while it is active (tunlr config lists them).\n", sub)
		} else {
			fmt.Fprintf(a.stdout, "%q uses the DNS and sniffer it sets while it is active, tunlr's where it sets none.\n", sub)
		}
		if w.Leak != "" {
			fmt.Fprintf(a.stdout, "%s %s\n", a.style.warn("Warning:"), w.Leak)
		}
	}
	return w, nil
}

// printMixinChanges 列出命令改了（done）或将要改的 mixin 文件和其中的键，文件名相对于 mixin.d；没有时什么都不输出。
func (a *app) printMixinChanges(changes []*setup.MixinChange, done bool) {
	if len(changes) == 0 {
		return
	}
	title := "Mixin files to change (in mixin.d):"
	if done {
		title = "Changed mixin files (in mixin.d):"
	}
	fmt.Fprintln(a.stdout, title)
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	for _, c := range changes {
		var parts []string
		if len(c.Set) > 0 {
			parts = append(parts, "set "+strings.Join(c.Set, ", "))
		}
		if len(c.Unset) > 0 {
			parts = append(parts, "unset "+strings.Join(c.Unset, ", "))
		}
		if c.Removed {
			parts = append(parts, "nothing left, so the file is deleted")
		}
		fmt.Fprintf(tw, "  %s\t%s\n", c.Name, strings.Join(parts, "; "))
	}
	tw.Flush()
}

const dnsModeDefault = "default"

// dnsModes 是 dns mode 的取值，顺序与选择时的选项（dnsModeOptions）一致。
var dnsModes = []string{setup.DNSFakeIP, setup.DNSRedirHost, dnsModeDefault}

func (a *app) newDNSCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "Show how mihomo looks up and answers names; set the servers, the mode and fake-ip-filter",
		Long: "mihomo looks up names itself, with TUN on or off: for direct connections, for rules that match\n" +
			"addresses and for the nodes' own names. With TUN on it also answers the queries TUN catches on\n" +
			"this server and those of LAN devices that use it as their gateway and DNS.\n" +
			"  servers         whom mihomo asks: one group for Chinese domains, one for the others\n" +
			"  mode            how it answers the queries TUN catches: fake-ip or redir-host\n" +
			"  ipv6            whether names get IPv6 addresses (connections to them: tunlr ipv6)\n" +
			"  fake-ip-filter  your names that get real addresses in fake-ip mode\n" +
			"Whose DNS a subscription uses, tunlr's (the default) or its own, is set with\n" +
			"tunlr sub set <sub> --dns.\n\n" +
			"These commands edit the mixin file that now sets the key, or else mixin.d/" + setup.DNSMixinFile + ", and\n" +
			"publish the configuration after you confirm; editing the files yourself works the same.\n\n" +
			"Without a subcommand, show the DNS settings and where they come from; in a terminal, then\n" +
			"choose what to change.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			var s *setup.DNSStatus
			if err := a.spin("Composing the configuration", func() (err error) {
				s, err = a.installer().DNSStatus(cmd.Context())
				return err
			}); err != nil {
				return err
			}
			if a.interactive() {
				return a.chooseDNSChange(cmd, s)
			}
			return a.render(s, func() error {
				a.printDNS(s)
				return nil
			})
		},
	}
	cmd.AddCommand(a.withFullDiff(a.newDNSServersCommand()), a.withFullDiff(a.newDNSModeCommand()),
		a.withFullDiff(a.newDNSIPv6Command()), a.withFullDiff(a.newFakeIPFilterCommand()))
	return cmd
}

func (a *app) newDNSModeCommand() *cobra.Command {
	return &cobra.Command{
		Use:       "mode [fake-ip|redir-host|default]",
		Short:     "Set how mihomo answers the queries TUN catches: fake-ip, redir-host, or default",
		ValidArgs: dnsModes,
		Long: "The DNS mode only matters for the queries mihomo answers itself: those TUN catches on this\n" +
			"server and those of LAN devices that use it as their gateway and DNS.\n" +
			"  fake-ip     mihomo answers with fake addresses from dns.fake-ip-range (tunlr dns shows it) and\n" +
			"              maps them back to the names; proxied names need no real lookup. Some apps and devices\n" +
			"              dislike fake addresses, and devices keep them cached for a while after mihomo stops.\n" +
			"              The names in fake-ip-filter get real addresses (tunlr dns fake-ip-filter adds yours).\n" +
			"  redir-host  mihomo answers with real addresses (foreign names looked up through the proxies);\n" +
			"              rules by name keep working through the mapping and sniffing.\n" +
			"  default     the configuration decides again: tunlr's fallbacks (fake-ip), or the\n" +
			"              subscription's dns with tunlr sub set <sub> --dns sub.\n\n" +
			"The mode is written to the mixin file that now sets it, or else mixin.d/" + setup.DNSMixinFile + ";\n" +
			"default removes it from the shared mixins and those of the active subscription. The\n" +
			"configuration is published after you confirm. In a terminal, without a mode, choose it.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			switch {
			case len(args) == 1 && !slices.Contains(dnsModes, args[0]):
				return usageErrorf("the DNS mode must be fake-ip, redir-host or default, got %q", args[0])
			case len(args) == 1:
				return a.setDNSMode(ctx, args[0])
			case !a.interactive():
				return usageErrorf("dns mode needs fake-ip, redir-host or default without a terminal (tunlr dns --json shows the mode)")
			}
			var m *setup.DNSMode
			if err := a.spin("Composing the configuration", func() (err error) {
				m, err = a.installer().DNSMode(ctx)
				return err
			}); err != nil {
				return err
			}
			return a.chooseDNSMode(ctx, m)
		},
	}
}

// chooseDNSMode 让用户选择 DNS 模式，m 是现在的模式。
func (a *app) chooseDNSMode(ctx context.Context, m *setup.DNSMode) error {
	options, initial := dnsModeOptions(m)
	k, err := a.prompt.Choose(ctx, "Which DNS mode?", options, initial)
	if stop, err := a.cancelled(err); stop {
		return err
	}
	return a.setDNSMode(ctx, dnsModes[k])
}

// dnsModeOptions 是选择 DNS 模式的选项（与 dnsModes 对应）和光标的初始位置：模式由 mixin 写定时停在它上面，
// 否则停在“按配置”。
func dnsModeOptions(m *setup.DNSMode) ([]string, int) {
	pinned := m.Pinned()
	now := func(mode string) string {
		if pinned && m.Mode == mode {
			return " (now)"
		}
		return ""
	}
	decide := "Let the configuration decide"
	if !pinned {
		decide += " (now"
		if m.Mode != "" {
			decide += ": " + m.Mode
		}
		decide += ")"
	}
	initial := 2
	if i := slices.Index(dnsModes, m.Mode); pinned && i >= 0 {
		initial = i
	}
	return []string{
		"fake-ip: answer with fake addresses" + now(setup.DNSFakeIP),
		"redir-host: answer with real addresses" + now(setup.DNSRedirHost),
		decide,
	}, initial
}

// describeDNSSource 说明一项 DNS 设置来自哪里，如 "tunlr's fallback"、"set in 90-dns.yaml"、
// "set in home/30-x.yaml, only while home is active"。set 报告配置中有它。
func describeDNSSource(from string, fallback, set bool, sub string) string {
	switch {
	case fallback && from == config.FromTemplate && !set:
		return "not set"
	case fallback && from == config.FromTemplate:
		return "tunlr's template"
	case fallback && from == pipeline.ManagedDefaults:
		return "tunlr's fallback"
	case fallback:
		return "fallback in " + from
	case from == config.FromSubscription:
		return "from the subscription"
	case sub != "" && strings.HasPrefix(from, sub+"/"):
		return "set in " + from + ", only while " + sub + " is active"
	}
	return "set in " + from
}

// printDNS 输出 DNS 模式、问谁、你加进 fake-ip-filter 的条目，以及它们来自哪里。
func (a *app) printDNS(s *setup.DNSStatus) {
	w, m := a.stdout, s.Mode
	mode, src := m.Mode, describeDNSSource(m.From, m.Fallback, m.Mode != "", m.Subscription)
	if mode == "" {
		mode, src = setup.DNSRedirHost, "not set; mihomo's default"
	}
	fmt.Fprintf(w, "DNS mode:          %s %s\n", mode, a.style.dim("("+src+")"))
	a.printIPv6(w, &s.IPv6)
	if mode == setup.DNSFakeIP {
		ranges := s.FakeIPRange
		if ranges == "" {
			ranges = "198.18.0.1/16 " + a.style.dim("(mihomo's default)")
		}
		if s.IPv6.Answering() {
			r6 := s.FakeIPRange6
			if r6 == "" {
				r6 = a.style.warn("no IPv6 range (dns.fake-ip-range6)")
			}
			ranges += ", " + r6
		}
		fmt.Fprintf(w, "Fake addresses:    %s\n", ranges)
	}
	for _, l := range []struct {
		label string
		list  setup.DNSServerList
	}{{"Chinese domains:", s.Servers.China}, {"Other domains:", s.Servers.Other}} {
		servers := strings.Join(l.list.Servers, ", ")
		if servers == "" {
			servers = a.style.warn("none")
		}
		src := describeDNSSource(l.list.From, l.list.Fallback, len(l.list.Servers) > 0, m.Subscription)
		fmt.Fprintf(w, "%-19s%s %s\n", l.label, servers, a.style.dim("("+src+")"))
	}
	a.printFakeIPFilter(s.FakeIPFilter)
	if s.SubDNS != "" {
		fmt.Fprintf(w, "%s uses its own DNS where it sets one (tunlr.sub-dns in %s; tunlr sub set %s --dns tunlr undoes it).\n",
			m.Subscription, s.SubDNS, m.Subscription)
	}
	switch {
	case !s.Enabled:
		fmt.Fprintln(w, a.style.warn("mihomo's DNS is off (dns.enable is false), so these settings are not used."))
	case !s.Tun:
		fmt.Fprintln(w, a.style.dim("TUN is off, so the mode, IPv6 and fake-ip-filter are not used by this server's programs now; the servers are."))
	}
	if s.GatewayIPv6 != "" {
		fmt.Fprintf(w, "%s %s (tunlr dns ipv6 off).\n", a.style.warn("Warning:"), s.GatewayIPv6)
	}
}

// printFakeIPFilter 输出你加进 fake-ip-filter 的条目，不长时逐个列出。
func (a *app) printFakeIPFilter(entries []string) {
	switch list := strings.Join(entries, ", "); {
	case len(entries) == 0:
		fmt.Fprintln(a.stdout, "fake-ip-filter:    the configuration's list (tunlr dns fake-ip-filter adds names)")
	case len(list) <= 60:
		fmt.Fprintf(a.stdout, "fake-ip-filter:    yours added: %s\n", list)
	default:
		fmt.Fprintf(a.stdout, "fake-ip-filter:    %d names of yours added (tunlr dns fake-ip-filter)\n", len(entries))
	}
}

// chooseDNSChange 是终端中不带子命令的 dns（主菜单的“DNS”）：显示现状，让用户选择改模式、IPv6、问谁还是
// fake-ip-filter；模式选中后再选取值，IPv6 再分两步选流量和回答（chooseIPv6）。
func (a *app) chooseDNSChange(cmd *cobra.Command, s *setup.DNSStatus) error {
	a.printDNS(s)
	fmt.Fprintln(a.stdout)
	mode := "Mode: fake-ip or redir-host"
	if s.Mode.Mode != "" {
		mode += " (now " + s.Mode.Mode + ")"
	}
	yours := ""
	if n := len(s.FakeIPFilter); n > 0 {
		yours = fmt.Sprintf(" (%d of yours)", n)
	}
	ctx := cmd.Context()
	k, err := a.prompt.Choose(ctx, "Change which DNS setting?", []string{
		mode,
		"IPv6: traffic and answers (now " + describeIPv6(&s.IPv6) + ")",
		"Servers: who answers Chinese domains and other domains",
		"fake-ip-filter: names that get real addresses in fake-ip mode" + yours,
	}, 0)
	if stop, err := a.cancelled(err); stop {
		return err
	}
	switch k {
	case 0:
		return a.chooseDNSMode(ctx, &s.Mode)
	case 1:
		return a.chooseIPv6(ctx, &s.IPv6)
	case 2:
		return a.editDNSServers(cmd, nil)
	}
	return a.editFakeIPFilter(cmd, nil)
}

// publishedView 是 dns 的子命令在 --json 输出中共有的部分。
type publishedView struct {
	Plan     *planView        `json:"plan,omitempty"`
	Applied  bool             `json:"applied"`
	Revision *config.Revision `json:"revision,omitempty"`
}

// mixinEdit 是 dns 的子命令准备好的修改，由 publishEdit 发布。
type mixinEdit struct {
	// command 是命令（requireConfirmation 用），question 是发布前的确认，done 是完成后的一句话。
	command, question, done string
	changes                 []*setup.MixinChange
	plan                    *pipeline.Plan
	// save 写入文件并发布 plan（为 nil 时只写入）。
	save func(plan *pipeline.Plan) (*config.Revision, error)
	// notes 在预览之后输出提示，after 在发布之后检查结果；都可以为 nil。
	notes func()
	after func() error
	// view 是 --json 输出，pv 是其中的 publishedView。
	view any
	pv   *publishedView
}

// publishEdit 是 dns 的子命令改 mixin 文件的流程：配置不变时直接保存，否则预览、列出要改的文件，确认后与
// 文件一起发布。发布失败时文件恢复原状，配置由流水线回滚。
func (a *app) publishEdit(ctx context.Context, e mixinEdit) error {
	if !e.plan.Changed() {
		if a.opts.DryRun {
			return a.render(e.view, func() error {
				_, err := fmt.Fprintln(a.stdout, "Dry run: nothing changed (the configuration stays the same).")
				return err
			})
		}
		if _, err := e.save(nil); err != nil {
			return err
		}
		return a.render(e.view, func() error {
			a.printMixinChanges(e.changes, true)
			if e.notes != nil {
				e.notes()
			}
			_, err := fmt.Fprintln(a.stdout, e.done)
			return err
		})
	}
	pv := newPlanView(e.plan, false)
	e.pv.Plan = &pv
	if !a.opts.JSON {
		a.printPlan(pv)
		fmt.Fprintln(a.stdout)
		a.printMixinChanges(e.changes, false)
		if e.notes != nil {
			e.notes()
		}
	}
	if err := checkExit(e.plan, false); err != nil || a.opts.DryRun {
		if a.opts.JSON {
			if rerr := a.render(e.view, nil); rerr != nil {
				return rerr
			}
		} else if err != nil {
			fmt.Fprintln(a.stdout, "\nNot changed.")
		} else {
			fmt.Fprintln(a.stdout, "\nDry run: nothing changed.")
		}
		return err
	}
	if err := a.requireConfirmation(e.command); err != nil {
		return err
	}
	question := e.question
	if e.plan.Current == nil {
		question = "Save the mixin files?"
	}
	if ok, err := a.ask(ctx, question, "", "Not changed."); !ok {
		return err
	}
	err := a.spin("Publishing the configuration", func() (err error) {
		e.pv.Revision, err = e.save(e.plan)
		return err
	})
	if errors.Is(err, core.ErrRolledBack) {
		return fmt.Errorf("%w; the previous configuration and mixin files are back in place", err)
	}
	if err != nil {
		return err
	}
	e.pv.Applied = e.pv.Revision != nil
	finish := func() error {
		return a.render(e.view, func() error {
			fmt.Fprintf(a.stdout, "\n%s\n", a.style.good(e.done))
			switch {
			case e.pv.Revision != nil:
				fmt.Fprintf(a.stdout, "Published revision %d.\n", e.pv.Revision.Number)
			case e.plan.Current == nil:
				fmt.Fprintln(a.stdout, "No configuration has been published yet; it takes effect with tunlr config apply.")
			}
			return nil
		})
	}
	if e.after == nil || e.pv.Revision == nil {
		return finish()
	}
	if a.opts.JSON {
		err := e.after()
		if rerr := finish(); rerr != nil {
			return rerr
		}
		if err != nil {
			return renderedError{err}
		}
		return nil
	}
	if err := finish(); err != nil {
		return err
	}
	return e.after()
}

// dnsModeView 是 dns mode 在 --json 输出中的形式。
type dnsModeView struct {
	*setup.DNSModeEdit
	publishedView
}

// setDNSMode 是 dns mode fake-ip|redir-host|default 的流程：模式写进现在决定它的 mixin（没有时 DNSMixinFile），
// default 从写了它的 mixin 中删除；已经是这样时只报告，否则同 publishEdit。
func (a *app) setDNSMode(ctx context.Context, mode string) error {
	command := "dns mode " + mode
	if mode == dnsModeDefault {
		mode = ""
	}
	inst := a.installer()
	var edit *setup.DNSModeEdit
	plan, err := a.plan(func() (p *pipeline.Plan, err error) {
		edit, p, err = inst.PlanDNSMode(ctx, mode)
		return p, err
	})
	if err != nil {
		return err
	}
	v := &dnsModeView{DNSModeEdit: edit}
	if len(edit.Changes) == 0 {
		return a.render(v, func() error {
			_, err := fmt.Fprintln(a.stdout, "Already so; nothing to change.")
			return err
		})
	}
	done := "The configuration decides the DNS mode again."
	if mode != "" {
		done = "mihomo answers DNS queries in " + mode + " mode now."
	}
	return a.publishEdit(ctx, mixinEdit{
		command: command, question: "Publish the configuration with this DNS mode?", done: done,
		changes: edit.Changes, plan: plan, view: v, pv: &v.publishedView,
		save:  func(p *pipeline.Plan) (*config.Revision, error) { return inst.SaveDNSMode(ctx, edit, p) },
		notes: func() { a.printDNSModeNotes(edit) },
	})
}

// printDNSModeNotes 提醒改为 redir-host 时，采用的订阅的 DNS 缺少它需要的设置。
func (a *app) printDNSModeNotes(edit *setup.DNSModeEdit) {
	if len(edit.Lacks) == 0 {
		return
	}
	sub := edit.Before.Subscription
	fmt.Fprintf(a.stdout, "\n%s %s uses its own DNS (tunlr.sub-dns in %s), which has no %s. In redir-host mode\n"+
		"devices get the addresses it answers: foreign names may get polluted addresses, Chinese names far-away\n"+
		"ones. tunlr sub set %s --dns tunlr uses tunlr's DNS instead, which has both.\n",
		a.style.warn("Warning:"), sub, edit.SubDNS, strings.Join(edit.Lacks, " or "), sub)
}

// dnsServersView 是 dns servers 在 --json 输出中的形式。
type dnsServersView struct {
	*setup.DNSServersEdit
	publishedView
	// Checks 是发布后经内核解析的结果，内核没有运行或 DNS 没有开启时为空；Reverted 是解析不通、放回原来的服务器
	// 后发布的修订。
	Checks   []setup.DNSCheck `json:"checks,omitempty"`
	Reverted *config.Revision `json:"reverted,omitempty"`
}

func (a *app) newDNSServersCommand() *cobra.Command {
	var china, other []string
	var reset bool
	cmd := &cobra.Command{
		Use:   "servers",
		Short: "Set the DNS servers for Chinese domains (and the nodes' names) and for the others",
		Long: "mihomo asks two groups of DNS servers:\n" +
			"  Chinese domains  (geosite:cn) asked directly, not through the proxies, so the answers are\n" +
			"                   nearby; they also look up the nodes' own names. Written to the geosite:cn\n" +
			"                   entry of dns.nameserver-policy (with #DIRECT) and to\n" +
			"                   dns.proxy-server-nameserver.\n" +
			"  Other domains    asked by the rules, usually through the proxies, so the answers are not\n" +
			"                   polluted. Written to dns.nameserver.\n" +
			"A server is an IP (port optional), system, or an address starting with udp://, tcp://,\n" +
			"tls:// (DoT), https:// (DoH), quic:// (DoQ), h3:// or dhcp://<interface>; mix them freely.\n\n" +
			"They are written to the mixin that now sets them, or else mixin.d/" + setup.DNSMixinFile + ", and the\n" +
			"configuration is published after you confirm. Then mihomo looks up www.baidu.com and\n" +
			"www.google.com; if that fails you are asked whether to put the previous servers back\n" +
			"(without a terminal the command exits with code 3).\n\n" +
			"In a terminal, without flags, edit both lists in text boxes prefilled with the servers in\n" +
			"use (tunlr's defaults where there are none), one per line: Enter starts a new line, Tab is\n" +
			"done, Ctrl+E opens your editor and Esc cancels. An emptied box removes your servers, so the\n" +
			"configuration's are used. Without a terminal use --cn, --other or --reset.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			changed := cmd.Flags().Changed
			ch := setup.DNSServersChange{Reset: reset}
			switch {
			case reset && (changed("cn") || changed("other")):
				return usageErrorf("--reset removes your servers from both lists; it does not go with --cn or --other")
			case reset:
			case changed("cn") || changed("other"):
				var err error
				if changed("cn") {
					if ch.China, err = parseDNSServerFlags("--cn", china); err != nil {
						return err
					}
				}
				if changed("other") {
					if ch.Other, err = parseDNSServerFlags("--other", other); err != nil {
						return err
					}
				}
			case !a.interactive():
				return usageErrorf("dns servers needs --cn, --other or --reset without a terminal (tunlr dns --json lists the servers)")
			default:
				return a.editDNSServers(cmd, nil)
			}
			return a.editDNSServers(cmd, &ch)
		},
	}
	cmd.Flags().StringArrayVar(&china, "cn", nil, "a server for Chinese domains (repeatable; replaces the list, \"\" removes yours)")
	cmd.Flags().StringArrayVar(&other, "other", nil, "a server for the other domains (repeatable; replaces the list, \"\" removes yours)")
	cmd.Flags().BoolVar(&reset, "reset", false, "remove your servers from both lists")
	return cmd
}

// parseDNSServerFlags 整理 --cn、--other 给出的服务器；都为空时返回空的列表（删除你的设置）。
func parseDNSServerFlags(flag string, values []string) ([]string, error) {
	servers, err := setup.ParseDNSServers(strings.Join(values, "\n"))
	if err != nil {
		return nil, usageErrorf("%s %v", flag, err)
	}
	if servers == nil {
		servers = []string{}
	}
	return servers, nil
}

// editDNSServers 是 dns servers 的流程：ch 为 nil 时在文本框中编辑，否则按参数修改；然后同 publishEdit，发布后
// 经内核检查（checkDNSServers）。
func (a *app) editDNSServers(cmd *cobra.Command, ch *setup.DNSServersChange) error {
	ctx := cmd.Context()
	inst := a.installer()
	if ch == nil {
		var cur *setup.DNSServers
		if err := a.spin("Composing the configuration", func() (err error) {
			cur, err = inst.DNSServers(ctx)
			return err
		}); err != nil {
			return err
		}
		ch = &setup.DNSServersChange{}
		var err error
		ch.China, err = a.askDNSServers(ctx, "Servers for Chinese domains, one per line",
			"Asked directly, not through the proxies; they also look up the nodes' names.", cur.China)
		if stop, err := a.cancelled(err); stop {
			return err
		}
		ch.Other, err = a.askDNSServers(ctx, "Servers for the other domains, one per line",
			"Asked by the rules, usually through the proxies.", cur.Other)
		if stop, err := a.cancelled(err); stop {
			return err
		}
	}
	var edit *setup.DNSServersEdit
	plan, err := a.plan(func() (p *pipeline.Plan, err error) {
		edit, p, err = inst.PlanDNSServers(ctx, *ch)
		return p, err
	})
	if err != nil {
		return err
	}
	v := &dnsServersView{DNSServersEdit: edit}
	if len(edit.Changes) == 0 {
		return a.render(v, func() error {
			_, err := fmt.Fprintln(a.stdout, "Already so; nothing to change.")
			return err
		})
	}
	return a.publishEdit(ctx, mixinEdit{
		command: "dns servers", question: "Publish the configuration with these DNS servers?", done: "The DNS servers are changed.",
		changes: edit.Changes, plan: plan, view: v, pv: &v.publishedView,
		save:  func(p *pipeline.Plan) (*config.Revision, error) { return inst.SaveDNSServers(ctx, edit, p) },
		after: func() error { return a.checkDNSServers(ctx, v) },
	})
}

// askDNSServers 在文本框中编辑一组服务器，预填现在用的，没有时 tunlr 的默认值。返回 nil 表示不改（与现在用的
// 相同），清空时返回空的列表（删除你的设置）。
func (a *app) askDNSServers(ctx context.Context, title, desc string, l setup.DNSServerList) ([]string, error) {
	value := l.Servers
	switch {
	case len(l.Servers) == 0:
		value = l.Default
		desc += "\nNone are set now; filled in with tunlr's defaults."
	case !slices.Equal(l.Servers, l.Default):
		desc += "\ntunlr's defaults: " + strings.Join(l.Default, ", ") + "."
	}
	desc += "\nEmpty the box to remove yours (the configuration's are used)."
	text, err := a.prompt.Text(ctx, title, desc, strings.Join(value, "\n"), func(s string) error {
		_, err := setup.ParseDNSServers(s)
		return err
	})
	if err != nil {
		return nil, err
	}
	servers, _ := setup.ParseDNSServers(text)
	switch {
	case len(servers) == 0:
		return []string{}, nil
	case slices.Equal(servers, l.Servers):
		return nil, nil
	}
	return servers, nil
}

// checkDNSServers 在发布之后经内核解析一个中国的域名和一个境外的域名。解析不通时在终端中问是否放回原来的服务器，
// 否则以检查未通过退出（退出码 3）；内核没有运行或 DNS 没有开启时不检查。
func (a *app) checkDNSServers(ctx context.Context, v *dnsServersView) error {
	inst := a.installer()
	var checks []setup.DNSCheck
	err := a.spin("Looking up names through mihomo", func() (err error) {
		checks, err = inst.CheckDNS(ctx)
		return err
	})
	switch {
	case errors.Is(err, mihomo.ErrNotRunning):
		if !a.opts.JSON {
			fmt.Fprintln(a.stdout, "mihomo is not running, so the servers were not checked.")
		}
		return nil
	case errors.Is(err, setup.ErrDNSOff):
		if !a.opts.JSON {
			fmt.Fprintf(a.stdout, "%s, so the servers were not checked.\n", setup.ErrDNSOff.Error())
		}
		return nil
	case err != nil:
		return err
	}
	v.Checks = checks
	var failed []string
	for _, c := range checks {
		if c.Error != "" {
			failed = append(failed, c.Name)
		}
	}
	if !a.opts.JSON {
		fmt.Fprintln(a.stdout, "Looked up through mihomo:")
		tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
		for _, c := range checks {
			kind := "Chinese domain"
			if c.Kind != "china" {
				kind = "other domain"
			}
			result := a.style.good("ok") + " " + strings.Join(c.Addresses[:min(len(c.Addresses), 2)], ", ")
			if c.Error != "" {
				result = a.style.bad("failed") + " " + c.Error
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", c.Name, kind, result)
		}
		tw.Flush()
	}
	if len(failed) == 0 {
		return nil
	}
	err = fmt.Errorf("%w: mihomo could not look up %s with these servers", core.ErrCheckFailed, strings.Join(failed, ", "))
	if !a.interactive() {
		return err
	}
	ok, err := a.confirm(ctx, "Put the previous servers back?", "Otherwise the new ones stay; tunlr dns servers changes them again.")
	if err != nil || !ok {
		return err
	}
	if err := a.spin("Putting the previous servers back", func() (err error) {
		v.Reverted, err = inst.RevertDNSServers(ctx, v.DNSServersEdit)
		return err
	}); err != nil {
		return err
	}
	msg := "The previous servers are back."
	if v.Reverted != nil {
		msg = fmt.Sprintf("The previous servers are back (revision %d).", v.Reverted.Number)
	}
	fmt.Fprintln(a.stdout, msg)
	return renderedError{fmt.Errorf("%w: the new DNS servers did not work", core.ErrRolledBack)}
}

// fakeIPFilterChange 是 dns fake-ip-filter 的参数，nil 表示在文本框中编辑。
type fakeIPFilterChange struct {
	add, remove []string
	reset       bool
}

// fakeIPFilterView 是 dns fake-ip-filter 在 --json 输出中的形式。
type fakeIPFilterView struct {
	*setup.FakeIPFilterEdit
	publishedView
}

func (a *app) newFakeIPFilterCommand() *cobra.Command {
	var ch fakeIPFilterChange
	cmd := &cobra.Command{
		Use:   "fake-ip-filter",
		Short: "Add your own names that get real addresses in fake-ip mode",
		Long: "In fake-ip mode mihomo answers with fake addresses, except for the names in dns.fake-ip-filter:\n" +
			"LAN names, time servers, STUN, game consoles and such, given by tunlr's fallbacks or the\n" +
			"subscription (tunlr config shows tunlr's). Add your own here: one name or pattern per line, such\n" +
			"as +.lan, *.example.com or nas.example.com. They are kept in mixin.d/" + setup.DNSMixinFile + " as\n" +
			"tunlr.fake-ip-filter.append and added to whichever list is in use, so changes to that list still\n" +
			"apply. To drop names from that list, write tunlr.fake-ip-filter.delete in a mixin.\n\n" +
			"In a terminal, without flags, edit your names in a text box: Enter starts a new line, Tab is done,\n" +
			"Ctrl+E opens your editor and Esc cancels. The diff is shown and the configuration is published\n" +
			"after you confirm. Without a terminal use --add, --remove or --reset.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case ch.reset && (len(ch.add) > 0 || len(ch.remove) > 0):
				return usageErrorf("--reset removes all your names; it does not go with --add or --remove")
			case ch.reset || len(ch.add) > 0 || len(ch.remove) > 0:
				return a.editFakeIPFilter(cmd, &ch)
			case !a.interactive():
				return usageErrorf("dns fake-ip-filter needs --add, --remove or --reset without a terminal (tunlr dns --json lists your names)")
			}
			return a.editFakeIPFilter(cmd, nil)
		},
	}
	cmd.Flags().StringArrayVar(&ch.add, "add", nil, "add a name or pattern (repeatable)")
	cmd.Flags().StringArrayVar(&ch.remove, "remove", nil, "remove one of your names (repeatable)")
	cmd.Flags().BoolVar(&ch.reset, "reset", false, "remove all your names")
	return cmd
}

// apply 返回按参数修改后你加的条目：cur 是现在的条目和它们加入的列表。
func (ch *fakeIPFilterChange) apply(cur *setup.FakeIPFilter) ([]string, error) {
	if ch.reset {
		return nil, nil
	}
	entries := slices.Clone(cur.Entries)
	for _, r := range ch.remove {
		r = strings.TrimSpace(r)
		switch i := slices.Index(entries, r); {
		case i >= 0:
			entries = slices.Delete(entries, i, i+1)
		case slices.Contains(cur.Base, r):
			return nil, usageErrorf("%q comes from the configuration's list, not from your names; drop it with tunlr.fake-ip-filter.delete in a mixin (tunlr config edit)", r)
		default:
			return nil, usageErrorf("%q is not one of your names (tunlr dns lists them)", r)
		}
	}
	add, err := setup.ParseFakeIPFilter(strings.Join(ch.add, "\n"))
	if err != nil {
		return nil, usageErrorf("--add %v", err)
	}
	for _, s := range add {
		if !slices.Contains(entries, s) {
			entries = append(entries, s)
		}
	}
	return entries, nil
}

// editFakeIPFilter 是 dns fake-ip-filter 的流程：ch 为 nil 时在文本框中编辑你的条目，否则按参数修改；然后同
// publishEdit。
func (a *app) editFakeIPFilter(cmd *cobra.Command, ch *fakeIPFilterChange) error {
	ctx := cmd.Context()
	inst := a.installer()
	var cur *setup.FakeIPFilter
	if err := a.spin("Composing the configuration", func() (err error) {
		cur, err = inst.FakeIPFilter(ctx)
		return err
	}); err != nil {
		return err
	}
	var entries []string
	if ch != nil {
		var err error
		if entries, err = ch.apply(cur); err != nil {
			return err
		}
	} else {
		desc := "Only your own names; tunlr adds them to the configuration's list."
		if n := len(cur.Base); n > 0 {
			desc = fmt.Sprintf("Only your own names; the configuration's list already has %d, such as %s.",
				n, strings.Join(cur.Base[:min(n, 3)], ", "))
		}
		text, err := a.prompt.Text(ctx, "Names that get real addresses in fake-ip mode, one per line", desc,
			strings.Join(cur.Entries, "\n"), func(s string) error {
				_, err := setup.ParseFakeIPFilter(s)
				return err
			})
		if stop, err := a.cancelled(err); stop {
			return err
		}
		entries, _ = setup.ParseFakeIPFilter(text)
	}

	var edit *setup.FakeIPFilterEdit
	plan, err := a.plan(func() (p *pipeline.Plan, err error) {
		edit, p, err = inst.PlanFakeIPFilter(ctx, entries)
		return p, err
	})
	if err != nil {
		return err
	}
	v := &fakeIPFilterView{FakeIPFilterEdit: edit}
	if !a.opts.JSON && len(edit.Duplicates) > 0 {
		fmt.Fprintf(a.stdout, "Already in the configuration's list, not added: %s\n", strings.Join(edit.Duplicates, ", "))
	}
	if !edit.Changed() {
		return a.render(v, func() error {
			_, err := fmt.Fprintln(a.stdout, "Already so; nothing to change.")
			return err
		})
	}
	done := "You have no names of your own in fake-ip-filter now."
	if len(edit.Entries) > 0 {
		done = "Your names in fake-ip-filter: " + strings.Join(edit.Entries, ", ") + "."
	}
	return a.publishEdit(ctx, mixinEdit{
		command: "dns fake-ip-filter", question: "Publish the configuration with these names?", done: done,
		changes: edit.Changes, plan: plan, view: v, pv: &v.publishedView,
		save:  func(p *pipeline.Plan) (*config.Revision, error) { return inst.SaveFakeIPFilter(ctx, edit, p) },
		notes: func() { a.printFakeIPFilterNotes(edit, plan) },
	})
}

// printFakeIPFilterNotes 提醒没有生效的条目，以及 DNS 模式不是 fake-ip 时 fake-ip-filter 不起作用。
func (a *app) printFakeIPFilterNotes(edit *setup.FakeIPFilterEdit, plan *pipeline.Plan) {
	if len(edit.Lost) > 0 {
		by := "a mixin applied later replaces dns.fake-ip-filter"
		if edit.LostTo != "" {
			by = edit.LostTo + " replaces dns.fake-ip-filter after " + setup.DNSMixinFile +
				"; use tunlr.fake-ip-filter.append there instead"
		}
		fmt.Fprintf(a.stdout, "\n%s %s: %s.\n", a.style.warn("Not in the configuration:"), strings.Join(edit.Lost, ", "), by)
	}
	if mode := config.DNSMode(plan.Config); mode != "" && mode != setup.DNSFakeIP {
		fmt.Fprintf(a.stdout, "\nNote: the DNS mode is %s; fake-ip-filter only matters in fake-ip mode.\n", mode)
	}
}
