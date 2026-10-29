package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
	"github.com/tunlr-app/cli/templates"
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

// 定时更新的任务：订阅（每个订阅按自己的周期更新，启用的订阅更新后发布）和 geo 数据。tunlr 自身的
// 升级不做定时任务，只能手动执行。
//
// geo 数据由内核按配置中的 geo-auto-update、geo-update-interval 自己下载；tunlr 的任务是在内核更新过
// 文件之后，按 geox-url 所指的 GitHub release 资产的官方 sha256 核对（见 updateGeo）。
const (
	TaskSubscription = "subscription"
	TaskGeo          = "geo"
)

// Tasks 是全部定时更新任务，按执行顺序。
var Tasks = []string{TaskSubscription, TaskGeo}

// geoRetry 是 geo 数据核对失败后再试的间隔。
const geoRetry = 6 * time.Hour

// Schedule 是一项定时更新（一个订阅，或 geo 数据）的周期和执行情况。
type Schedule struct {
	Task string `json:"task"`
	// Name 是订阅的名称，Active 报告它是否为启用的订阅；geo 数据没有。
	Name   string `json:"name,omitempty"`
	Active bool   `json:"active,omitempty"`
	// Period 是周期，如 "1d"；settings.PeriodOff 表示不定时更新。geo 数据的取自生效的配置
	// （geo-auto-update、geo-update-interval）。
	Period string `json:"period"`
	// Last 是 tunlr 最近一次执行的情况，没有执行过时为 nil。geo 数据是最近一次核对。
	Last *state.UpdateRun `json:"last,omitempty"`
	// Updated 是内核最近一次更新 geo 数据的时间（最新的 geo 文件的修改时间，内核不报告它的更新结果）；
	// 还没有 geo 文件或是订阅时为零值。
	Updated time.Time `json:"updated,omitzero"`
	// Next 是到期时间，tunlr-update.timer 每小时唤醒一次，到期后的第一次唤醒时执行。订阅见
	// subscription.NextUpdate；geo 数据是内核下一次更新的大致时间（Updated 之后再过一个周期，由内核
	// 决定）。不定时更新时为零值。
	Next time.Time `json:"next,omitzero"`

	// check 报告 geo 数据需要核对：内核更新过文件（Updated 晚于上次核对），或上次核对失败已过 geoRetry。
	check bool
}

// Due 报告任务在 now 是否到期。geo 数据由内核按自己的周期更新，tunlr 只在它更新之后核对。
func (s Schedule) Due(now time.Time) bool {
	if s.Task == TaskGeo {
		return s.check
	}
	return !s.Next.IsZero() && !now.Before(s.Next)
}

// Schedules 返回各项定时更新的周期和执行情况：先是各订阅（按添加顺序），最后是 geo 数据。
func (i *Installer) Schedules() ([]Schedule, error) {
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

	geo, err := i.geoSchedule(st)
	if err != nil {
		return nil, err
	}
	return append(out, geo), nil
}

// geoSchedule 返回 geo 数据的更新情况：周期取自生效的配置，还没有发布过时为模板的默认值。
func (i *Installer) geoSchedule(st *state.State) (Schedule, error) {
	geo := Schedule{Task: TaskGeo, Period: settings.PeriodOff}
	_, update, err := i.geoSettings()
	if err != nil {
		return geo, err
	}
	if update.Auto {
		geo.Period = periodOf(update.Interval)
	}
	if geo.Updated, err = mihomo.GeoModTime(i.Layout.CoreDir(), mihomo.AllGeoFiles); err != nil {
		return geo, err
	}
	geo.Updated = geo.Updated.UTC()
	if !geo.Updated.IsZero() && update.Auto {
		geo.Next = geo.Updated.Add(update.Interval)
	}
	if st != nil {
		if run, ok := st.Updates[TaskGeo]; ok {
			geo.Last = &run
		}
	}
	switch {
	case geo.Updated.IsZero():
	case geo.Last == nil:
		geo.check = true
	case geo.Updated.After(geo.Last.At):
		geo.check = true
	case geo.Last.Status == StepFailed:
		geo.check = !i.now().Before(geo.Last.At.Add(geoRetry))
	}
	return geo, nil
}

// geoSettings 读出生效的配置中 geo 数据的来源和自动更新的设置；还没有发布过时用模板中的。
func (i *Installer) geoSettings() (config.GeoNeeds, config.GeoUpdate, error) {
	cfg, err := os.ReadFile(i.Layout.CoreConfig())
	if errors.Is(err, fs.ErrNotExist) {
		cfg, err = templates.Config, nil
	}
	if err != nil {
		return config.GeoNeeds{}, config.GeoUpdate{}, err
	}
	return config.GeoSettings(cfg)
}

// periodOf 把间隔写成周期设置的写法：整天的写成天数（"7d"），否则写成小时数（"6h"）。
func periodOf(d time.Duration) string {
	h := int(d / time.Hour)
	if h%24 == 0 {
		return fmt.Sprintf("%dd", h/24)
	}
	return fmt.Sprintf("%dh", h)
}

// SyncTimers 按状态启用或停用 timer：tunlr-update.timer 总是启用，即使没有订阅定时更新、配置中也关闭了
// geo 数据的自动更新：面板随时可以让内核更新 geo 数据或升级内核，之后都要核对（没有要做的事时它什么都
// 不输出）；透明代理开启（含正在检测）时启用 tunlr-check.timer，它检查的都是透明代理的事。还没有安装
// unit 时什么都不做。
func (i *Installer) SyncTimers(ctx context.Context) error {
	sd := i.systemd()
	if !sd.Installed() {
		return nil
	}
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return errors.Join(sd.SetTimer(ctx, service.UpdateTimer, true),
		sd.SetTimer(ctx, service.CheckTimer, st != nil && st.TunEnabled()))
}

// UpdateOptions 是 Update 的参数。
type UpdateOptions struct {
	// Tasks 为空时只执行到期的更新（tunlr-update.timer 触发的 _update）；否则不看周期，立即执行这些
	// 任务：TaskSubscription 更新全部订阅（包括不自动更新的），TaskGeo 让内核立即更新 geo 数据再核对
	// （即使配置中关闭了自动更新）。
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
	manual := len(opts.Tasks) > 0
	if !manual {
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
		title := "Verifying the geo data"
		if manual {
			title = "Updating the geo data"
		}
		step := add("geo", title, func() (string, string) { return i.updateGeo(ctx, owner, manual) })
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

// updateGeo 按生效的配置中的 geox-url 核对工作目录中的 geo 文件：来源是 GitHub release 资产的按官方
// sha256 核对，不一致的换成校验过的文件并重启内核（内核的 geo 数据只能由它自己的更新器重新加载）；
// 其他来源无从校验，原样保留。fetch 为 true 时（手动执行）先让内核经 geox-url 立即更新（无需重启即
// 生效）：内核没有运行时只核对已有的文件，下载失败时本次不核对，留给下一次。
func (i *Installer) updateGeo(ctx context.Context, owner *fsutil.Owner, fetch bool) (string, string) {
	client := mihomo.NewClient(i.Layout.CoreSocket())
	var msgs []string
	if fetch {
		err := client.UpdateGeo(ctx)
		switch {
		case errors.Is(err, mihomo.ErrNotRunning):
			msgs = append(msgs, "mihomo is not running, so nothing was downloaded")
		case err != nil:
			return StepFailed, "mihomo could not update the geo data, will try again next time: " + err.Error()
		}
	}
	geo, _, err := i.geoSettings()
	if err != nil {
		return StepFailed, err.Error()
	}
	d := i.Download
	if d == nil {
		set, err := settings.Load(i.Layout.SettingsFile())
		if err != nil {
			return StepFailed, err.Error()
		}
		d = &download.Client{Mirror: set.GitHubMirror}
	}
	check, err := mihomo.VerifyGeo(ctx, d, i.Layout.CoreDir(), mihomo.AllGeoFiles, geo.URLs, owner)
	if err != nil {
		return StepFailed, "verify the geo data: " + err.Error()
	}
	if len(check.Verified) > 0 {
		msgs = append(msgs, "verified "+strings.Join(check.Verified, ", "))
	}
	if len(check.Unverified) > 0 {
		msgs = append(msgs, strings.Join(check.Unverified, ", ")+" from a custom source, not verified")
	}
	if len(check.Verified)+len(check.Unverified)+len(check.Replaced) == 0 {
		return StepSkipped, strings.Join(append(msgs, "no geo data yet"), "; ")
	}
	if len(check.Replaced) == 0 {
		return StepOK, strings.Join(msgs, "; ")
	}
	msgs = append(msgs, "replaced "+strings.Join(check.Replaced, ", ")+" with verified copies")
	msg := strings.Join(msgs, "; ")
	// 内核没有运行时不启动它，下次启动时自然加载新文件。
	if _, err := client.Version(ctx); errors.Is(err, mihomo.ErrNotRunning) {
		return StepOK, msg
	}
	if _, err := i.kernels(owner, mihomo.Arch{}).Restart(ctx); err != nil {
		return StepFailed, msg + "; restarting mihomo failed: " + err.Error()
	}
	return StepOK, msg + " and restarted mihomo"
}
