package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// setSubDNS 是 sub set <sub> --dns tunlr|subscription：订阅用谁的 DNS 和嗅探（写进它的 mixin），订阅启用时随后
// 发布。from 为空时（菜单的“Change a sub”）让用户选择。
func (a *app) setSubDNS(cmd *cobra.Command, sub, from string) error {
	w, err := a.writeSubDNS(cmd, sub, from)
	if err != nil || w == nil {
		return err
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
		return nil, usageErrorf("--dns takes %s (tunlr's DNS and sniffer) or %s (the subscription's, the default), got %q",
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
			"The subscription's (default): the dns and sniffer it sets, tunlr's where it sets none",
			"tunlr's: Chinese names asked directly, others by the rules; sniffing on",
		}
		initial := 0
		if now == setup.DNSFromTunlr {
			initial = 1
		}
		k, err := a.prompt.Choose(ctx, fmt.Sprintf("Which DNS should %q use?", sub), options, initial)
		if stop, err := a.cancelled(err); stop {
			return nil, err
		}
		from = []string{setup.DNSFromSubscription, setup.DNSFromTunlr}[k]
	}
	if err := a.requireYes("sub set --dns"); err != nil {
		return nil, err
	}
	w, err := inst.SetSubscriptionDNS(ctx, sub, from)
	if err != nil {
		return nil, err
	}
	if !a.opts.JSON {
		_, statErr := os.Stat(w.File)
		switch {
		case from == setup.DNSFromTunlr:
			fmt.Fprintf(a.stdout, "Wrote %s: %q uses tunlr's DNS and sniffer (see tunlr config).\n", w.File, sub)
		case errors.Is(statErr, fs.ErrNotExist):
			fmt.Fprintf(a.stdout, "Removed %s: %q uses the DNS and sniffer it sets, or else tunlr's.\n", w.File, sub)
		default:
			fmt.Fprintf(a.stdout, "Updated %s: %q uses the DNS and sniffer it sets, or else tunlr's.\n", w.File, sub)
		}
	}
	return w, nil
}

// dnsModeView 是 tun dns 在 --json 输出中的形式。
type dnsModeView struct {
	// Mode 是开关：fake-ip、redir-host，为空时按配置。
	Mode string `json:"mode"`
	// Published 是生效的配置中的 dns.enhanced-mode，没有写时为空。
	Published string `json:"published,omitempty"`
	// FakeIPFilter 是你加进 dns.fake-ip-filter 的条目（只在显示状态时）。
	FakeIPFilter []string         `json:"fake_ip_filter,omitempty"`
	Plan         *planView        `json:"plan,omitempty"`
	Applied      bool             `json:"applied"`
	Revision     *config.Revision `json:"revision,omitempty"`
}

const dnsModeDefault = "default"

func (a *app) newTunDNSCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "Show how mihomo answers the DNS queries it takes over; set fake-ip or redir-host, edit fake-ip-filter",
		Long: "The DNS mode only matters for the queries mihomo answers itself: those TUN catches on this\n" +
			"server and those of LAN devices that use it as their gateway and DNS.\n" +
			"  fake-ip     mihomo answers with addresses from 198.18.0.0/16 and maps them back to the names;\n" +
			"              proxied names need no real lookup. Some apps and devices dislike fake addresses,\n" +
			"              and devices keep them cached for a while after mihomo stops. The names in\n" +
			"              fake-ip-filter get real addresses (tunlr tun dns fake-ip-filter adds yours).\n" +
			"  redir-host  mihomo answers with real addresses (foreign names looked up through the proxies);\n" +
			"              rules by name keep working through the mapping and sniffing.\n" +
			"Without a choice the configuration decides: the subscription's dns, or else tunlr's fallbacks\n" +
			"(fake-ip). Once set, mixins may not set dns.enhanced-mode; default lets the configuration decide again.\n\n" +
			"Without a subcommand, show the DNS mode and your fake-ip-filter names; in a terminal, then choose\n" +
			"what to change.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.installer().TunStatus(cmd.Context())
			if err != nil {
				return err
			}
			if a.interactive() {
				return a.chooseDNSChange(cmd, s)
			}
			return a.render(dnsModeView{Mode: s.DNSMode, Published: s.PublishedDNSMode, FakeIPFilter: s.FakeIPFilter}, func() error {
				a.printDNSMode(s)
				return nil
			})
		},
	}
	cmd.AddCommand(a.withFullDiff(a.newFakeIPFilterCommand()))
	for _, m := range []struct{ mode, short string }{
		{state.DNSFakeIP, "Answer with fake addresses (198.18.0.0/16) whatever the configuration says"},
		{state.DNSRedirHost, "Answer with real addresses whatever the configuration says"},
		{dnsModeDefault, "Let the configuration decide again (the subscription's dns, or else fake-ip)"},
	} {
		mode := m.mode
		cmd.AddCommand(a.withFullDiff(&cobra.Command{
			Use:   mode,
			Short: m.short,
			Args:  usageArgs(cobra.NoArgs),
			RunE:  func(cmd *cobra.Command, args []string) error { return a.setDNSMode(cmd.Context(), mode) },
		}))
	}
	return cmd
}

// printDNSMode 输出 DNS 模式：开关，没有设时是生效的配置写的。
func (a *app) printDNSMode(s *setup.TunStatus) {
	if s.DNSMode != "" {
		fmt.Fprintf(a.stdout, "DNS mode:          %s (set by tunlr tun dns; default lets the configuration decide)\n", s.DNSMode)
		return
	}
	from := "from the configuration"
	if s.PublishedDNSMode != "" {
		from = s.PublishedDNSMode + ", " + from
	}
	fmt.Fprintf(a.stdout, "DNS mode:          %s (tunlr tun dns fake-ip|redir-host sets it)\n", from)
	a.printFakeIPFilter(s.FakeIPFilter)
}

// printFakeIPFilter 输出你加进 fake-ip-filter 的条目，不长时逐个列出。
func (a *app) printFakeIPFilter(entries []string) {
	switch list := strings.Join(entries, ", "); {
	case len(entries) == 0:
		fmt.Fprintln(a.stdout, "fake-ip-filter:    the configuration's list (tunlr tun dns fake-ip-filter adds names)")
	case len(list) <= 60:
		fmt.Fprintf(a.stdout, "fake-ip-filter:    yours added: %s\n", list)
	default:
		fmt.Fprintf(a.stdout, "fake-ip-filter:    %d names of yours added (tunlr tun dns fake-ip-filter)\n", len(entries))
	}
}

// chooseDNSChange 是终端中不带子命令的 tun dns（主菜单的“DNS”）：显示现状，让用户选择改模式还是 fake-ip-filter。
func (a *app) chooseDNSChange(cmd *cobra.Command, s *setup.TunStatus) error {
	a.printDNSMode(s)
	fmt.Fprintln(a.stdout)
	now := func(on bool) string {
		if on {
			return " (now)"
		}
		return ""
	}
	yours := ""
	if n := len(s.FakeIPFilter); n > 0 {
		yours = fmt.Sprintf(" (%d of yours)", n)
	}
	options := []string{
		"fake-ip: answer with fake addresses" + now(s.DNSMode == state.DNSFakeIP),
		"redir-host: answer with real addresses" + now(s.DNSMode == state.DNSRedirHost),
		"Let the configuration decide" + now(s.DNSMode == ""),
		"fake-ip-filter: names that get real addresses in fake-ip mode" + yours,
	}
	modes := []string{state.DNSFakeIP, state.DNSRedirHost, dnsModeDefault}
	initial := slices.Index(modes, cmp.Or(s.DNSMode, dnsModeDefault))
	k, err := a.prompt.Choose(cmd.Context(), "Change which DNS setting?", options, initial)
	if stop, err := a.cancelled(err); stop {
		return err
	}
	if k < len(modes) {
		return a.setDNSMode(cmd.Context(), modes[k])
	}
	return a.editFakeIPFilter(cmd, nil)
}

// fakeIPFilterChange 是 tun dns fake-ip-filter 的参数，nil 表示在文本框中编辑。
type fakeIPFilterChange struct {
	add, remove []string
	reset       bool
}

// fakeIPFilterView 是 tun dns fake-ip-filter 在 --json 输出中的形式。
type fakeIPFilterView struct {
	*setup.FakeIPFilterEdit
	Plan     *planView        `json:"plan,omitempty"`
	Applied  bool             `json:"applied"`
	Revision *config.Revision `json:"revision,omitempty"`
}

func (a *app) newFakeIPFilterCommand() *cobra.Command {
	var ch fakeIPFilterChange
	cmd := &cobra.Command{
		Use:   "fake-ip-filter",
		Short: "Add your own names that get real addresses in fake-ip mode",
		Long: "In fake-ip mode mihomo answers with fake addresses, except for the names in dns.fake-ip-filter:\n" +
			"LAN names, time servers, STUN, game consoles and such, given by the subscription or tunlr's\n" +
			"fallbacks (tunlr config shows tunlr's). Add your own here: one name or pattern per line, such as\n" +
			"+.lan, *.example.com or nas.example.com. They are kept in mixin.d/" + setup.FakeIPFilterMixinFile + " as\n" +
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
				return usageErrorf("tun dns fake-ip-filter needs --add, --remove or --reset without a terminal (tunlr tun dns --json lists your names)")
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
			return nil, usageErrorf("%q is not one of your names (tunlr tun dns lists them)", r)
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

// editFakeIPFilter 是 tun dns fake-ip-filter 的流程：ch 为 nil 时在文本框中编辑你的条目，否则按参数修改；然后同
// setDNSMode：没有变化时只报告，配置不变时直接保存，否则预览、确认后与 mixin 文件一起发布。
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
	v := fakeIPFilterView{FakeIPFilterEdit: edit}
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
	if !plan.Changed() {
		if a.opts.DryRun {
			return a.render(v, func() error {
				_, err := fmt.Fprintln(a.stdout, "Dry run: nothing changed (the configuration stays the same).")
				return err
			})
		}
		if _, err := inst.SaveFakeIPFilter(ctx, edit, nil); err != nil {
			return err
		}
		return a.render(v, func() error {
			_, err := fmt.Fprintln(a.stdout, done)
			return err
		})
	}
	pv := newPlanView(plan, false)
	v.Plan = &pv
	if !a.opts.JSON {
		a.printPlan(pv)
		a.printFakeIPFilterNotes(edit, plan)
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
	if err := a.requireConfirmation("tun dns fake-ip-filter"); err != nil {
		return err
	}
	question := "Publish the configuration with these names?"
	if plan.Current == nil {
		question = "Save these names?"
	}
	if ok, err := a.ask(ctx, question, "", "Not changed."); !ok {
		return err
	}
	err = a.spin("Publishing the configuration", func() (err error) {
		v.Revision, err = inst.SaveFakeIPFilter(ctx, edit, plan)
		return err
	})
	if errors.Is(err, core.ErrRolledBack) {
		return fmt.Errorf("%w; the previous configuration and %s are back in place", err, edit.File)
	}
	if err != nil {
		return err
	}
	v.Applied = v.Revision != nil
	return a.render(v, func() error {
		fmt.Fprintf(a.stdout, "\n%s\n", a.style.good(done))
		switch {
		case v.Revision != nil:
			fmt.Fprintf(a.stdout, "Published revision %d.\n", v.Revision.Number)
		case plan.Current == nil:
			fmt.Fprintln(a.stdout, "No configuration has been published yet; they take effect with tunlr config apply.")
		}
		return nil
	})
}

// printFakeIPFilterNotes 提醒没有生效的条目，以及 DNS 模式不是 fake-ip 时 fake-ip-filter 不起作用。
func (a *app) printFakeIPFilterNotes(edit *setup.FakeIPFilterEdit, plan *pipeline.Plan) {
	if len(edit.Lost) > 0 {
		by := "a mixin applied later replaces dns.fake-ip-filter"
		if edit.LostTo != "" {
			by = edit.LostTo + " replaces dns.fake-ip-filter after " + setup.FakeIPFilterMixinFile +
				"; use tunlr.fake-ip-filter.append there instead"
		}
		fmt.Fprintf(a.stdout, "\n%s %s: %s.\n", a.style.warn("Not in the configuration:"), strings.Join(edit.Lost, ", "), by)
	}
	if mode := config.DNSMode(plan.Config); mode != "" && mode != state.DNSFakeIP {
		fmt.Fprintf(a.stdout, "\nNote: the DNS mode is %s; fake-ip-filter only matters in fake-ip mode.\n", mode)
	}
}

// setDNSMode 是 tun dns fake-ip|redir-host|default 的流程：已经是目标状态时只报告；配置没有变化时直接保存，
// 否则预览、确认后与开关一起发布。
func (a *app) setDNSMode(ctx context.Context, mode string) error {
	command := "tun dns " + mode
	if mode == dnsModeDefault {
		mode = ""
	}
	inst := a.installer()
	var before string
	plan, err := a.plan(func() (p *pipeline.Plan, err error) {
		before, p, err = inst.PlanDNSMode(ctx, mode)
		return p, err
	})
	if err != nil {
		return err
	}
	v := dnsModeView{Mode: mode}
	done := "The configuration decides the DNS mode again."
	if mode != "" {
		done = "mihomo answers DNS queries in " + mode + " mode now."
	}
	if before == mode && !plan.Changed() {
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
		if _, err := inst.SaveDNSMode(ctx, mode, nil); err != nil {
			return err
		}
		return a.render(v, func() error {
			_, err := fmt.Fprintln(a.stdout, done)
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
	if ok, err := a.ask(ctx, "Publish the configuration with this DNS mode?", "", "Not changed."); !ok {
		return err
	}
	err = a.spin("Publishing the configuration", func() (err error) {
		v.Revision, err = inst.SaveDNSMode(ctx, mode, plan)
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
