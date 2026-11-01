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
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
	"github.com/tunlr-app/cli/templates"
)

// 定时更新各步骤、tun on 各项检测的结果。
const (
	StepOK      = "ok"
	StepSkipped = "skipped"
	StepFailed  = "failed"
	// StepWarn 只用于 tun on 的检测：有问题，但不关闭 TUN。
	StepWarn = "warn"
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

// 更新的任务：订阅（每个订阅按自己的周期更新，启用的订阅更新后发布）和 geo 数据。tunlr 自身的
// 升级不做定时任务，只能手动执行。
//
// geo 数据由内核按配置中的 geo-auto-update、geo-update-interval 自己下载，tunlr 不下载也不校验，
// 不定时执行；TaskGeo 只用于手动让内核立即更新（tunlr update run geo）。
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
	// Period 是周期，如 "1d"；settings.PeriodOff 表示不定时更新。geo 数据的取自生效的配置
	// （geo-auto-update、geo-update-interval）。
	Period string `json:"period"`
	// Last 是 tunlr 最近一次执行的情况，没有执行过时为 nil。geo 数据没有：由内核更新。
	Last *state.UpdateRun `json:"last,omitempty"`
	// Updated 是内核最近一次更新 geo 数据的时间（最新的 geo 文件的修改时间，内核不报告它的更新结果）；
	// 还没有 geo 文件或是订阅时为零值。
	Updated time.Time `json:"updated,omitzero"`
	// Next 是到期时间，tunlr-update.timer 每小时唤醒一次，到期后的第一次唤醒时执行。订阅见
	// subscription.NextUpdate；geo 数据是内核下一次更新的大致时间（Updated 之后再过一个周期，由内核
	// 决定）。不定时更新时为零值。
	Next time.Time `json:"next,omitzero"`
	// Sources 是 geo 数据的设置（config.GeoKeys）在要发布的配置中的值和来源；只有 geo 数据有，且只在调用方
	// 要展示时由 GeoSources 填写（要合成配置，定时更新不需要）。
	Sources []config.KeySource `json:"sources,omitempty"`
}

// Due 报告任务在 now 是否到期，由 tunlr-update.timer 执行。geo 数据由内核按自己的周期更新，从不到期。
func (s Schedule) Due(now time.Time) bool {
	if s.Task == TaskGeo {
		return false
	}
	return !s.Next.IsZero() && !now.Before(s.Next)
}

// Schedules 返回各项定时更新的周期和执行情况：先是各订阅（按添加顺序），最后是 geo 数据。
func (i *Installer) Schedules() ([]Schedule, error) {
	subs, err := i.subscriptions().List()
	if err != nil {
		return nil, err
	}
	var out []Schedule
	for _, s := range subs {
		sched := Schedule{Task: TaskSubscription, Name: s.Name, Active: s.Active, Period: s.UpdatePeriod(), Next: subscription.NextUpdate(&s.Subscription)}
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

	geo, err := i.geoSchedule()
	if err != nil {
		return nil, err
	}
	return append(out, geo), nil
}

// geoSchedule 返回 geo 数据的更新情况：周期取自生效的配置，还没有发布过时为模板的默认值。
func (i *Installer) geoSchedule() (Schedule, error) {
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
	return geo, nil
}

// geoSettings 读出生效的配置中 geo 数据的来源和自动更新的设置；还没有发布过时用模板和内置的兜底值合成的。
func (i *Installer) geoSettings() (config.GeoNeeds, config.GeoUpdate, error) {
	cfg, err := os.ReadFile(i.Layout.CoreConfig())
	if errors.Is(err, fs.ErrNotExist) {
		var res *config.Result
		res, err = config.Compose(config.Layers{Template: templates.Config, Managed: []config.Managed{pipeline.DefaultsManaged()}})
		if err == nil {
			cfg = res.Config
		}
	}
	if err != nil {
		return config.GeoNeeds{}, config.GeoUpdate{}, err
	}
	return config.GeoSettings(cfg)
}

// GeoSources 按订阅 sub（为空时为启用的订阅）启用时合成配置（不校验），返回 geo 数据的设置（config.GeoKeys）
// 的值和来源：兜底值（tunlr:defaults 或 mixin 中的 tunlr.defaults）、订阅或 mixin。
func (i *Installer) GeoSources(ctx context.Context, sub string) ([]config.KeySource, error) {
	plan, err := i.pipeline(nil).Compose(ctx, sub)
	if err != nil {
		return nil, err
	}
	return plan.GeoSources, nil
}

// periodOf 把间隔写成周期设置的写法：整天的写成天数（"7d"），否则写成小时数（"6h"）。
func periodOf(d time.Duration) string {
	h := int(d / time.Hour)
	if h%24 == 0 {
		return fmt.Sprintf("%dd", h/24)
	}
	return fmt.Sprintf("%dh", h)
}

// SyncTimers 按状态启用或停用 timer 和 path unit：tunlr-update.timer 总是启用（随时可能添加订阅，没有到期的
// 更新时它什么都不做）；tunlr-aftercare.path 总是启用，面板随时可以升级内核；tunlr-firewall.service 总是
// 启用，随 nftables.service 重新加载工具的表（没有要加载的时什么都不做）；透明代理、网关或局域网访问开着时
// 启用 tunlr-check.timer（见 checkWanted）。还没有安装 unit 时什么都不做。
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
		sd.SetTimer(ctx, service.AftercarePath, true),
		sd.SetTimer(ctx, service.FirewallUnit, true),
		sd.SetTimer(ctx, service.CheckTimer, checkWanted(st)))
}

// UpdateOptions 是 Update 的参数。
type UpdateOptions struct {
	// Tasks 为空时只执行到期的更新（tunlr-update.timer 触发的 _update）；否则不看周期，立即执行这些
	// 任务：TaskSubscription 更新全部订阅（包括不自动更新的），TaskGeo 让内核按 geox-url 立即更新 geo
	// 数据（即使配置中关闭了自动更新）。
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
// 成功后按它和 mixin 发布（同 apply --yes）；再让内核更新 geo 数据（只在手动执行时）。没有到期的任务时
// 不留下任何结果。一步失败时记录原因并继续后面的步骤。订阅的执行情况记在各自的抓取记录中。有任何一步失败时返回错误，结果中仍包含每一步的情况。
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
		add("geo", "Having mihomo update the geo data", func() (string, string) { return i.updateGeo(ctx) })
	}
	if !manual {
		// 每天查一次 tunlr 和 mihomo 的最新版本，菜单和 status 据此提示；不是更新步骤，失败只记日志。
		if uc, checked, err := i.CheckUpdates(ctx); err != nil {
			i.log().Warn("check for updates", "error", err)
		} else if checked {
			i.log().Info("checked for updates", "tunlr", uc.Tool, "mihomo", uc.Kernel)
		}
	}
	if n := res.Failed(); n > 0 {
		errs = append(errs, fmt.Errorf("%d of %d update steps failed", n, len(res.Steps)))
	}
	return res, errors.Join(errs...)
}

// updateSubscriptions 重新抓取 names 列出的订阅，每个订阅一步（名为 "sub <名称>"），返回启用的订阅
// 是否抓取成功（成功才需要发布）。
func (i *Installer) updateSubscriptions(ctx context.Context, names []string, push func(name, status, msg string) Step) bool {
	list, err := subscription.LoadList(i.Layout.SubscriptionList())
	if err != nil {
		push("subscription", StepFailed, err.Error())
		return false
	}
	results, err := i.subscriptions().Update(ctx, names, subscription.UpdateOptions{})
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
		if r.Name == list.Active {
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

// Aftercare 核对换过的内核（tunlr-aftercare.path 在 bin/mihomo 变化时触发的 _aftercare，见
// mihomo.Manager.Aftercare），返回一步结果（名为 kernel）；失败时同时返回错误。
func (i *Installer) Aftercare(ctx context.Context) (Step, error) {
	owner, _ := service.KernelUser(i.Layout)
	res, err := i.kernels(owner, mihomo.Arch{}).Aftercare(ctx)
	s := Step{Name: "kernel", Status: StepOK}
	switch {
	case err != nil:
		s.Status, s.Message = StepFailed, err.Error()
	case res.Action == mihomo.AftercareNone:
		s.Status, s.Message = StepSkipped, "mihomo is not installed or not running; it is checked when it starts"
	case res.Action == mihomo.AftercareKnown:
		s.Message = "bin/mihomo is a known-good version"
	case res.Action == mihomo.AftercareVerified:
		s.Message = fmt.Sprintf("verified %s (upgraded from the panel) against the official build", res.Version)
	default:
		s.Message = fmt.Sprintf("replaced the kernel with %s: %s", res.Version, res.Reason)
	}
	return s, err
}

// updateGeo 让内核按生效的配置中的 geox-url 立即更新 geo 数据，内核自己加载，无需重启。tunlr 不校验内核
// 下载的文件。内核不报告哪些文件换了（内容与上游相同时它不写文件），由更新前后文件的修改时间和大小得出。
func (i *Installer) updateGeo(ctx context.Context) (string, string) {
	home := i.Layout.CoreDir()
	before, _ := mihomo.GeoStamps(home, mihomo.AllGeoFiles)
	err := mihomo.NewClient(i.Layout.CoreSocket()).UpdateGeo(ctx)
	switch {
	case errors.Is(err, mihomo.ErrNotRunning):
		return StepSkipped, "mihomo is not running; it downloads missing geo data when it starts"
	case err != nil:
		return StepFailed, "mihomo could not update the geo data: " + err.Error()
	}
	after, err := mihomo.GeoStamps(home, mihomo.AllGeoFiles)
	if err != nil || before == nil {
		return StepOK, "mihomo updated the geo data from geox-url"
	}
	return StepOK, describeGeoUpdate(before, after)
}

// describeGeoUpdate 按文件说明内核更新 geo 数据的结果，如
// "mihomo updated GeoSite.dat; geoip.metadb was already up to date"。
func describeGeoUpdate(before, after map[string]string) string {
	var updated, same []string
	for _, f := range mihomo.AllGeoFiles {
		s, ok := after[f.Name]
		switch {
		case !ok:
		case s != before[f.Name]:
			updated = append(updated, f.Name)
		default:
			same = append(same, f.Name)
		}
	}
	switch {
	case len(updated) == 0 && len(same) == 0:
		return "mihomo has no geo data files yet; it downloads them when the configuration uses them"
	case len(updated) == 0:
		return "the geo data was already up to date (" + strings.Join(same, ", ") + ")"
	case len(same) == 0:
		return "mihomo updated " + strings.Join(updated, ", ")
	}
	verb := "was"
	if len(same) > 1 {
		verb = "were"
	}
	return "mihomo updated " + strings.Join(updated, ", ") + "; " + strings.Join(same, ", ") + " " + verb + " already up to date"
}
