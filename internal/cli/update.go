package cli

import (
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/pipeline"
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
			"subscription on its own period and the geo data on its (tunlr update set).\n\n" +
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
			"the configuration (like tunlr config apply --yes); geo has mihomo download the geo data and\n" +
			"verifies it against the official sha256. Without arguments, both. A subscription that\n" +
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
		for _, s := range scheds {
			last := "never"
			if s.Last != nil {
				last = formatTime(s.Last.At) + " " + s.Last.Status
				if s.Updater == settings.GeoUpdaterMihomo {
					last = formatTime(s.Last.At) + " (newest file)"
				}
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
		_, err := fmt.Fprintln(a.stdout, a.style.dim("\nDue updates run at the next hourly check; tunlr update run runs them now."))
		return err
	})
}

// scheduleLabel 是一项更新在表格中的名称，如 "sub home *"（启用的订阅）、"geo (by tunlr)"。
func scheduleLabel(s setup.Schedule) string {
	if s.Task == setup.TaskGeo {
		return "geo (by " + s.Updater + ")"
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

// describeNext 是下次到期的时间；由内核自己更新 geo 数据时由它决定。
func (a *app) describeNext(s setup.Schedule) string {
	switch {
	case s.Updater == settings.GeoUpdaterMihomo && s.Period != settings.PeriodOff:
		return "mihomo"
	case s.Next.IsZero():
		return "-"
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
		line += " by " + s.Updater
	}
	switch {
	case s.Last == nil:
		line += a.style.dim(", not run yet")
	case s.Updater == settings.GeoUpdaterMihomo:
		line += a.style.dim(", newest file " + formatTime(s.Last.At))
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

// updaterChoices 是交互地选择 geo 更新方式时的选项，顺序同 updaters。
var (
	updaters       = []string{settings.GeoUpdaterTunlr, settings.GeoUpdaterMihomo}
	updaterChoices = []string{
		"tunlr   has mihomo download it on schedule, then verifies it against the official sha256",
		"mihomo  the kernel updates it by itself (geo-auto-update); nothing is verified",
	}
)

func (a *app) newUpdateSetCommand() *cobra.Command {
	var by string
	cmd := &cobra.Command{
		Use:   "set geo|sub [name] [period]",
		Short: "Set how often the geo data or a subscription updates",
		Long: "Set how often something is updated: a number of hours or days such as 6h, 1d or 7d,\n" +
			"off for never, or default.\n\n" +
			"update set sub <name> [period] sets a subscription's period (default " + state.DefaultUpdate + "). Every saved\n" +
			"subscription is fetched on its own period; only the active one's configuration is\n" +
			"published. A new subscription's period is given with tunlr sub add --update.\n\n" +
			"update set geo [period] sets the geo data's (default " + settings.DefaultGeoUpdate + "). --by says who updates it:\n" +
			"tunlr (the default) has mihomo download it on schedule and verifies it against the\n" +
			"official sha256; mihomo lets the kernel update it by itself (geo-auto-update in the\n" +
			"configuration, nothing is verified). Switching to or from mihomo changes the\n" +
			"configuration, so tunlr then shows the diff and publishes it after confirmation (mihomo\n" +
			"restarts). The setting is saved in " + a.layout.SettingsFile() + ".\n\n" +
			"tunlr-update.timer is turned off when nothing is scheduled. What is left out is chosen\n" +
			"from a list (needs a terminal).",
		Args: usageArgs(cobra.MaximumNArgs(3)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.opts.DryRun {
				return usageErrorf("update set does not support --dry-run")
			}
			target := ""
			if len(args) > 0 {
				target, args = args[0], args[1:]
			} else {
				if !a.interactive() {
					return usageErrorf("update set needs geo or sub, e.g. tunlr update set geo 7d or tunlr update set sub home 6h")
				}
				var err error
				if target, args, err = a.chooseUpdateTarget(cmd); err != nil || target == "" {
					_, err := a.cancelled(err)
					return err
				}
			}
			switch target {
			case "geo":
				return a.setGeoUpdate(cmd, args, by)
			case "sub", "subscription":
				if by != "" {
					return usageErrorf("--by is for the geo data only; tunlr fetches subscriptions")
				}
				return a.setSubUpdate(cmd, args)
			}
			return usageErrorf("update set takes geo or sub first, got %q", target)
		},
	}
	cmd.Flags().StringVar(&by, "by", "", "who updates the geo data: tunlr or mihomo (default restores tunlr)")
	return cmd
}

// chooseUpdateTarget 让用户从订阅和 geo 数据中选择要设置周期的一项，返回 update set 的目标和余下的参数。
func (a *app) chooseUpdateTarget(cmd *cobra.Command) (string, []string, error) {
	scheds, err := a.installer().Schedules()
	if err != nil {
		return "", nil, err
	}
	options := make([]string, len(scheds))
	for i, s := range scheds {
		options[i] = fmt.Sprintf("%s (%s)", scheduleLabel(s), describePeriod(s.Period))
	}
	i, err := a.prompt.Choose(cmd.Context(), "Change how often what updates?", options, 0)
	if err != nil {
		return "", nil, err
	}
	if s := scheds[i]; s.Task == setup.TaskSubscription {
		return "sub", []string{s.Name}, nil
	}
	return "geo", nil, nil
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
	var period string
	if len(args) == 2 {
		var err error
		if period, err = updatePeriodArg(args[1]); err != nil {
			return err
		}
	} else {
		entries, err := a.subscriptions().List()
		if err != nil {
			return err
		}
		i := slices.IndexFunc(entries, func(e subscription.Entry) bool { return e.Name == name })
		if i < 0 {
			return fmt.Errorf("%w: %s", subscription.ErrNotFound, name)
		}
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
			_, err := fmt.Fprintf(a.stdout, "%q is no longer fetched automatically; fetch it with tunlr sub update %s.\n", name, name)
			return err
		}
		_, err := fmt.Fprintf(a.stdout, "%q is %s.\n", name, describeFetching(period))
		return err
	})
}

// setGeoUpdate 是 update set geo [period] [--by]：修改 geo 数据的更新周期和方式。
func (a *app) setGeoUpdate(cmd *cobra.Command, args []string, by string) error {
	if len(args) > 1 {
		return usageErrorf("update set geo takes the period only, e.g. tunlr update set geo 7d --by mihomo")
	}
	if len(args) == 0 && by == "" && !a.interactive() {
		return usageErrorf("update set geo needs the period or --by, e.g. tunlr update set geo 7d --by mihomo")
	}
	set, err := settings.Load(a.layout.SettingsFile())
	if err != nil {
		return err
	}
	period, updater := set.GeoUpdate, set.GeoUpdater
	if by != "" {
		updater = by
	} else if len(args) == 0 {
		i, err := a.prompt.Choose(cmd.Context(), "Who updates the geo data?", updaterChoices, slices.Index(updaters, set.GeoUpdaterValue()))
		if cancelled, err := a.cancelled(err); cancelled {
			return err
		}
		updater = updaters[i]
	}
	if len(args) == 1 {
		period = args[0]
	} else if a.interactive() {
		if period, err = a.choosePeriod(cmd, "geo data", set.GeoPeriod()); err != nil || period == "" {
			_, err := a.cancelled(err)
			return err
		}
	}
	if period == "default" {
		period = ""
	} else if period != "" {
		if _, err := settings.ParsePeriod(period); err != nil {
			return err
		}
	}
	if updater == "default" {
		updater = ""
	} else if updater != "" {
		if err := settings.ValidateGeoUpdater(updater); err != nil {
			return err
		}
	}
	// 由内核自己更新 geo 数据的开关在配置中：涉及它时要发布配置，先确认能够提问。
	old := set.GeoUpdaterValue()
	publish := old == settings.GeoUpdaterMihomo || (updater == settings.GeoUpdaterMihomo && period != settings.PeriodOff)
	if publish {
		if err := a.requireConfirmation("update set geo"); err != nil {
			return err
		}
	}
	if err := a.installer().SetGeoUpdate(cmd.Context(), period, updater); err != nil {
		return err
	}
	scheds, err := a.installer().Schedules()
	if err != nil {
		return err
	}
	s := scheds[len(scheds)-1]
	if !publish {
		return a.render(s, a.printGeoSetting(s))
	}
	if !a.opts.JSON {
		if err := a.printGeoSetting(s)(); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout)
	}
	plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().Plan(cmd.Context()) })
	if err == nil {
		err = a.publish(cmd, plan, "Apply this configuration?")
	}
	if err == nil && !a.aborted {
		return nil
	}
	// 没有发布：设置退回去，配置和设置保持一致。
	if rerr := a.installer().SetGeoUpdate(cmd.Context(), set.GeoUpdate, set.GeoUpdater); rerr != nil {
		return fmt.Errorf("%w; and the setting could not be reverted: %v", err, rerr)
	}
	if !a.opts.JSON {
		fmt.Fprintln(a.stdout, "The geo update setting is back to what it was.")
	}
	return err
}

// printGeoSetting 输出 geo 数据的更新设置。
func (a *app) printGeoSetting(s setup.Schedule) func() error {
	return func() error {
		switch {
		case s.Period == settings.PeriodOff:
			_, err := fmt.Fprintln(a.stdout, "The geo data is no longer updated automatically; run tunlr update run geo when needed.")
			return err
		case s.Updater == settings.GeoUpdaterMihomo:
			_, err := fmt.Fprintf(a.stdout, "The geo data is updated %s by mihomo itself (not verified).\n", settings.DescribePeriod(s.Period))
			return err
		}
		_, err := fmt.Fprintf(a.stdout, "The geo data is updated %s by tunlr; next due %s.\n", settings.DescribePeriod(s.Period), a.describeNext(s))
		return err
	}
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
