package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

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
	Steps []Step `json:"steps"`
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

// Update 是 tunlr-update.timer 每天执行的更新，依次：更新启用的订阅 → 按当前订阅和 mixin 发布
// （同 apply --yes）→ 面板升级的善后 → geo 数据更新。一步失败时记录原因并继续后面的步骤；
// 有任何一步失败时返回错误，结果中仍包含每一步的情况。
func (i *Installer) Update(ctx context.Context) (*UpdateResult, error) {
	owner, _ := service.KernelUser(i.Layout)
	res := &UpdateResult{}
	add := func(name string, f func() (string, string)) {
		status, msg := f()
		res.Steps = append(res.Steps, Step{Name: name, Status: status, Message: msg})
		// 结果由调用方输出（定时任务的输出进入 journal），这里只留调试日志。
		i.log().Debug("update step", "name", name, "status", status, "message", msg)
	}

	add("subscription", func() (string, string) { return i.updateSubscription(ctx) })
	add("publish", func() (string, string) { return i.publish(ctx, owner) })
	add("kernel", func() (string, string) { return i.aftercare(ctx, owner) })
	add("geo", func() (string, string) { return i.updateGeo(ctx, owner) })

	if n := res.Failed(); n > 0 {
		return res, fmt.Errorf("%d of %d update steps failed", n, len(res.Steps))
	}
	return res, nil
}

func (i *Installer) updateSubscription(ctx context.Context) (string, string) {
	st, err := state.Load(i.Layout.StateFile())
	if errors.Is(err, fs.ErrNotExist) || (err == nil && st.Active == "") {
		return StepSkipped, "no active subscription"
	}
	if err != nil {
		return StepFailed, err.Error()
	}
	subs := &subscription.Manager{Layout: i.Layout, Version: i.Version, Now: i.Now}
	results, err := subs.Update(ctx, []string{st.Active}, subscription.UpdateOptions{})
	if len(results) == 1 && results[0].Err != nil {
		return StepFailed, fmt.Sprintf("%s: %v (keeping the last good copy)", st.Active, results[0].Err)
	}
	if err != nil {
		return StepFailed, err.Error()
	}
	return StepOK, fmt.Sprintf("%s: %d proxies", st.Active, results[0].Summary.Proxies)
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
