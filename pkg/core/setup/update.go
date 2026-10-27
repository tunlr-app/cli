package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// 定时更新各步骤的结果。
const (
	StepOK      = "ok"
	StepSkipped = "skipped"
	StepFailed  = "failed"
)

// Step 是定时更新中一步的结果。
type Step struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// UpdateResult 是定时更新的结果。
type UpdateResult struct {
	// Tasks 是执行了的任务；定时执行时没有到期的任务则为空。
	Tasks []string `json:"tasks"`
	Steps []Step   `json:"steps"`
}

// 定时更新的任务：订阅（每个订阅按自己的周期更新，启用的订阅更新后发布）和 geo 数据。它们是会过期的
// 资源，周期由用户设置。tunlr 自身的升级不做定时任务，只能手动执行。
const (
	TaskSubscription = "subscription"
	TaskGeo          = "geo"
)

// Tasks 是全部定时更新任务，按执行顺序。
var Tasks = []string{TaskSubscription, TaskGeo}

// Schedule 是一项定时更新（一个订阅，或 geo 数据）的周期和执行情况。
type Schedule struct {
	Task string `json:"task"`
	// Name 是订阅的名称，Active 报告它是否为启用的订阅；geo 数据没有。
	Name   string `json:"name,omitempty"`
	Active bool   `json:"active,omitempty"`
	// Period 是周期设置，如 "1d"；settings.PeriodOff 表示不定时更新。
	Period string `json:"period"`
	// Updater 是谁来更新 geo 数据（settings.GeoUpdaterTunlr 或 GeoUpdaterMihomo）；订阅没有。
	Updater string `json:"updater,omitempty"`
	// Last 是最近一次执行的情况，没有执行过时为 nil。由内核自己更新 geo 数据时是最新的 geo 文件的
	// 修改时间：内核不报告它的更新结果。
	Last *state.UpdateRun `json:"last,omitempty"`
	// Next 是到期时间，tunlr-update.timer 每小时唤醒一次，到期后的第一次唤醒时执行。订阅见
	// subscription.NextUpdate；geo 数据是上次执行（没有执行过时为安装）之后再过一个周期。不定时
	// 更新、或由内核自己更新 geo 数据时为零值。
	Next time.Time `json:"next,omitzero"`
}

// Due 报告任务在 now 是否到期。
func (s Schedule) Due(now time.Time) bool { return !s.Next.IsZero() && !now.Before(s.Next) }

// Schedules 返回各项定时更新的周期和执行情况：先是各订阅（按添加顺序），最后是 geo 数据。
func (i *Installer) Schedules() ([]Schedule, error) {
	set, err := settings.Load(i.Layout.SettingsFile())
	if err != nil {
		return nil, err
	}
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var out []Schedule
	if st != nil {
		for k := range st.Subscriptions {
			s := &st.Subscriptions[k]
			sched := Schedule{Task: TaskSubscription, Name: s.Name, Active: s.Name == st.Active, Period: s.UpdatePeriod(), Next: subscription.NextUpdate(s)}
			switch {
			case s.LastErrorAt.After(s.FetchedAt):
				sched.Last = &state.UpdateRun{At: s.LastErrorAt, Status: StepFailed, Message: s.LastError}
			case !s.FetchedAt.IsZero():
				sched.Last = &state.UpdateRun{At: s.FetchedAt, Status: StepOK}
				if s.Counts != nil {
					sched.Last.Message = fmt.Sprintf("%d proxies", s.Counts.Proxies)
				}
			}
			out = append(out, sched)
		}
	}

	geo := Schedule{Task: TaskGeo, Period: set.GeoPeriod(), Updater: set.GeoUpdaterValue()}
	if geo.Updater == settings.GeoUpdaterMihomo {
		at, err := mihomo.GeoModTime(i.Layout.CoreDir(), mihomo.AllGeoFiles)
		if err != nil {
			return nil, err
		}
		if !at.IsZero() {
			geo.Last = &state.UpdateRun{At: at.UTC(), Status: StepOK, Message: "newest geo file"}
		}
		return append(out, geo), nil
	}
	since := i.now()
	if st != nil {
		since = st.CreatedAt
		if run, ok := st.Updates[TaskGeo]; ok {
			geo.Last, since = &run, run.At
		}
	}
	// 设置在读取时已经校验过。
	if d, _ := settings.ParsePeriod(geo.Period); d > 0 {
		geo.Next = since.Add(d)
	}
	return append(out, geo), nil
}

// DescribeGeoUpdate 是 geo 数据更新设置的说法，如 "weekly by tunlr"、"weekly by mihomo"、"never"。
func DescribeGeoUpdate(set *settings.Settings) string {
	if set.GeoPeriod() == settings.PeriodOff {
		return "never"
	}
	return settings.DescribePeriod(set.GeoPeriod()) + " by " + set.GeoUpdaterValue()
}

// SetGeoUpdate 设置 geo 数据的更新周期和更新方式，为空的恢复默认值。已经安装时随即按新的设置启用或
// 停用 tunlr-update.timer。更新方式会改变配置中的 geo-auto-update，由调用方随后发布配置。
func (i *Installer) SetGeoUpdate(ctx context.Context, period, updater string) error {
	if period != "" {
		if _, err := settings.ParsePeriod(period); err != nil {
			return err
		}
	}
	if updater != "" {
		if err := settings.ValidateGeoUpdater(updater); err != nil {
			return err
		}
	}
	if err := settings.Set(i.Layout.SettingsFile(), settings.KeyGeoUpdate, period); err != nil {
		return err
	}
	if err := settings.Set(i.Layout.SettingsFile(), settings.KeyGeoUpdater, updater); err != nil {
		return err
	}
	return i.SyncTimers(ctx)
}

// SyncTimers 按设置和状态启用或停用 timer：有任一项更新定时执行时启用 tunlr-update.timer；透明代理
// 开启（含正在检测）时启用 tunlr-check.timer，它检查的都是透明代理的事。还没有安装 unit 时什么都不做。
func (i *Installer) SyncTimers(ctx context.Context) error {
	sd := i.systemd()
	if !sd.Installed() {
		return nil
	}
	scheds, err := i.Schedules()
	if err != nil {
		return err
	}
	update := slices.ContainsFunc(scheds, func(s Schedule) bool { return !s.Next.IsZero() })
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return errors.Join(sd.SetTimer(ctx, service.UpdateTimer, update),
		sd.SetTimer(ctx, service.CheckTimer, st != nil && st.TunEnabled()))
}

// UpdateOptions 是 Update 的参数。
type UpdateOptions struct {
	// Tasks 为空时只执行到期的更新（tunlr-update.timer 触发的 _update）；否则不看周期，立即执行这些
	// 任务：TaskSubscription 更新全部订阅（包括不自动更新的），TaskGeo 由 tunlr 更新 geo 数据（即使
	// 设置为由内核自己更新）。
	Tasks []string
	// Progress 在每一步开始时调用，可为空。
	Progress func(step string)
}

// Failed 返回失败的步骤数。
func (r *UpdateResult) Failed() int {
	n := 0
	for _, s := range r.Steps {
		if s.Status == StepFailed {
			n++
		}
	}
	return n
}

// Update 执行更新（见 UpdateOptions.Tasks）：先逐个更新订阅（失败时保留上一次的内容），启用的订阅更新
// 成功后按它和 mixin 发布（同 apply --yes）；再更新 geo 数据。执行了任何任务时最后做面板升级的善后：
// 它不下载内容，是兜底救援，不单独设周期。一步失败时记录原因并继续后面的步骤。订阅的执行情况记在
// 各自的抓取记录中，geo 数据的记入状态文件。有任何一步失败时返回错误，结果中仍包含每一步的情况。
func (i *Installer) Update(ctx context.Context, opts UpdateOptions) (*UpdateResult, error) {
	res := &UpdateResult{Tasks: []string{}, Steps: []Step{}}
	for _, t := range opts.Tasks {
		if !slices.Contains(Tasks, t) {
			return nil, fmt.Errorf("%w: unknown update task %q", core.ErrInvalidArgument, t)
		}
	}
	scheds, err := i.Schedules()
	if err != nil {
		return nil, err
	}
	var subs []string
	var geo bool
	if len(opts.Tasks) == 0 {
		now := i.now()
		for _, s := range scheds {
			switch {
			case !s.Due(now):
			case s.Task == TaskGeo:
				geo = true
			default:
				subs = append(subs, s.Name)
			}
		}
		if len(subs) > 0 {
			res.Tasks = append(res.Tasks, TaskSubscription)
		}
		if geo {
			res.Tasks = append(res.Tasks, TaskGeo)
		}
	} else {
		for _, t := range Tasks {
			if slices.Contains(opts.Tasks, t) {
				res.Tasks = append(res.Tasks, t)
			}
		}
		if slices.Contains(opts.Tasks, TaskSubscription) {
			for _, s := range scheds {
				if s.Task == TaskSubscription {
					subs = append(subs, s.Name)
				}
			}
		}
		geo = slices.Contains(opts.Tasks, TaskGeo)
	}
	if len(res.Tasks) == 0 {
		return res, nil
	}

	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	owner, _ := service.KernelUser(i.Layout)
	push := func(name, status, msg string) Step {
		s := Step{Name: name, Status: status, Message: msg}
		res.Steps = append(res.Steps, s)
		// 结果由调用方输出（定时任务的输出进入 journal），这里只留调试日志。
		i.log().Debug("update step", "name", name, "status", status, "message", msg)
		return s
	}
	add := func(name, title string, f func() (string, string)) Step {
		progress(title)
		status, msg := f()
		return push(name, status, msg)
	}
	var errs []error
	if slices.Contains(res.Tasks, TaskSubscription) {
		if len(subs) == 0 {
			push("subscription", StepSkipped, "no subscriptions")
		} else {
			progress("Updating the subscriptions")
			if i.updateSubscriptions(ctx, subs, push) {
				add("publish", "Publishing the configuration", func() (string, string) { return i.publish(ctx, owner) })
			}
		}
	}
	if geo {
		step := add("geo", "Updating the geo data", func() (string, string) { return i.updateGeo(ctx, owner) })
		errs = append(errs, i.record(ctx, TaskGeo, []Step{step}))
	}
	add("kernel", "Checking the kernel", func() (string, string) { return i.aftercare(ctx, owner) })

	if n := res.Failed(); n > 0 {
		errs = append(errs, fmt.Errorf("%d of %d update steps failed", n, len(res.Steps)))
	}
	return res, errors.Join(errs...)
}

// record 把任务 task 这次执行的情况（各步的结果合在一起）记入状态文件。没有状态文件（还没有安装）时不记。
func (i *Installer) record(ctx context.Context, task string, steps []Step) error {
	run := state.UpdateRun{At: i.now().UTC(), Status: steps[0].Status}
	var msgs []string
	for _, s := range steps {
		if s.Status == StepFailed {
			run.Status = StepFailed
		}
		if s.Message != "" {
			msgs = append(msgs, s.Message)
		}
	}
	run.Message = strings.Join(msgs, "; ")

	l, err := i.lock(ctx)
	if err != nil {
		return err
	}
	defer l.Release()
	st, err := state.Load(i.Layout.StateFile())
	if err != nil {
		return ignoreNotExist(err)
	}
	if st.Updates == nil {
		st.Updates = map[string]state.UpdateRun{}
	}
	st.Updates[task] = run
	return state.Save(i.Layout.StateFile(), st)
}

// updateSubscriptions 重新抓取 names 列出的订阅，每个订阅一步（名为 "sub <名称>"），返回启用的订阅
// 是否抓取成功（成功才需要发布）。
func (i *Installer) updateSubscriptions(ctx context.Context, names []string, push func(name, status, msg string) Step) bool {
	st, err := state.Load(i.Layout.StateFile())
	if err != nil {
		push("subscription", StepFailed, err.Error())
		return false
	}
	subs := &subscription.Manager{Layout: i.Layout, Version: i.Version, Now: i.Now}
	results, err := subs.Update(ctx, names, subscription.UpdateOptions{})
	if results == nil {
		push("subscription", StepFailed, err.Error())
		return false
	}
	activeOK := false
	for _, r := range results {
		if r.Err != nil {
			push("sub "+r.Name, StepFailed, r.Err.Error()+" (keeping the last good copy)")
			continue
		}
		push("sub "+r.Name, StepOK, fmt.Sprintf("%d proxies", r.Summary.Proxies))
		if r.Name == st.Active {
			activeOK = true
		}
	}
	return activeOK
}

func (i *Installer) publish(ctx context.Context, owner *fsutil.Owner) (string, string) {
	p := i.pipeline(owner)
	plan, err := p.Plan(ctx)
	if err != nil {
		return StepFailed, err.Error()
	}
	if !plan.Changed() {
		return StepOK, "no changes"
	}
	if plan.Check != nil {
		return StepFailed, "keeping the current configuration: " + plan.Check.Error()
	}
	applied, err := p.Apply(ctx, plan)
	if err != nil {
		return StepFailed, err.Error()
	}
	msg := fmt.Sprintf("published revision %d", applied.Revision.Number)
	switch {
	case applied.Restarted:
		msg += ", mihomo restarted"
	case applied.Reloaded:
		msg += ", mihomo reloaded it"
	}
	return StepOK, msg
}

func (i *Installer) aftercare(ctx context.Context, owner *fsutil.Owner) (string, string) {
	res, err := i.kernels(owner, mihomo.Arch{}).Aftercare(ctx)
	if err != nil {
		return StepFailed, err.Error()
	}
	switch res.Action {
	case mihomo.AftercareNone:
		return StepSkipped, "mihomo is not installed or not running"
	case mihomo.AftercareKnown:
		return StepOK, "the running kernel is a known-good version"
	case mihomo.AftercareVerified:
		return StepOK, fmt.Sprintf("verified %s (upgraded from the panel) against the official build", res.Version)
	}
	return StepOK, fmt.Sprintf("replaced the kernel with %s: %s", res.Version, res.Reason)
}

// updateGeo 让内核经 geox-url 更新 geo 数据（无需重启即生效），再按官方 sha256 核对；
// 不一致的换成校验过的文件并重启内核。内核下载失败时本次跳过，留给下一次。
func (i *Installer) updateGeo(ctx context.Context, owner *fsutil.Owner) (string, string) {
	err := mihomo.NewClient(i.Layout.CoreSocket()).UpdateGeo(ctx)
	if errors.Is(err, mihomo.ErrNotRunning) {
		return StepSkipped, "mihomo is not running"
	}
	if err != nil {
		return StepFailed, "mihomo could not update the geo data, will try again next time: " + err.Error()
	}
	d := i.Download
	if d == nil {
		set, err := settings.Load(i.Layout.SettingsFile())
		if err != nil {
			return StepFailed, err.Error()
		}
		d = &download.Client{Mirror: set.GitHubMirror}
	}
	replaced, err := mihomo.VerifyGeo(ctx, d, i.Layout.CoreDir(), mihomo.AllGeoFiles, owner)
	if err != nil {
		return StepFailed, "verify the geo data: " + err.Error()
	}
	if len(replaced) == 0 {
		return StepOK, "geo data updated and verified"
	}
	msg := "replaced " + strings.Join(replaced, ", ") + " with verified copies"
	if _, err := i.kernels(owner, mihomo.Arch{}).Restart(ctx); err != nil {
		return StepFailed, msg + "; restarting mihomo failed: " + err.Error()
	}
	return StepOK, msg + " and restarted mihomo"
}
