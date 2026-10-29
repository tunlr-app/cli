package cli

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/state"
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
		Short: "Show when subscriptions and geo data update; run updates now, set their schedules",
		Long: "tunlr-update.timer wakes up every hour and runs the updates that are due: each\n" +
			"subscription on its own period (tunlr update set). The geo data is downloaded by mihomo\n" +
			"itself as the configuration says (geo-auto-update, geo-update-interval: the template's,\n" +
			"the subscription's or a mixin's); after each download the timer verifies the files that\n" +
			"come from a GitHub release against the official sha256.\n\n" +
			"Without a subcommand, show how often each one updates, how it last went and when it is\n" +
			"next due.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error { return a.showSchedules() },
	}
	cmd.AddCommand(a.newUpdateRunCommand(), a.newUpdateSetCommand())
	return cmd
}

func (a *app) newUpdateRunCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "run [sub|geo...]",
		Short: "Run the subscription and geo data updates now, whether or not they are due",
		Long: "Run the scheduled updates now, whether or not they are due: sub fetches every saved\n" +
			"subscription (also those set to never update) and, if the active one changed, publishes\n" +
			"the configuration (like tunlr config apply --yes); geo has mihomo download the geo data from\n" +
			"geox-url and verifies the files from a GitHub release against the official sha256 (other\n" +
			"sources cannot be verified). Without arguments, both. A subscription that\n" +
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

// showSchedules 列出每个订阅和 geo 数据的更新周期、上次执行和下次到期（tunlr update）。
func (a *app) showSchedules() error {
	scheds, err := a.installer().Schedules()
	if err != nil {
		return err
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
		fmt.Fprintln(a.stdout, a.style.dim(describeGeoUpdates(geo)))
		_, err := fmt.Fprintln(a.stdout, a.style.dim("Due updates run at the next hourly check; tunlr update run runs them now."))
		return err
	})
}

// describeGeoUpdates 说明 geo 数据由谁更新、何时核对。
func describeGeoUpdates(s setup.Schedule) string {
	line := "mihomo downloads the geo data by itself (geo-auto-update in the configuration)"
	if s.Period == settings.PeriodOff {
		line = "mihomo does not update the geo data (geo-auto-update is off in the configuration)"
	}
	if !s.Updated.IsZero() {
		line += "; the newest file is from " + formatTime(s.Updated)
	}
	return line + ".\ntunlr verifies the files after each download (LAST RUN)."
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

// describeNext 是下次到期的时间。geo 数据是内核下一次更新的大致时间，由内核决定；内核更新过、还没有
// 核对时为 "verify"。
func (a *app) describeNext(s setup.Schedule) string {
	switch {
	case s.Task == setup.TaskGeo && s.Due(time.Now()):
		return "verify now"
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
// "geo weekly by mihomo, verified 2026-10-20 12:00"。
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
		switch {
		case s.Last == nil:
			return line + a.style.dim(", not verified yet")
		case s.Last.Status == setup.StepFailed:
			return line + ", " + a.style.bad("verification failed "+formatTime(s.Last.At))
		}
		return line + a.style.dim(", verified "+formatTime(s.Last.At))
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

// geoUpdateHint 说明 geo 数据的更新周期在哪里改。
const geoUpdateHint = "the geo data is updated by mihomo as the configuration says; change geo-auto-update and geo-update-interval (hours) with a mixin (tunlr mixin edit)"

func (a *app) newUpdateSetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set sub [name] [period]",
		Short: "Set how often a subscription updates",
		Long: "Set how often a subscription is fetched: a number of hours or days such as 6h, 1d or 7d,\n" +
			"off for never, or default (" + state.DefaultUpdate + "). Every saved subscription is fetched on its own\n" +
			"period; only the active one's configuration is published. A new subscription's period is\n" +
			"given with tunlr sub add --update. What is left out is chosen from a list (needs a\n" +
			"terminal).\n\n" +
			"The geo data is updated by mihomo itself as the configuration says: change\n" +
			"geo-auto-update and geo-update-interval (hours) with a mixin.",
		Args: usageArgs(cobra.MaximumNArgs(3)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.opts.DryRun {
				return usageErrorf("update set does not support --dry-run")
			}
			if len(args) > 0 {
				switch args[0] {
				case "sub", "subscription":
					args = args[1:]
				case "geo":
					return usageErrorf("%s", geoUpdateHint)
				default:
					return usageErrorf("update set takes sub first, e.g. tunlr update set sub home 6h")
				}
			}
			return a.setSubUpdate(cmd, args)
		},
	}
}

// setSubUpdate 是 update set sub [name] [period]：修改一个订阅的自动更新周期。
func (a *app) setSubUpdate(cmd *cobra.Command, args []string) error {
	if len(args) > 2 {
		return usageErrorf("update set sub takes a name and a period, e.g. tunlr update set sub home 6h")
	}
	if len(args) < 2 && !a.interactive() {
		return usageErrorf("update set sub needs the name of a subscription and the period, e.g. tunlr update set sub home 6h")
	}
	name := ""
	if len(args) > 0 {
		name = args[0]
	} else {
		var err error
		if name, err = a.chooseSubscription(cmd, "Change which subscription?"); err != nil {
			_, err := a.cancelled(err)
			return err
		}
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
	var period string
	if len(args) == 2 {
		if period, err = updatePeriodArg(args[1]); err != nil {
			return err
		}
	} else {
		if period, err = a.choosePeriod(cmd, fmt.Sprintf("subscription %q", name), entries[i].UpdatePeriod()); err != nil || period == "" {
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
// 问题中的说法，如 "geo data"。
func (a *app) choosePeriod(cmd *cobra.Command, object, current string) (string, error) {
	options := make([]string, 0, len(periodChoices)+1)
	for _, p := range periodChoices {
		options = append(options, describePeriod(p))
	}
	options = append(options, periodOther)
	initial := max(0, slices.Index(periodChoices, current))
	i, err := a.prompt.Choose(cmd.Context(), fmt.Sprintf("Update the %s how often?", object), options, initial)
	if err != nil {
		return "", err
	}
	if i < len(periodChoices) {
		return periodChoices[i], nil
	}
	p, err := a.prompt.Input(cmd.Context(), "Period", "A number of hours or days, such as 3h or 2d, or off.", func(s string) error {
		_, err := settings.ParsePeriod(strings.TrimSpace(s))
		return err
	})
	return strings.TrimSpace(p), err
}
