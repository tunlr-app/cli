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
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// updateTaskNames 是命令行中任务的名称（sub 与 sub 命令一致）。
var updateTaskNames = map[string]string{"sub": setup.TaskSubscription, "subscription": setup.TaskSubscription, "geo": setup.TaskGeo}

// updateTasks 把命令行中的任务名称转换为任务。
func updateTasks(args []string) ([]string, error) {
	tasks := make([]string, 0, len(args))
	for _, arg := range args {
		t, ok := updateTaskNames[arg]
		if !ok {
			return nil, usageErrorf("unknown update %q: use sub or geo", arg)
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (a *app) newUpdateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Show when subscriptions and geo data update; run updates now",
		Long: "tunlr-update.timer wakes up every hour and runs the updates that are due: each\n" +
			"subscription on its own period (tunlr sub set <sub> --update). The geo data is downloaded by\n" +
			"mihomo itself as the configuration says (geox-url, geo-auto-update, geo-update-interval);\n" +
			"tunlr neither downloads nor verifies it. Each subscription can have its own settings (tunlr\n" +
			"sub set <sub> --geo-update, --geo-url); what neither they nor the subscription set comes\n" +
			"from tunlr's built-in fallbacks (tunlr:defaults, listed by tunlr config).\n\n" +
			"Without a subcommand, show how often each one updates, how it last went and when it is\n" +
			"next due, and where each geo setting comes from.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error { return a.showSchedules(cmd.Context()) },
	}
	cmd.AddCommand(a.newUpdateRunCommand())
	return cmd
}

func (a *app) newUpdateRunCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "run [sub|geo...]",
		Short: "Run the subscription and geo data updates now, whether or not they are due",
		Long: "Run the scheduled updates now, whether or not they are due: sub fetches every saved\n" +
			"subscription (also those set to never update) and, if the active one changed, publishes\n" +
			"the configuration (like tunlr config apply --yes); geo has mihomo download the geo data from\n" +
			"geox-url now (it is not verified). Without arguments, both. A subscription that\n" +
			"fails to update keeps its last good copy; a configuration that fails mihomo -t is not\n" +
			"published. To fetch only some subscriptions, use tunlr sub update.",
		Args: usageArgs(cobra.ArbitraryArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.opts.DryRun {
				return usageErrorf("update run does not support --dry-run; preview the configuration with tunlr config plan")
			}
			tasks, err := updateTasks(args)
			if err != nil {
				return err
			}
			if len(tasks) == 0 {
				tasks = setup.Tasks
			}
			var done func(error)
			opts := setup.UpdateOptions{Tasks: tasks}
			opts.Progress, done = a.stepper()
			res, err := a.installer().Update(cmd.Context(), opts)
			if res == nil {
				done(err)
				return err
			}
			done(nil)
			return a.renderUpdate(res, err)
		},
	}
}

// newInternalUpdateCommand 是 tunlr-update.timer 每小时触发的内部命令：执行到期的更新。没有到期的
// 更新时什么都不输出，journal 中只留下真正执行过的更新。
func (a *app) newInternalUpdateCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "_update",
		Hidden: true,
		Args:   usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := a.installer().Update(cmd.Context(), setup.UpdateOptions{})
			if res == nil {
				return err
			}
			if len(res.Tasks) == 0 && !a.opts.JSON {
				return err
			}
			return a.renderUpdate(res, err)
		},
	}
}

// newInternalAftercareCommand 是 tunlr-aftercare.path 在 bin/mihomo 变化时触发的内部命令：核对换过的内核
// （面板升级的善后），结果进入 journal。
func (a *app) newInternalAftercareCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "_aftercare",
		Hidden: true,
		Args:   usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.installer().Aftercare(cmd.Context())
			return a.renderUpdate(&setup.UpdateResult{Tasks: []string{}, Steps: []setup.Step{s}}, err)
		},
	}
}

// renderUpdate 输出每一步的结果。err 是 Update 返回的错误：有步骤失败时结果已经说明了原因，
// 不再重复输出错误，只让退出码为非零。
func (a *app) renderUpdate(res *setup.UpdateResult, err error) error {
	width := 7
	for _, s := range res.Steps {
		width = max(width, len(s.Name))
	}
	rerr := a.render(res, func() error {
		for _, s := range res.Steps {
			a.printCheck(s.Status, s.Name, width, s.Message)
		}
		return nil
	})
	if err != nil {
		return renderedError{err}
	}
	return rerr
}

// showSchedules 列出每个订阅和 geo 数据的更新周期、上次执行和下次到期，以及 geo 数据各项设置的来源（tunlr update）。
func (a *app) showSchedules(ctx context.Context) error {
	scheds, err := a.installer().Schedules()
	if err != nil {
		return err
	}
	// 合成失败（如启用的订阅还没有内容）时不列出来源，问题由 config plan 报告。
	if sources, err := a.installer().GeoSources(ctx, ""); err == nil {
		scheds[len(scheds)-1].Sources = sources
	}
	return a.render(scheds, func() error {
		tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "UPDATE\tEVERY\tLAST RUN\tNEXT DUE")
		var failed []setup.Schedule
		var geo setup.Schedule
		for _, s := range scheds {
			if s.Task == setup.TaskGeo {
				geo = s
			}
			last := "never"
			if s.Last != nil {
				last = formatTime(s.Last.At) + " " + s.Last.Status
				if s.Last.Status == setup.StepFailed {
					failed = append(failed, s)
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", scheduleLabel(s), describePeriod(s.Period), last, a.describeNext(s))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		for _, s := range failed {
			fmt.Fprintf(a.stdout, "\nThe last update of %s failed: %s\n", scheduleObject(s), s.Last.Message)
		}
		fmt.Fprintln(a.stdout)
		if len(geo.Sources) > 0 {
			active := ""
			for _, s := range scheds {
				if s.Active {
					active = s.Name
				}
			}
			if active != "" {
				fmt.Fprintf(a.stdout, "Geo data settings with %q active (tunlr sub set %s --geo-update, --geo-url changes them):\n", active, active)
			} else {
				fmt.Fprintln(a.stdout, "Geo data settings (no subscription yet):")
			}
			if err := a.writeGeoSources(geo.Sources); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout)
		}
		fmt.Fprintln(a.stdout, a.style.dim(describeGeoUpdates(geo)))
		_, err := fmt.Fprintln(a.stdout, a.style.dim("Due updates run at the next hourly check; tunlr update run runs them now."))
		return err
	})
}

// describeGeoUpdates 说明 geo 数据由谁更新、最新的文件是什么时候的。
func describeGeoUpdates(s setup.Schedule) string {
	line := "mihomo downloads the geo data by itself (geo-auto-update in the configuration)"
	if s.Period == settings.PeriodOff {
		line = "mihomo does not update the geo data (geo-auto-update is off in the configuration)"
	}
	if !s.Updated.IsZero() {
		line += "; the newest file is from " + formatTime(s.Updated)
	}
	return line + "."
}

// writeGeoSources 列出 geo 数据各项设置的值和来源。
func (a *app) writeGeoSources(sources []config.KeySource) error {
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	for _, s := range sources {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", s.Path, cmp.Or(s.Value, "(mihomo's default)"), a.describeGeoSource(s))
	}
	return tw.Flush()
}

// describeGeoSource 说明一项 geo 设置来自哪里：兜底值（tunlr:defaults，或 mixin 中的 tunlr.defaults），
// 订阅，或 mixin 的 mihomo:（如 tunlr sub set --geo-update 写的订阅的 geo mixin）；哪里都没有写时用内核的默认值。
func (a *app) describeGeoSource(s config.KeySource) string {
	switch {
	case s.Fallback && s.From == config.FromTemplate:
		return a.style.dim("not set")
	case s.Fallback:
		return a.style.dim("fallback (" + s.From + ")")
	case s.From == config.FromSubscription:
		return "the subscription"
	}
	return "mixin " + s.From
}

// scheduleLabel 是一项更新在表格中的名称，如 "sub home *"（启用的订阅）、"geo"。
func scheduleLabel(s setup.Schedule) string {
	if s.Task == setup.TaskGeo {
		return "geo"
	}
	if s.Active {
		return "sub " + s.Name + " *"
	}
	return "sub " + s.Name
}

// scheduleObject 是一项更新在句子中的说法，如 `the subscription "home"`、"the geo data"。
func scheduleObject(s setup.Schedule) string {
	if s.Task == setup.TaskGeo {
		return "the geo data"
	}
	return fmt.Sprintf("the subscription %q", s.Name)
}

// describePeriod 是周期设置的说法，带上设置的原值，如 "daily (1d)"。
func describePeriod(p string) string {
	if p == settings.PeriodOff {
		return "never (off)"
	}
	return settings.DescribePeriod(p) + " (" + p + ")"
}

// describeNext 是下次到期的时间。geo 数据是内核下一次更新的大致时间，由内核决定。
func (a *app) describeNext(s setup.Schedule) string {
	switch {
	case s.Next.IsZero():
		return "-"
	case s.Task == setup.TaskGeo:
		return "~" + formatTime(s.Next) + " (mihomo)"
	case s.Due(time.Now()):
		return "now"
	}
	return formatTime(s.Next)
}

// describeSchedule 是 status 中一项更新的一行说明，如 "sub home daily, last ok 2026-10-27 03:12"、
// "geo weekly by mihomo, newest file 2026-10-20 12:00"。
func (a *app) describeSchedule(s setup.Schedule) string {
	name := "sub " + s.Name
	if s.Task == setup.TaskGeo {
		name = "geo"
	}
	if s.Period == settings.PeriodOff {
		return name + " " + a.style.dim("never")
	}
	line := name + " " + settings.DescribePeriod(s.Period)
	if s.Task == setup.TaskGeo {
		line += " by mihomo"
		if s.Updated.IsZero() {
			return line + a.style.dim(", no files yet")
		}
		return line + a.style.dim(", newest file "+formatTime(s.Updated))
	}
	switch {
	case s.Last == nil:
		line += a.style.dim(", not run yet")
	case s.Last.Status == setup.StepFailed:
		line += ", " + a.style.bad("last failed "+formatTime(s.Last.At))
	default:
		line += a.style.dim(", last " + s.Last.Status + " " + formatTime(s.Last.At))
	}
	return line
}

// 交互地设置周期时可选的周期。
var periodChoices = []string{"1h", "6h", "12h", "1d", "7d", settings.PeriodOff}

const periodOther = "Other..."

// setGeoUpdate 是 sub set <sub> --geo-update <period> --geo-url <file>=<url>...：修改一个订阅的 geo 数据设置（写进它的
// geo mixin），订阅启用时随后发布。period 和 urls 都为空时（菜单的“Change a sub”）列出当前的设置，选择改哪一项。
func (a *app) setGeoUpdate(cmd *cobra.Command, sub, period string, urls []string) error {
	ctx := cmd.Context()
	if a.opts.DryRun {
		return usageErrorf("sub set --geo-update, --geo-url does not support --dry-run")
	}
	ch := setup.GeoChange{URLs: map[string]string{}, Update: period}
	for _, u := range urls {
		k, v, ok := strings.Cut(u, "=")
		if !ok || v == "" {
			return usageErrorf("--geo-url takes <file>=<url> or <file>=default, e.g. --geo-url geosite=https://example.com/geosite.dat")
		}
		ch.URLs[k] = v
	}
	if ch.Update != "" && ch.Update != setup.GeoDefault {
		if _, err := settings.ParsePeriod(ch.Update); err != nil {
			return err
		}
	}
	entries, err := a.subscriptions().List()
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(entries, func(e subscription.Entry) bool { return e.Name == sub }) {
		return fmt.Errorf("%w: %s", subscription.ErrNotFound, sub)
	}
	if ch.Update == "" && len(urls) == 0 {
		if ch, err = a.chooseGeoChange(cmd, sub); err != nil {
			_, err := a.cancelled(err)
			return err
		}
	}
	if err := a.requireYes("sub set --geo-update, --geo-url"); err != nil {
		return err
	}
	w, err := a.installer().SetGeo(ctx, sub, ch)
	if err != nil {
		return err
	}
	if !a.opts.JSON {
		if _, err := os.Stat(w.File); errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(a.stdout, "Removed %s: %q uses what it sets, or else tunlr's fallbacks.\n", w.File, sub)
		} else {
			fmt.Fprintf(a.stdout, "Wrote %s for %q to %s.\n", strings.Join(w.Keys, ", "), sub, w.File)
		}
	}
	return a.publishSubChange(cmd, sub, w.Active, w)
}

// publishSubChange 在订阅 sub 的 mixin 改好之后发布配置（sub set --geo-*、--dns）：sub 是启用的订阅（active）时
// 预览、确认后发布，否则说明在它启用时生效。view 是 --json 输出中的写入结果。
func (a *app) publishSubChange(cmd *cobra.Command, sub string, active bool, view any) error {
	if !active {
		return a.render(view, func() error {
			_, err := fmt.Fprintf(a.stdout, "%q is not the active subscription; the change applies when it is (tunlr sub use %s).\n", sub, sub)
			return err
		})
	}
	plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().Plan(cmd.Context()) })
	switch {
	case err != nil:
		return fmt.Errorf("the change is saved, but the configuration could not be composed: %w", err)
	case plan.Current == nil:
		return a.render(view, func() error {
			_, err := fmt.Fprintln(a.stdout, "No configuration has been published yet; the change takes effect with tunlr config apply.")
			return err
		})
	case !plan.Changed():
		return a.render(view, func() error {
			_, err := fmt.Fprintln(a.stdout, "The configuration stays the same.")
			return err
		})
	}
	if !a.opts.JSON {
		fmt.Fprintln(a.stdout)
	}
	err = a.publish(cmd, plan, "Apply this configuration?")
	if err == nil && a.aborted {
		fmt.Fprintln(a.stdout, "The change stays saved; tunlr config apply publishes it later.")
	}
	a.aborted = false // 修改已经保存，不发布也不是什么都没做。
	return err
}

// chooseGeoChange 列出订阅 sub 启用时 geo 数据各项设置的值和来源，让用户选择改哪一项并给出新值。
func (a *app) chooseGeoChange(cmd *cobra.Command, sub string) (setup.GeoChange, error) {
	ctx := cmd.Context()
	ch := setup.GeoChange{URLs: map[string]string{}}
	sources, err := a.installer().GeoSources(ctx, sub)
	if err != nil {
		return ch, err
	}
	fmt.Fprintf(a.stdout, "Geo data settings with %q active:\n", sub)
	if err := a.writeGeoSources(sources); err != nil {
		return ch, err
	}
	fmt.Fprintln(a.stdout)
	own := sub + "/" + setup.GeoMixinFile
	values := map[string]string{}
	changed := false
	for _, s := range sources {
		values[s.Path] = s.Value
		changed = changed || s.From == own
	}
	period := setup.GeoPeriod(sources)
	options := []string{"How often mihomo updates it (now " + describePeriod(period) + ")"}
	for _, k := range setup.GeoURLKeys {
		options = append(options, "Where "+k+" is downloaded from")
	}
	if changed {
		options = append(options, "Undo all changes for "+sub+" (remove "+own+")")
	}
	i, err := a.prompt.Choose(ctx, "Change what?", options, 0)
	if err != nil {
		return ch, err
	}
	def := "what " + sub + " sets, or else tunlr's fallbacks"
	switch {
	case i == 0:
		ch.Update, err = a.choosePeriod(cmd, "geo data", period, def)
	case i <= len(setup.GeoURLKeys):
		k := setup.GeoURLKeys[i-1]
		var u string
		u, err = a.prompt.Input(ctx, "Where to download "+k+" from",
			"An http or https URL; leave it empty for the default ("+def+").\nNow: "+cmp.Or(values["geox-url."+k], "mihomo's default"), "",
			func(s string) error {
				if s = strings.TrimSpace(s); s == "" {
					return nil
				}
				return setup.ValidateGeoURL(s)
			})
		ch.URLs[k] = cmp.Or(strings.TrimSpace(u), setup.GeoDefault)
	default:
		ch.Update = setup.GeoDefault
		for _, k := range setup.GeoURLKeys {
			ch.URLs[k] = setup.GeoDefault
		}
	}
	return ch, err
}

// setSubUpdate 是 sub set <name> --update <period>：修改一个订阅的自动更新周期。period 为空时（菜单的
// “Change a sub”）从列表中选。
func (a *app) setSubUpdate(cmd *cobra.Command, name, period string) error {
	if a.opts.DryRun {
		return usageErrorf("sub set --update does not support --dry-run")
	}
	entries, err := a.subscriptions().List()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(entries, func(e subscription.Entry) bool { return e.Name == name })
	if i < 0 {
		return fmt.Errorf("%w: %s", subscription.ErrNotFound, name)
	}
	source := cmp.Or(entries[i].URL, entries[i].Path)
	if period != "" {
		if period, err = updatePeriodArg(period); err != nil {
			return err
		}
	} else {
		if period, err = a.choosePeriod(cmd, fmt.Sprintf("subscription %q", name), entries[i].UpdatePeriod(), ""); err != nil || period == "" {
			_, err := a.cancelled(err)
			return err
		}
	}
	if err := a.subscriptions().Set(cmd.Context(), name, period); err != nil {
		return err
	}
	if err := a.syncTimers(cmd); err != nil {
		return err
	}
	return a.render(map[string]string{"name": name, "update": period}, func() error {
		if period == settings.PeriodOff {
			_, err := fmt.Fprintf(a.stdout, "%q is no longer updated automatically; update it with tunlr sub update %s.\n", name, name)
			return err
		}
		_, err := fmt.Fprintf(a.stdout, "%q is %s.\n", name, describeFetching(period, source))
		return err
	})
}

// choosePeriod 让用户从常用周期中选择，或输入其他周期；光标起始于当前设置。object 是更新对象在
// 问题中的说法，如 "geo data"。def 不为空时另有恢复默认的一项（返回 "default"），def 说明默认是什么。
func (a *app) choosePeriod(cmd *cobra.Command, object, current, def string) (string, error) {
	choices := slices.Clone(periodChoices)
	options := make([]string, 0, len(choices)+2)
	for _, p := range choices {
		options = append(options, describePeriod(p))
	}
	if def != "" {
		choices = append(choices, setup.GeoDefault)
		options = append(options, "Default ("+def+")")
	}
	options = append(options, periodOther)
	initial := max(0, slices.Index(choices, current))
	i, err := a.prompt.Choose(cmd.Context(), fmt.Sprintf("Update the %s how often?", object), options, initial)
	if err != nil {
		return "", err
	}
	if i < len(choices) {
		return choices[i], nil
	}
	p, err := a.prompt.Input(cmd.Context(), "Period", "A number of hours or days, such as 3h or 2d, or off.", "", func(s string) error {
		_, err := settings.ParsePeriod(strings.TrimSpace(s))
		return err
	})
	return strings.TrimSpace(p), err
}
