package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
)

// planView 是 pipeline.Plan 在 --json 输出中的形式。
type planView struct {
	Kind         string   `json:"kind"`
	Changed      bool     `json:"changed"`
	FirstPublish bool     `json:"first_publish"`
	Subscription string   `json:"subscription,omitempty"`
	Dropped      []string `json:"dropped,omitempty"`
	// Ignored 是订阅中写了、mixin 要求不采用的顶层键（tunlr.ignore-from-subscription）。
	Ignored   []config.IgnoredKey `json:"ignored,omitempty"`
	Mixins    []string            `json:"mixins,omitempty"`
	Overrides []config.Override   `json:"overrides,omitempty"`
	// Rewrites 是受管 mixin 在合成的最后改写的值，如经镜像的 geox-url。
	Rewrites []config.Rewrite `json:"rewrites,omitempty"`
	// ProxyFilter 是节点筛选（tunlr.proxy-filter）的结果，没有设置时省略。
	ProxyFilter *config.ProxyFilterResult `json:"proxy_filter,omitempty"`
	// GeoSources 是 geo 数据的自动更新和来源的各键来自哪里（tunlr:defaults、subscription、mixin 文件，或哪里都没写时为 template）。
	GeoSources []config.KeySource `json:"geo_sources,omitempty"`
	// DNSSources 是 dns、sniffer 来自哪里（subscription、tunlr:defaults 等兜底值）、又被哪些 mixin 文件改过。
	DNSSources []config.SectionSource `json:"dns_sources,omitempty"`
	// Managed 是 tunlr 的交互功能生成的受管 mixin。
	Managed    []managedView    `json:"managed,omitempty"`
	Warnings   []config.Warning `json:"warnings,omitempty"`
	RollbackTo int              `json:"rollback_to,omitempty"`
	// Restart 是需要重启内核才能生效的变化；不为空时发布后重启内核而不是热重载。
	Restart []string `json:"restart,omitempty"`
	// GeoReplaced 是来源变了、发布时换成按新来源下载的 geo 文件。
	GeoReplaced []string `json:"geo_replaced,omitempty"`
	// Proxies 汇总节点的变化，没有变化时省略。
	Proxies *config.ProxyChanges `json:"proxies,omitempty"`
	// Diff 总是完整的；DiffSections 按顶层键汇总它增删的行数。
	Diff         string               `json:"diff,omitempty"`
	DiffSections []config.DiffSection `json:"diff_sections,omitempty"`
	Config       string               `json:"config,omitempty"`
	Check        checkView            `json:"check"`
}

// hiddenSecret 在输出中代替 API secret 的值。
const hiddenSecret = "(hidden)"

// checkView 是校验结果。Status 取值：
// passed、failed（配置有误）、error（校验未能完成，如数据下载失败）、
// not_installed（内核未安装，无法校验）、unchanged（没有变化，不需要校验）。
type checkView struct {
	Status   string   `json:"status"`
	Messages []string `json:"messages,omitempty"`
}

type applyView struct {
	Plan     planView         `json:"plan"`
	Applied  bool             `json:"applied"`
	Revision *config.Revision `json:"revision,omitempty"`
	// Reloaded 报告运行中的内核是否已热重载新配置，Restarted 报告是否已重启并通过健康检查；
	// 都为 false 表示内核没有运行。RestartNeeded 是热重载后仍需重启才能生效的键（内核不由服务运行时）。
	Reloaded      bool     `json:"reloaded"`
	Restarted     bool     `json:"restarted"`
	RestartNeeded []string `json:"restart_needed,omitempty"`
}

type revisionView struct {
	config.Revision
	Current bool `json:"current"`
}

func (a *app) pipeline() *pipeline.Pipeline {
	return &pipeline.Pipeline{Layout: a.layout, Version: a.info.Version, Validator: a.validator, Reloader: a.reloader, Download: a.download}
}

func newPlanView(p *pipeline.Plan, full bool) planView {
	v := planView{
		Kind: p.Kind, Changed: p.Changed(), FirstPublish: p.Current == nil,
		Subscription: p.Subscription, Dropped: p.Dropped, Ignored: p.Ignored, Mixins: p.Mixins,
		Overrides: p.Overrides, Rewrites: p.Rewrites, Warnings: p.Warnings, RollbackTo: p.RollbackTo, Restart: p.Restart, GeoReplaced: p.GeoReplaced, Proxies: p.Proxies, Diff: hideSecretInDiff(p.Diff),
		DiffSections: p.DiffSections, ProxyFilter: p.ProxyFilter, GeoSources: p.GeoSources, DNSSources: p.DNSSources,
	}
	if p.Managed != nil {
		v.Managed = newManagedViews(p.Managed)
	}
	if full {
		v.Config = string(p.Config)
	}
	var ce *mihomo.CheckError
	switch {
	case !p.Changed():
		v.Check.Status = "unchanged"
	case p.Check == nil:
		v.Check.Status = "passed"
	case errors.Is(p.Check, mihomo.ErrNotInstalled):
		v.Check = checkView{"not_installed", []string{p.Check.Error()}}
	case errors.As(p.Check, &ce) && !ce.MissingData:
		v.Check = checkView{"failed", ce.Messages}
	default:
		v.Check = checkView{"error", []string{p.Check.Error()}}
	}
	return v
}

// describeProxyFilter 概括节点筛选的结果，如 "kept 20 of 43 proxies; removed empty groups: 美国; filtered by mihomo in providers: a"。
func describeProxyFilter(f *config.ProxyFilterResult) string {
	var parts []string
	if f.Total > 0 || len(f.Providers) == 0 {
		parts = append(parts, fmt.Sprintf("kept %d of %d proxies", f.Kept, f.Total))
	}
	if len(f.Groups) > 0 {
		parts = append(parts, "removed empty groups: "+strings.Join(f.Groups, ", "))
	}
	if len(f.Providers) > 0 {
		parts = append(parts, "filtered by mihomo in providers: "+strings.Join(f.Providers, ", "))
	}
	return strings.Join(parts, "; ")
}

// describeGeoOverrides 列出不是兜底值（tunlr.defaults，或哪里都没写）的 geo 设置及其来源，如
// "geox-url.geosite (subscription), geo-update-interval (home/20-geo.yaml)"；都是兜底值时为空。
func describeGeoOverrides(sources []config.KeySource) string {
	var parts []string
	for _, s := range sources {
		if !s.Fallback {
			parts = append(parts, s.Path+" ("+s.From+")")
		}
	}
	return strings.Join(parts, ", ")
}

// describeDNSSources 说明 DNS 和嗅探的设置来自哪里，说法相同的键合在一起，如
// "dns from the subscription, changed by 10-dns.yaml; sniffer from the fallbacks"。
func describeDNSSources(sources []config.SectionSource) string {
	var descs []string
	var keys [][]string
	for _, s := range sources {
		files := strings.Join(s.ChangedBy, ", ")
		var d string
		switch {
		case !s.Set && s.From != "" && files != "":
			d = "removed by " + files
		case !s.Set:
			d = "not set"
		case s.From == "" && files != "":
			d = "from " + files
		case s.From == "":
			d = "from the mixins"
		default:
			d = "from " + describeLayer(s.From, s.Fallback)
			if files != "" {
				d += ", changed by " + files
			}
		}
		if i := slices.Index(descs, d); i >= 0 {
			keys[i] = append(keys[i], s.Key)
			continue
		}
		descs, keys = append(descs, d), append(keys, []string{s.Key})
	}
	parts := make([]string, len(descs))
	for i, d := range descs {
		parts[i] = strings.Join(keys[i], ", ") + " " + d
	}
	return strings.Join(parts, "; ")
}

// describeIgnored 按文件列出不从订阅采用的键，如 "dns, sniffer (home/20-dns.yaml)"。
func describeIgnored(ks []config.IgnoredKey) string {
	var files []string
	keys := map[string][]string{}
	for _, k := range ks {
		if _, ok := keys[k.File]; !ok {
			files = append(files, k.File)
		}
		keys[k.File] = append(keys[k.File], k.Key)
	}
	parts := make([]string, len(files))
	for i, f := range files {
		parts[i] = strings.Join(keys[f], ", ") + " (" + f + ")"
	}
	return strings.Join(parts, "; ")
}

// describeLayer 说明配置的一层：订阅、模板、tunlr:defaults 的兜底值，或某个 mixin 文件的 tunlr.defaults。
func describeLayer(from string, fallback bool) string {
	switch {
	case from == config.FromSubscription:
		return "the subscription"
	case from == config.FromTemplate:
		return "the template"
	case fallback && from == pipeline.ManagedDefaults:
		return "the fallbacks"
	case fallback:
		return "the fallbacks in " + from
	}
	return from
}

// writeRewrites 按受管 mixin 分组列出它在合成的最后改写的值，如 "Rewritten by tunlr:mirror: geox-url.geoip, geox-url.mmdb"。
func writeRewrites(w io.Writer, rs []config.Rewrite) {
	var by []string
	paths := map[string][]string{}
	for _, r := range rs {
		if _, ok := paths[r.By]; !ok {
			by = append(by, r.By)
		}
		paths[r.By] = append(paths[r.By], r.Path)
	}
	for _, b := range by {
		fmt.Fprintf(w, "Rewritten by %s: %s\n", b, strings.Join(paths[b], ", "))
	}
}

// printPlan 以文本输出计划：来源、差异（或完整配置）、校验结果。在终端里超过一屏时交给分页器。
func (a *app) printPlan(v planView) {
	var b strings.Builder
	a.writePlan(&b, v)
	a.page(b.String(), false)
}

func (a *app) writePlan(w io.Writer, v planView) {
	switch {
	case v.Kind == pipeline.KindRollback:
		fmt.Fprintf(w, "Rolling back to revision %d.\n", v.RollbackTo)
	case v.Subscription == "":
		fmt.Fprintln(w, "No active subscription: the configuration has no proxies (add one with: tunlr sub add).")
	default:
		fmt.Fprintf(w, "Subscription: %s\n", v.Subscription)
	}
	if len(v.Dropped) > 0 {
		fmt.Fprintf(w, "Ignored from the subscription: %s\n", strings.Join(v.Dropped, ", "))
	}
	if len(v.Ignored) > 0 {
		fmt.Fprintf(w, "Not taken from the subscription (tunlr.ignore-from-subscription): %s\n", describeIgnored(v.Ignored))
	}
	if len(v.Mixins) > 0 {
		fmt.Fprintf(w, "Mixins (in order): %s\n", strings.Join(v.Mixins, ", "))
	}
	for _, o := range v.Overrides {
		fmt.Fprintf(w, "  %s: %s overrides %s\n", o.Path, o.File, o.Previous)
	}
	if f := v.ProxyFilter; f != nil {
		fmt.Fprintf(w, "Proxy filter: %s\n", describeProxyFilter(f))
	}
	if len(v.Managed) > 0 {
		var names, defaults []string
		for _, m := range v.Managed {
			switch {
			case !m.Default:
				names = append(names, m.Name)
			case !m.empty():
				defaults = append(defaults, m.Name)
			}
		}
		fmt.Fprintf(w, "Managed by tunlr: %s (mixins cannot change their keys; see: tunlr config)\n", strings.Join(names, ", "))
		if len(defaults) > 0 {
			fmt.Fprintf(w, "Defaults from tunlr: %s (mixins can override them; see: tunlr config)\n", strings.Join(defaults, ", "))
		}
	}
	if len(v.DNSSources) > 0 {
		fmt.Fprintf(w, "DNS settings: %s (see: tunlr config)\n", describeDNSSources(v.DNSSources))
	}
	if s := describeGeoOverrides(v.GeoSources); s != "" {
		fmt.Fprintf(w, "Geo data settings not from the fallbacks: %s (see: tunlr update)\n", s)
	}
	writeRewrites(w, v.Rewrites)
	if len(v.Warnings) > 0 {
		fmt.Fprintln(w, a.style.warn("Warnings:"))
		for _, wn := range v.Warnings {
			fmt.Fprintf(w, "  %s\n", wn)
		}
	}
	fmt.Fprintln(w)

	switch {
	case v.Config != "":
		fmt.Fprint(w, v.Config)
		fmt.Fprintln(w)
	case !v.Changed:
		fmt.Fprintln(w, "No changes: the published configuration is up to date.")
		return
	case v.FirstPublish:
		fmt.Fprintln(w, "Nothing has been published yet; this will be the first configuration (show it with --full).")
		fmt.Fprintln(w)
	default:
		if v.Proxies != nil {
			fmt.Fprintf(w, "Proxies: %s\n\n", describeProxies(v.Proxies))
		}
		if n := strings.Count(v.Diff, "\n"); n > maxDiffLines && !a.fullDiff {
			a.writeDiffSections(w, v.DiffSections, n)
		} else if a.fullDiff {
			fmt.Fprint(w, a.style.diff(v.Diff))
		} else {
			width, _ := a.size()
			if width == 0 {
				width = defaultDiffWidth
			}
			diff, shortened := shortenDiff(v.Diff, width)
			fmt.Fprint(w, a.style.diff(diff))
			if shortened {
				fmt.Fprintln(w, a.style.dim("Long lines are shortened (…); show them in full with --full-diff."))
			}
		}
		fmt.Fprintln(w)
	}
	if len(v.GeoReplaced) > 0 {
		fmt.Fprintf(w, "%s %s: geox-url points elsewhere now; downloaded from the new source for the check, put in place when published.\n",
			a.style.warn("The geo data changes source:"), strings.Join(v.GeoReplaced, ", "))
	}
	switch {
	case len(v.Restart) > 0:
		fmt.Fprintf(w, "%s %s cannot change by reloading.\n", a.style.warn("mihomo will be restarted:"), strings.Join(v.Restart, ", "))
	case len(v.GeoReplaced) > 0:
		fmt.Fprintf(w, "%s mihomo loads new geo data only when it starts.\n", a.style.warn("mihomo will be restarted:"))
	}

	switch v.Check.Status {
	case "passed":
		fmt.Fprintf(w, "Check (mihomo -t): %s\n", a.style.good("passed"))
	case "failed":
		fmt.Fprintf(w, "Check (mihomo -t): %s\n", a.style.bad("FAILED"))
		for _, m := range v.Check.Messages {
			fmt.Fprintf(w, "  %s\n", m)
		}
	case "not_installed":
		fmt.Fprintf(w, "Check (mihomo -t): not run, %s\n", v.Check.Messages[0])
	default:
		fmt.Fprintf(w, "Check (mihomo -t): %s %s\n", a.style.warn("could not complete:"), v.Check.Messages[0])
	}
}

// hideSecretInDiff 把差异中顶层 secret 行的值换成 hiddenSecret（如 panel reset-secret 的差异），
// 终端滚动记录和 --json 输出里都不出现 secret；查看它用 tunlr panel。
func hideSecretInDiff(diff string) string {
	lines := strings.SplitAfter(diff, "\n")
	for i, l := range lines {
		if len(l) > 1 && strings.ContainsRune(" -+", rune(l[0])) && strings.HasPrefix(l[1:], "secret:") {
			lines[i] = l[:1] + "secret: " + hiddenSecret + "\n"
		}
	}
	return strings.Join(lines, "")
}

// maxDiffLines 是完整显示差异的最大行数，更长的差异（如换了机场）按顶层键汇总，用 --full-diff 查看全部。
const maxDiffLines = 300

// writeDiffSections 输出按顶层键汇总的增删行数，代替过长的差异。
func (a *app) writeDiffSections(w io.Writer, sections []config.DiffSection, lines int) {
	fmt.Fprintf(w, "The diff has %d lines; changed lines by section (show all with --full-diff):\n", lines)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range sections {
		key := s.Key
		if key == "" {
			key = "(top of file)"
		}
		// 同一列每格的颜色控制符一样长，tabwriter 照样对齐。
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", key, a.style.good(fmt.Sprintf("+%d", s.Added)), a.style.bad(fmt.Sprintf("-%d", s.Removed)))
	}
	tw.Flush()
}

func (a *app) withFullDiff(cmd *cobra.Command) *cobra.Command {
	cmd.Flags().BoolVar(&a.fullDiff, "full-diff", false, fmt.Sprintf("show the whole diff even when it is longer than %d lines, without shortening long lines", maxDiffLines))
	return cmd
}

// describeProxies 用一行概括节点的变化，例如
// “26 changed (port), 1 renamed (剩余流量：90.57 GB → 90.55 GB)”。
func describeProxies(c *config.ProxyChanges) string {
	const maxNames = 3
	names := func(list []string) string {
		if len(list) > maxNames {
			return strings.Join(list[:maxNames], ", ") + fmt.Sprintf(", … (%d more)", len(list)-maxNames)
		}
		return strings.Join(list, ", ")
	}
	var parts []string
	if c.Changed > 0 {
		fields := slices.Sorted(maps.Keys(c.Fields))
		if len(fields) > 1 {
			for i, f := range fields {
				fields[i] = fmt.Sprintf("%s: %d", f, c.Fields[f])
			}
		}
		parts = append(parts, fmt.Sprintf("%d changed (%s)", c.Changed, strings.Join(fields, ", ")))
	}
	if len(c.Renamed) > 0 {
		rs := make([]string, len(c.Renamed))
		for i, r := range c.Renamed {
			rs[i] = r.From + " → " + r.To
		}
		parts = append(parts, fmt.Sprintf("%d renamed (%s)", len(c.Renamed), names(rs)))
	}
	if len(c.Added) > 0 {
		parts = append(parts, fmt.Sprintf("%d added (%s)", len(c.Added), names(c.Added)))
	}
	if len(c.Removed) > 0 {
		parts = append(parts, fmt.Sprintf("%d removed (%s)", len(c.Removed), names(c.Removed)))
	}
	return strings.Join(parts, ", ")
}

// checkExit 把校验结果转换为命令的错误：配置有误为“检查未通过”（退出码 3），
// 其他未通过的情况为一般错误。结果已经输出，不再重复报告。
func checkExit(p *pipeline.Plan, allowNotInstalled bool) error {
	if p.Check == nil || (allowNotInstalled && errors.Is(p.Check, mihomo.ErrNotInstalled)) {
		return nil
	}
	return renderedError{p.Check}
}

func (a *app) newPlanCommand() *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Preview the configuration that config apply would publish",
		Long: "Compose the configuration from the template, the active subscription and the mixin\n" +
			"files in /etc/tunlr/mixin.d, show how it differs from the published one, and check it\n" +
			"with mihomo -t. A mistake in a mixin file fails the check (exit code 3).\n" +
			"Nothing is changed, except that geo data the check needs may be downloaded.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().Plan(cmd.Context()) })
			if err != nil {
				return err
			}
			v := newPlanView(plan, full)
			if err := a.render(v, func() error { a.printPlan(v); return nil }); err != nil {
				return err
			}
			return checkExit(plan, true)
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "print the whole configuration instead of the diff")
	return cmd
}

func (a *app) newApplyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Check and publish the configuration",
		Long: "Compose the configuration like plan, show the diff, and publish it after confirmation.\n" +
			"The configuration must pass mihomo -t; the previous one is kept in the history\n" +
			"(see tunlr config). Use --yes to skip the confirmation, --dry-run to only preview.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireConfirmation("config apply"); err != nil {
				return err
			}
			plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().Plan(cmd.Context()) })
			if err != nil {
				return err
			}
			return a.publish(cmd, plan, "Apply this configuration?")
		},
	}
}

func (a *app) newRollbackCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollback [revision]",
		Short: "Publish an earlier configuration again (tunlr config lists the history)",
		Long: "Publish a configuration from the history again. Without a revision, choose it from\n" +
			"the history in a terminal; otherwise (or with --yes) go back to the most recent one\n" +
			"that differs from the published configuration, so running it twice switches back.\n" +
			"The next config apply composes from the subscription and mixins again. tunlr config lists\n" +
			"the history.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			n := 0
			if len(args) == 1 {
				var err error
				if n, err = strconv.Atoi(args[0]); err != nil || n < 1 {
					return usageErrorf("revision must be a positive number, got %q", args[0])
				}
			}
			if err := a.requireConfirmation("config rollback"); err != nil {
				return err
			}
			if n == 0 && a.interactive() && !a.opts.Yes {
				var err error
				if n, err = a.chooseRevision(cmd); err != nil || n == 0 {
					_, err := a.cancelled(err)
					return err
				}
			}
			plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().PlanRollback(cmd.Context(), n) })
			if err != nil {
				return err
			}
			return a.publish(cmd, plan, fmt.Sprintf("Roll back to revision %d?", plan.RollbackTo))
		},
	}
	return cmd
}

// plan 执行 fn 合成并校验配置（可能要下载 geo 数据，较慢），期间显示 spinner。
func (a *app) plan(fn func() (*pipeline.Plan, error)) (*pipeline.Plan, error) {
	var p *pipeline.Plan
	err := a.spin("Composing and checking the configuration", func() (err error) {
		p, err = fn()
		return err
	})
	return p, err
}

// chooseRevision 让用户从历史中选一个修订，光标起始于不带参数时会回到的那一份（最近一个与生效配置
// 不同的修订）。没有历史时告诉用户并返回 0。
func (a *app) chooseRevision(cmd *cobra.Command) (int, error) {
	revs, current, err := a.pipeline().Revisions()
	if err != nil {
		return 0, err
	}
	if len(revs) == 0 {
		fmt.Fprintln(a.stdout, "No configuration has been published yet; there is nothing to roll back to.")
		return 0, nil
	}
	initial := 0
	for i, r := range revs {
		if r.Number != current {
			initial = i
			break
		}
	}
	i, err := a.prompt.Choose(cmd.Context(), "Roll back to which revision?", revisionOptions(revs, current), initial)
	if err != nil {
		return 0, err
	}
	return revs[i].Number, nil
}

// requireConfirmation 在无法提问又没有 --yes 时，于开始工作前报告用法错误。
func (a *app) requireConfirmation(command string) error {
	if a.opts.Yes || a.opts.DryRun || a.interactive() {
		return nil
	}
	return usageErrorf("%s needs confirmation; pass --yes to proceed without asking (or --dry-run to preview)", command)
}

// requireYes 同 requireConfirmation，用于不支持 --dry-run 的命令（如 core stop、core upgrade）：
// 带 --dry-run 时报告用法错误，不把它当作不必确认。
func (a *app) requireYes(command string) error {
	if a.opts.DryRun {
		return usageErrorf("%s does not support --dry-run", command)
	}
	if a.opts.Yes || a.interactive() {
		return nil
	}
	return usageErrorf("%s needs confirmation; pass --yes to proceed without asking", command)
}

// ask 在执行操作前确认，同意时返回 true。用户回答否或取消时输出 declined，记下 a.aborted（什么都
// 没做，主菜单显示为已取消）并返回 false。已经做了一部分的操作中途确认时用 confirm。
func (a *app) ask(ctx context.Context, question, description, declined string) (bool, error) {
	ok, err := a.confirm(ctx, question, description)
	if err != nil || ok {
		return ok, err
	}
	a.aborted = true
	fmt.Fprintln(a.stdout, declined)
	return false, nil
}

// confirm 向用户确认，description 是问题下的说明（可以为空）；--yes 时直接同意。调用前须用
// requireConfirmation 排除无法提问的情况。用户取消算作回答“否”。
func (a *app) confirm(ctx context.Context, question, description string) (bool, error) {
	if a.opts.Yes {
		return true, nil
	}
	ok, err := a.prompt.Confirm(ctx, question, description)
	if err != nil && !errors.Is(err, huh.ErrUserAborted) {
		return false, err
	}
	return ok, nil
}

// publish 输出计划，经确认后发布。校验未通过、--dry-run 或用户取消时不发布。
func (a *app) publish(cmd *cobra.Command, plan *pipeline.Plan, question string) error {
	v := applyView{Plan: newPlanView(plan, false)}
	if !a.opts.JSON {
		a.printPlan(v.Plan)
	}
	if err := checkExit(plan, false); err != nil || !plan.Changed() || a.opts.DryRun {
		if a.opts.JSON {
			if rerr := a.render(v, nil); rerr != nil {
				return rerr
			}
		} else if err != nil {
			fmt.Fprintln(a.stdout, "\nNot applied.")
		} else if a.opts.DryRun && plan.Changed() {
			fmt.Fprintln(a.stdout, "\nDry run: nothing applied.")
		}
		return err
	}

	// 问题下方给出节点变化的摘要，不必回头翻 diff。
	summary := ""
	if v.Plan.Proxies != nil && !v.Plan.FirstPublish {
		if s := describeProxies(v.Plan.Proxies); s != "" {
			summary = "Proxies: " + s
		}
	}
	if ok, err := a.ask(cmd.Context(), question, summary, "Not applied."); !ok {
		return err
	}

	var res *pipeline.ApplyResult
	err := a.spin("Publishing the configuration", func() (err error) {
		res, err = a.pipeline().Apply(cmd.Context(), plan)
		return err
	})
	if errors.Is(err, core.ErrRolledBack) {
		return fmt.Errorf("%w; the previous configuration is back in place", err)
	}
	if err != nil {
		return err
	}
	v.Applied, v.Revision, v.Reloaded = true, res.Revision, res.Reloaded
	v.Restarted, v.RestartNeeded = res.Restarted, res.RestartNeeded
	return a.render(v, func() error {
		fmt.Fprintf(a.stdout, "\n%s to %s.\n", a.style.good(fmt.Sprintf("Published revision %d", res.Revision.Number)), a.layout.CoreConfig())
		var err error
		switch {
		case res.Restarted:
			_, err = fmt.Fprintln(a.stdout, "mihomo has restarted with it and passed the health check.")
		case res.Reloaded && len(res.RestartNeeded) > 0:
			_, err = fmt.Fprintf(a.stdout, "mihomo has reloaded it, but %s only take effect after mihomo restarts;\n"+
				"it is not run by the tunlr service, so restart it yourself (or set it up with: tunlr init).\n",
				strings.Join(res.RestartNeeded, ", "))
		case res.Reloaded:
			_, err = fmt.Fprintln(a.stdout, "mihomo has reloaded it.")
		default:
			_, err = fmt.Fprintln(a.stdout, "mihomo is not running; it will use this configuration when it starts.")
		}
		return err
	})
}

// newRevisionViews 是配置历史的展示形式，current 是与生效配置相同的修订（0 为没有）。
func newRevisionViews(revs []config.Revision, current int) []revisionView {
	views := make([]revisionView, 0, len(revs))
	for _, r := range revs {
		views = append(views, revisionView{r, r.Number == current})
	}
	return views
}

// writeRevisions 输出配置历史（tunlr config），最新的在前，* 标出与生效配置相同的那一份。
func writeRevisions(w io.Writer, views []revisionView) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  \tREVISION\tCREATED\tSOURCE\tSUBSCRIPTION")
	current := false
	for _, v := range views {
		mark := ""
		if v.Current {
			mark, current = "*", true
		}
		sub := v.Subscription
		if sub == "" {
			sub = "-"
		}
		fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\n", mark, v.Number, v.CreatedAt.Local().Format(time.DateTime), describeReason(v.Revision), sub)
	}
	tw.Flush()
	if !current {
		fmt.Fprintln(w, "  The published configuration matches no revision (edited by hand?); apply or rollback archives it first.")
	}
}

func describeReason(r config.Revision) string {
	switch r.Reason {
	case config.ReasonRollback:
		return fmt.Sprintf("rollback to %d", r.RollbackOf)
	case config.ReasonFound:
		return "found on disk"
	}
	return r.Reason
}
