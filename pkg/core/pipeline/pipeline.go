// Package pipeline 实现配置流水线：合成 → 校验 → 预览差异 → 原子发布 → 记录历史，以及回滚。
//
// 一次发布分两步：Plan 计算要发布的配置、与生效配置的差异和 mihomo -t 的结果，不改动任何
// 状态（只可能补齐校验所需的 geo 数据）；用户确认后 Apply 在文件锁内发布这个 Plan。
// 两步之间生效的配置被改动时，Apply 拒绝发布，避免覆盖用户没有看过的变化。
package pipeline

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/lock"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/templates"
)

// DefaultKeep 是保留的历史修订数。
const DefaultKeep = 10

// uiTimeout 是安装面板的时限。面板不是必需的，下载太慢时放弃，交给内核。
const uiTimeout = 5 * time.Minute

// lockWait 是等待其他 tunlr 操作释放锁的最长时间。
const lockWait = 30 * time.Second

var (
	// ErrStale 表示生效的配置在 Plan 之后被改动过，需要重新预览。
	ErrStale = errors.New("the published configuration changed after it was planned; run the command again")
	// ErrNothingToRollBack 表示历史中没有与生效配置不同的修订。
	ErrNothingToRollBack = errors.New("no earlier revision to roll back to")
)

// Validator 校验配置，通常是 *mihomo.Validator。geo 是配置需要的 geo 数据，为 nil 时由 Validator 自己判断。
type Validator interface {
	Validate(ctx context.Context, cfg []byte, geo *config.GeoNeeds) error
}

// Reloader 让运行中的内核加载已写入磁盘的配置。
type Reloader interface {
	Reload(ctx context.Context) error
}

// Restarter 重启由服务运行的内核并做健康检查，通常是 *mihomo.Manager。服务没有安装，或 ifRunning
// 时服务没有运行，返回 mihomo.ErrNotRunning。调用时 Apply 持有文件锁。
type Restarter interface {
	RestartLocked(ctx context.Context, ifRunning bool) error
}

// Pipeline 执行配置的预览、发布与回滚。
type Pipeline struct {
	Layout paths.Layout
	// Version 是 tunlr 的版本，记录在历史修订中。
	Version string
	// Validator 为空时使用 Layout.CoreBinary() 的 mihomo -t，缺失的 geo 数据按工具设置中的镜像下载。
	Validator Validator
	// Download 为空时按工具设置中的镜像下载（geo 数据、面板）。
	Download *download.Client
	// Reloader 为空时经 Layout.CoreSocket() 的 API 让内核重新加载 Layout.CoreConfig()。
	// 返回 mihomo.ErrNotRunning 表示内核没有运行，配置会在内核启动时生效。
	Reloader Reloader
	// Restarter 用于需要重启内核才能生效的变化（见 config.RestartKeys），为空时经 systemd 重启
	// tunlr-core.service。
	Restarter Restarter
	// Owner 是内核用户：内核工作目录及其中的文件归它所有，mihomo -t 也以它运行。
	// 为 nil 时用 service.KernelUser，用户尚不存在时（尚未 init）保持当前身份。
	Owner *fsutil.Owner
	// Keep 为零时使用 DefaultKeep。
	Keep int
	// Now 为空时使用 time.Now。
	Now func() time.Time
}

// 计划的类型。
const (
	KindApply    = "apply"
	KindRollback = "rollback"
)

// Plan 是一次待发布的配置及其预览信息。
type Plan struct {
	Kind string
	// Config 是要发布的配置。
	Config []byte
	// Current 是生效的配置，尚未发布过时为 nil。
	Current []byte
	// Diff 是 Current 到 Config 的统一格式差异；尚未发布过或没有变化时为空。
	Diff string
	// DiffSections 按顶层键汇总 Diff 增删的行数。
	DiffSections []config.DiffSection
	// Subscription 是配置所用的订阅，没有启用的订阅时为空。
	Subscription string
	// Dropped 是订阅中被丢弃的顶层键（仅 KindApply）。
	Dropped []string
	// Mixins 是按应用顺序排列的 mixin 文件名（仅 KindApply）。
	Mixins []string
	// Warnings 是合成时发现、不阻止发布的问题（仅 KindApply）。
	Warnings []config.Warning
	// Overrides 是后面的 mixin 文件覆盖前面文件所设置的键（仅 KindApply）。
	Overrides []config.Override
	// Managed 是 tunlr 的交互功能生成的受管 mixin，按 Managed 函数的顺序排列（仅 KindApply）。
	// 其中含 API secret，展示时只列出键。
	Managed []config.Managed
	// RollbackTo 是回滚的目标修订（仅 KindRollback）。
	RollbackTo int
	// Restart 是有变化、需要重启内核才能生效的顶层键；不为空时发布后重启内核而不是热重载。
	Restart []string
	// Proxies 汇总节点的变化，尚未发布过、没有变化或无法解析时为 nil。
	Proxies *config.ProxyChanges
	// Check 是校验结果，nil 表示通过；没有变化时不校验，也为 nil。
	// 配置有误时为 *mihomo.CheckError，内核未安装时满足 errors.Is(err, mihomo.ErrNotInstalled)。
	Check error

	// newSecret 是本次生成、尚未保存的 API secret，oldSecret 是生成时状态文件中的 secret（第一次发布时为空）。
	newSecret, oldSecret string
	// facts 是从 Config 中提取的信息。
	facts *config.Facts
}

// Changed 报告 Config 与生效的配置是否不同。
func (p *Plan) Changed() bool {
	return p.Current == nil || !bytes.Equal(p.Current, p.Config)
}

// Plan 用启用的订阅、mixin 和工具设置合成新配置，与生效的配置比较并校验。
// mixin 有错时返回 *config.MixinError。
func (p *Pipeline) Plan(ctx context.Context) (*Plan, error) {
	return p.plan(ctx, planOptions{})
}

// PlanWithMixin 同 Plan，但用 m 代替磁盘上的同名 mixin 文件（不存在时按文件名顺序加入），
// 用于在保存编辑结果之前检查它。
func (p *Pipeline) PlanWithMixin(ctx context.Context, m config.Mixin) (*Plan, error) {
	return p.plan(ctx, planOptions{edited: &m})
}

// PlanTun 同 Plan，但按透明代理开关为 on 时的样子合成，不看状态文件中的开关。
// 用于开关之前预览和校验；调用方在发布前保存开关。
func (p *Pipeline) PlanTun(ctx context.Context, on bool) (*Plan, error) {
	return p.plan(ctx, planOptions{tun: &on})
}

// APIPort 是内核 API（面板）的端口，与模板中的 external-controller 一致。
const APIPort = 9090

// LANController 是面板的局域网访问开启时 external-controller 的值：监听所有 IPv4 地址，
// 来源由 nft 表 inet tunlr_panel 限制为私有地址段。
var LANController = fmt.Sprintf("0.0.0.0:%d", APIPort)

// PlanPanelLAN 同 Plan，但按面板局域网访问开关为 on 时的样子合成，不看状态文件中的开关。调用方在发布前
// 保存开关。
func (p *Pipeline) PlanPanelLAN(ctx context.Context, on bool) (*Plan, error) {
	return p.plan(ctx, planOptions{lan: &on})
}

// PlanProxyLAN 同 Plan，但按代理端口局域网访问开关为 on 时的样子合成，不看状态文件中的开关。调用方在
// 发布前保存开关。
func (p *Pipeline) PlanProxyLAN(ctx context.Context, on bool) (*Plan, error) {
	return p.plan(ctx, planOptions{proxyLAN: &on})
}

// PlanDevices 同 Plan，但按设备记录为 devices 时的样子合成，不看状态文件中的记录。调用方在发布前保存记录。
// devices 为空切片（不是 nil）表示没有记录。
func (p *Pipeline) PlanDevices(ctx context.Context, devices []state.Device) (*Plan, error) {
	if devices == nil {
		devices = []state.Device{}
	}
	return p.plan(ctx, planOptions{devices: devices})
}

// PlanWithSettings 同 Plan，但按工具设置为 set 时的样子合成，不读设置文件。用于编辑设置时预览。
func (p *Pipeline) PlanWithSettings(ctx context.Context, set *settings.Settings) (*Plan, error) {
	return p.plan(ctx, planOptions{settings: set})
}

// PlanFilter 同 Plan，但按全局过滤项为 f 时的样子合成，不看状态文件中的过滤项。调用方在发布前保存它。
func (p *Pipeline) PlanFilter(ctx context.Context, f state.Filter) (*Plan, error) {
	return p.plan(ctx, planOptions{filter: &f})
}

// PlanResetSecret 同 Plan，但换用新生成的 API secret，发布时保存。
func (p *Pipeline) PlanResetSecret(ctx context.Context) (*Plan, error) {
	return p.plan(ctx, planOptions{resetSecret: true})
}

// CheckMixins 只合成配置、不校验也不比较，检查 mixin 有没有错、有没有动 tunlr 独占的键；
// 有时返回 *config.MixinError。用于 doctor。
func (p *Pipeline) CheckMixins(ctx context.Context) error {
	_, err := p.plan(ctx, planOptions{composeOnly: true})
	return err
}

// Managed 返回按状态文件和工具设置生成的受管 mixin，与发布时所用的相同；还没有生成过 API secret 时
// secret 为空。用于展示（tunlr mixin list）。
func (p *Pipeline) Managed() ([]config.Managed, error) {
	set, err := settings.Load(p.Layout.SettingsFile())
	if err != nil {
		return nil, err
	}
	st, _, err := state.LoadOrNew(p.Layout.StateFile(), p.now())
	if err != nil {
		return nil, err
	}
	return managed(st.APISecret, switchesOf(st), set), nil
}

// switches 是决定受管 mixin 内容的开关。
type switches struct {
	tun, panelLAN, proxyLAN bool
	// direct 是走法为直连的设备的地址。
	direct []string
	filter state.Filter
}

func switchesOf(st *state.State) switches {
	return switches{tun: st.TunEnabled(), panelLAN: st.PanelLAN != nil, proxyLAN: st.ProxyLAN != nil, direct: st.DirectIPs(), filter: st.Filter}
}

type planOptions struct {
	// edited 不为 nil 时代替磁盘上的同名 mixin 文件。
	edited *config.Mixin
	// tun 不为 nil 时代替状态文件中的透明代理开关。
	tun *bool
	// resetSecret 换用新生成的 API secret。
	resetSecret bool
	// lan 不为 nil 时代替状态文件中的面板局域网访问开关。
	lan *bool
	// proxyLAN 不为 nil 时代替状态文件中的代理端口局域网访问开关。
	proxyLAN *bool
	// devices 不为 nil 时代替状态文件中的设备记录。
	devices []state.Device
	// filter 不为 nil 时代替状态文件中的全局过滤项。
	filter *state.Filter
	// settings 不为 nil 时代替设置文件。
	settings *settings.Settings
	// composeOnly 只合成，不检查端口安全，不比较也不校验。
	composeOnly bool
	// noValidate 合成、检查端口安全并与生效的配置比较，但不校验（不运行 mihomo -t）。
	noValidate bool
}

func (p *Pipeline) plan(ctx context.Context, opts planOptions) (*Plan, error) {
	edited := opts.edited
	set := opts.settings
	if set == nil {
		var err error
		if set, err = settings.Load(p.Layout.SettingsFile()); err != nil {
			return nil, err
		}
	}
	st, _, err := state.LoadOrNew(p.Layout.StateFile(), p.now())
	if err != nil {
		return nil, err
	}
	plan := &Plan{Kind: KindApply, Subscription: st.Active}

	secret := st.APISecret
	if secret == "" || opts.resetSecret {
		if secret, err = newSecret(); err != nil {
			return nil, err
		}
		plan.newSecret, plan.oldSecret = secret, st.APISecret
	}
	sw := switchesOf(st)
	if opts.tun != nil {
		sw.tun = *opts.tun
	}
	if opts.lan != nil {
		sw.panelLAN = *opts.lan
	}
	if opts.proxyLAN != nil {
		sw.proxyLAN = *opts.proxyLAN
	}
	if opts.devices != nil {
		sw.direct = (&state.State{Devices: opts.devices}).DirectIPs()
	}
	if opts.filter != nil {
		sw.filter = *opts.filter
	}
	var sub []byte
	if st.Active != "" {
		sub, err = os.ReadFile(p.Layout.SubscriptionFile(st.Active))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("the active subscription %q has no content yet; run: tunlr sub update %s", st.Active, st.Active)
		}
		if err != nil {
			return nil, err
		}
	}
	mixins, err := config.LoadMixins(p.Layout.MixinDir())
	if err != nil {
		return nil, err
	}
	if edited != nil {
		i, found := slices.BinarySearchFunc(mixins, edited.Name, func(m config.Mixin, name string) int {
			return strings.Compare(m.Name, name)
		})
		if found {
			mixins[i] = *edited
		} else {
			mixins = slices.Insert(mixins, i, *edited)
		}
	}
	for _, m := range mixins {
		plan.Mixins = append(plan.Mixins, m.Name)
	}
	plan.Managed = managed(secret, sw, set)
	res, err := config.Compose(config.Layers{
		Template:     templates.Config,
		Subscription: sub,
		Managed:      plan.Managed,
		Mixins:       mixins,
	})
	if err != nil || opts.composeOnly {
		return nil, err
	}
	// 只在重新合成时检查端口安全；回滚和重写独占的键不经过这里，以免旧配置无法恢复。
	if err := config.CheckExposure(res.Config); err != nil {
		return nil, err
	}
	plan.Config, plan.Dropped, plan.facts = res.Config, res.Dropped, res.Facts
	plan.Warnings, plan.Overrides = res.Warnings, res.Overrides
	if opts.noValidate {
		if plan.Current, err = p.readCurrent(); err != nil {
			return nil, err
		}
		if plan.Current != nil && plan.Changed() {
			plan.Diff, plan.DiffSections = config.Diff("current", "new", plan.Current, plan.Config)
		}
		return plan, nil
	}
	if err := p.finish(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// PlanEnforce 准备只按状态文件重写生效配置中受管 mixin 独占的键，其余内容不变；还没有发布过时返回 nil。
// 用于自动关闭透明代理：不重新合成，期间订阅或 mixin 的变化不会混进来，也不会因它们校验失败。
func (p *Pipeline) PlanEnforce(ctx context.Context) (*Plan, error) {
	current, err := p.readCurrent()
	if err != nil || current == nil {
		return nil, err
	}
	st, _, err := state.LoadOrNew(p.Layout.StateFile(), p.now())
	if err != nil {
		return nil, err
	}
	set, err := settings.Load(p.Layout.SettingsFile())
	if err != nil {
		return nil, err
	}
	plan := &Plan{Kind: KindApply, Subscription: st.Active}
	if plan.Config, plan.facts, err = config.Enforce(current, managed(st.APISecret, switchesOf(st), set)); err != nil {
		return nil, fmt.Errorf("the published configuration: %w", err)
	}
	if err := p.finish(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// PlanRollback 准备恢复历史修订 n。n 为 0 时选择最近一个与生效配置不同的修订，
// 因此连续回滚会在最近两个版本之间切换。
func (p *Pipeline) PlanRollback(ctx context.Context, n int) (*Plan, error) {
	h := p.history()
	current, err := p.readCurrent()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		revs, err := h.List()
		if err != nil {
			return nil, err
		}
		for _, r := range revs {
			if current == nil || r.SHA256 != config.Checksum(current) {
				n = r.Number
				break
			}
		}
		if n == 0 {
			return nil, ErrNothingToRollBack
		}
	}
	data, rev, err := h.Read(n)
	if err != nil {
		if errors.Is(err, config.ErrNoRevision) {
			err = fmt.Errorf("%w (see: tunlr rollback --list)", errors.Join(core.ErrInvalidArgument, err))
		}
		return nil, err
	}
	// 独占的键按当前状态重新写入：修订中的透明代理开关、面板的监听地址和 secret 可能与现在不同，照搬会与
	// 工具的规则、防失联状态和面板的链接对不上。
	st, _, err := state.LoadOrNew(p.Layout.StateFile(), p.now())
	if err != nil {
		return nil, err
	}
	set, err := settings.Load(p.Layout.SettingsFile())
	if err != nil {
		return nil, err
	}
	data, facts, err := config.Enforce(data, managed(st.APISecret, switchesOf(st), set))
	if err != nil {
		return nil, fmt.Errorf("revision %d: %w", n, err)
	}
	plan := &Plan{Kind: KindRollback, Config: data, Subscription: rev.Subscription, RollbackTo: n, facts: facts}
	if err := p.finish(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// finish 读取生效的配置，计算差异，有变化时校验。
func (p *Pipeline) finish(ctx context.Context, plan *Plan) error {
	current, err := p.readCurrent()
	if err != nil {
		return err
	}
	plan.Current = current
	if !plan.Changed() {
		return nil
	}
	if current != nil {
		plan.Diff, plan.DiffSections = config.Diff("current", "new", current, plan.Config)
		// 无法解析的生效配置（facts 为 nil）交给 mihomo -t 报告。
		facts, _ := config.Inspect(current)
		plan.Restart = config.RestartKeys(facts, plan.facts)
		if c := config.DiffProxies(facts, plan.facts); c != nil && !c.Empty() {
			plan.Proxies = c
		}
	}
	v, err := p.validator()
	if err != nil {
		return err
	}
	plan.Check = v.Validate(ctx, plan.Config, &plan.facts.Geo)
	return nil
}

// ApplyResult 是一次发布的结果。
type ApplyResult struct {
	// Revision 是记录下的历史修订。
	Revision *config.Revision
	// Reloaded 报告运行中的内核是否已热重载新配置。
	Reloaded bool
	// Restarted 报告内核是否已重启（有需要重启才能生效的变化）并通过健康检查。
	// Reloaded 与 Restarted 都为 false 表示内核没有运行，配置会在内核启动时生效。
	Restarted bool
	// RestartNeeded 是热重载后仍需重启内核才能生效的键：内核不是由服务运行的，工具无法重启它。
	RestartNeeded []string
	// UIInstalled 是本次新安装的面板目录，没有安装时为空。
	UIInstalled string
	// UIError 是安装面板失败的原因。面板不是发布的必要条件，失败时照常发布，
	// 由内核按 external-ui-url 自己下载。
	UIError error
}

// Apply 发布 plan：存档发布前不是由 tunlr 发布的配置，原子写入新配置，让运行中的内核加载，
// 然后记录为新的历史修订（与最新修订相同时沿用它）。内核加载失败时恢复原来的配置并让内核重新加载它，返回满足
// errors.Is(err, core.ErrRolledBack) 的错误。没有变化时什么都不做，返回 nil。
func (p *Pipeline) Apply(ctx context.Context, plan *Plan) (*ApplyResult, error) {
	if plan.Check != nil {
		return nil, plan.Check
	}
	if !plan.Changed() {
		return nil, nil
	}
	// 面板下载可能较慢，在加锁之前进行，不阻塞定时任务；安装是原子的。
	uiDir, uiErr := p.installUI(ctx, plan.facts)

	l, err := p.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()

	result, err := p.ApplyLocked(ctx, plan)
	if result != nil {
		result.UIInstalled, result.UIError = uiDir, uiErr
	}
	return result, err
}

// ApplyLocked 同 Apply，但调用方须持有文件锁，也不安装面板。用于与其他改动一起在锁内完成的发布，
// 如透明代理开关。
func (p *Pipeline) ApplyLocked(ctx context.Context, plan *Plan) (*ApplyResult, error) {
	if plan.Check != nil {
		return nil, plan.Check
	}
	if !plan.Changed() {
		return nil, nil
	}
	current, err := p.readCurrent()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(current, plan.Current) || (current == nil) != (plan.Current == nil) {
		return nil, ErrStale
	}
	if plan.newSecret != "" {
		if err := p.saveSecret(plan.newSecret, plan.oldSecret); err != nil {
			return nil, err
		}
	}

	h := p.history()
	revs, err := h.List()
	if err != nil {
		return nil, err
	}
	if current != nil && (len(revs) == 0 || revs[0].SHA256 != config.Checksum(current)) {
		if _, err := h.Add(current, config.Revision{CreatedAt: p.now().UTC(), Reason: config.ReasonFound, ToolVersion: p.Version}); err != nil {
			return nil, fmt.Errorf("archive the current configuration: %w", err)
		}
	}

	if err := p.writeConfig(plan.Config); err != nil {
		return nil, err
	}
	result := &ApplyResult{}
	if err := p.activate(ctx, plan.Restart, result); err != nil {
		return nil, p.restore(ctx, current, plan.Restart, err)
	}

	// 生效的配置被删除后（如卸载后重新 init）发布的与最新修订相同，不重复记录。
	if current == nil && len(revs) > 0 && revs[0].SHA256 == config.Checksum(plan.Config) {
		result.Revision = &revs[0]
		return result, nil
	}
	rev := config.Revision{
		CreatedAt:    p.now().UTC(),
		Reason:       config.ReasonApply,
		Subscription: plan.Subscription,
		ToolVersion:  p.Version,
	}
	if plan.Kind == KindRollback {
		rev.Reason, rev.RollbackOf = config.ReasonRollback, plan.RollbackTo
	}
	if result.Revision, err = h.Add(plan.Config, rev); err != nil {
		return nil, fmt.Errorf("configuration published, but recording it in the history failed: %w", err)
	}
	return result, nil
}

// activate 让运行中的内核使用刚写入的配置：restart 不为空时重启由服务运行的内核，否则热重载。
// 没有由服务运行的内核时（如手动运行）退回热重载，并在 result 中提示需要重启。
func (p *Pipeline) activate(ctx context.Context, restart []string, result *ApplyResult) error {
	if len(restart) > 0 {
		err := p.restarter().RestartLocked(ctx, true)
		if err == nil {
			result.Restarted = true
			return nil
		}
		if !errors.Is(err, mihomo.ErrNotRunning) {
			return &restartError{err}
		}
	}
	err := p.reloader().Reload(ctx)
	if errors.Is(err, mihomo.ErrNotRunning) {
		return nil
	}
	if err == nil {
		result.Reloaded = true
		result.RestartNeeded = restart
	}
	return err
}

// restartError 表示重启内核后健康检查等失败：恢复原配置后要再次重启，而不是热重载，
// 因为此时内核可能已经停止或正在被 systemd 重启。
type restartError struct{ err error }

func (e *restartError) Error() string { return e.err.Error() }
func (e *restartError) Unwrap() error { return e.err }

// restore 在内核加载新配置失败后恢复原来的配置，并让内核重新加载（或重启后加载）它。
// 内核在加载新配置时退出的，恢复文件后由它下次启动时加载。
func (p *Pipeline) restore(ctx context.Context, previous []byte, restart []string, cause error) error {
	var err error
	if previous == nil {
		err = os.Remove(p.Layout.CoreConfig())
	} else if err = p.writeConfig(previous); err == nil {
		if _, ok := errors.AsType[*restartError](cause); ok {
			err = p.restarter().RestartLocked(ctx, false)
		} else {
			err = p.activate(ctx, restart, &ApplyResult{})
		}
	}
	if err != nil {
		return fmt.Errorf("mihomo failed to load the new configuration: %w; restoring the previous one also failed: %v", cause, err)
	}
	return fmt.Errorf("%w: mihomo failed to load the new configuration: %w", core.ErrRolledBack, cause)
}

func (p *Pipeline) restarter() Restarter {
	if p.Restarter != nil {
		return p.Restarter
	}
	return &mihomo.Manager{Layout: p.Layout, Service: &service.Systemd{Layout: p.Layout}, Download: p.Download, Owner: p.owner(), Now: p.Now}
}

func (p *Pipeline) reloader() Reloader {
	if p.Reloader != nil {
		return p.Reloader
	}
	return &mihomo.Reloader{Client: mihomo.NewClient(p.Layout.CoreSocket()), ConfigPath: p.Layout.CoreConfig()}
}

// Revisions 返回历史修订（最新的在前）和与生效配置一致的修订号，没有一致的修订时为 0。
func (p *Pipeline) Revisions() ([]config.Revision, int, error) {
	revs, err := p.history().List()
	if err != nil {
		return nil, 0, err
	}
	current, err := p.readCurrent()
	if err != nil || current == nil {
		return revs, 0, err
	}
	sum := config.Checksum(current)
	for _, r := range revs {
		if r.SHA256 == sum {
			return revs, r.Number, nil
		}
	}
	return revs, 0, nil
}

// saveSecret 保存生成的 API secret（第一次发布或换新时），old 是生成时状态文件中的值。期间并发的
// 另一次发布已经保存了别的 secret 时，本次计划中的配置已经过时。
func (p *Pipeline) saveSecret(secret, old string) error {
	st, _, err := state.LoadOrNew(p.Layout.StateFile(), p.now())
	if err != nil {
		return err
	}
	switch st.APISecret {
	case secret:
		return nil
	case old:
		st.APISecret = secret
		return state.Save(p.Layout.StateFile(), st)
	}
	return ErrStale
}

// writeConfig 原子写入生效的配置，归内核用户所有。工作目录中的 API socket 由内核以 0666 创建且
// 访问免 secret，只能靠目录权限阻止其他本地用户，因此每次发布都把目录权限收紧到 0750。
func (p *Pipeline) writeConfig(data []byte) error {
	if err := p.ensureCoreDir(); err != nil {
		return err
	}
	return fsutil.WriteFileOwned(p.Layout.CoreConfig(), data, 0o640, p.owner())
}

func (p *Pipeline) ensureCoreDir() error {
	return mihomo.EnsureHome(p.Layout.CoreDir(), p.owner())
}

// owner 返回内核用户，见 Pipeline.Owner。查找失败时保持当前身份。
func (p *Pipeline) owner() *fsutil.Owner {
	if p.Owner == nil {
		p.Owner, _ = service.KernelUser(p.Layout)
	}
	return p.Owner
}

// installUI 在面板目录不存在时下载并校验 zashboard，免得内核自己从未经校验的地址下载。
// 只处理 external-ui-url 仍是 zashboard 官方地址（或经镜像的地址）的配置，
// 用户在 mixin 中换了面板时交给内核。返回新安装的目录。
func (p *Pipeline) installUI(ctx context.Context, facts *config.Facts) (string, error) {
	set, err := settings.Load(p.Layout.SettingsFile())
	if err != nil {
		return "", err
	}
	dir, url := facts.ExternalUI(p.Layout.CoreDir())
	if dir == "" {
		return "", nil
	}
	if url != mihomo.ZashboardURL && url != download.MirrorURL(set.GitHubMirror, mihomo.ZashboardURL) {
		return "", nil
	}
	if err := p.ensureCoreDir(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, uiTimeout)
	defer cancel()
	installed, err := mihomo.EnsureUI(ctx, p.downloader(set), dir, p.owner())
	if !installed {
		return "", err
	}
	return dir, nil
}

// CheckCurrent 用 mihomo -t 校验生效的配置，只读：不下载缺失的 geo 数据（缺了由 -t 报告）。
// 没有生效的配置时 exists 为 false。
func (p *Pipeline) CheckCurrent(ctx context.Context) (exists bool, err error) {
	current, err := p.readCurrent()
	if err != nil || current == nil {
		return false, err
	}
	v, err := p.validator()
	if err != nil {
		return true, err
	}
	return true, v.Validate(ctx, current, &config.GeoNeeds{})
}

// Current 返回生效的配置，没有时为 nil。
func (p *Pipeline) Current() ([]byte, error) { return p.readCurrent() }

func (p *Pipeline) readCurrent() ([]byte, error) {
	data, err := os.ReadFile(p.Layout.CoreConfig())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

func (p *Pipeline) validator() (Validator, error) {
	if p.Validator != nil {
		return p.Validator, nil
	}
	set, err := settings.Load(p.Layout.SettingsFile())
	if err != nil {
		return nil, err
	}
	return &mihomo.Validator{
		Binary:   p.Layout.CoreBinary(),
		HomeDir:  p.Layout.CoreDir(),
		Download: p.downloader(set),
		RunAs:    p.owner(),
	}, nil
}

func (p *Pipeline) downloader(set *settings.Settings) *download.Client {
	if p.Download != nil {
		return p.Download
	}
	return &download.Client{Mirror: set.GitHubMirror}
}

func (p *Pipeline) history() *config.History {
	keep := p.Keep
	if keep == 0 {
		keep = DefaultKeep
	}
	return &config.History{Dir: p.Layout.HistoryDir(), Keep: keep}
}

func (p *Pipeline) lock(ctx context.Context) (*lock.Lock, error) {
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	return lock.Acquire(ctx, p.Layout.LockFile())
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// 受管 mixin 的名称。
const (
	ManagedCore   = "tunlr:core"
	ManagedTun    = "tunlr:tun"
	ManagedGeo    = "tunlr:geo"
	ManagedPanel  = "tunlr:panel"
	ManagedProxy  = "tunlr:proxy"
	ManagedMirror = "tunlr:mirror"
	ManagedFilter = "tunlr:filter"
)

// managed 生成 tunlr 的交互功能对应的受管 mixin，顺序即展示的顺序。独占键的：
//
//	tunlr:core   内核的 Unix socket，工具经它调用 API
//	tunlr:tun    透明代理：enable 取自开关，其余是固定值，工具补充的规则、清理和检测依赖它们；
//	             面板临时打开 TUN 时也按这些值工作
//	tunlr:geo    geo 数据的自动更新，只有一个旋钮：由内核自己更新时才写 geo-auto-update: true 和间隔，
//	             否则关闭它，由 tunlr 按周期更新并校验；间隔在关闭时也归它管
//	tunlr:panel  API 的监听地址（面板的局域网访问开关）和 secret
//	tunlr:proxy  代理端口的局域网访问开关：开启时 allow-lan: true，lan-allowed-ips 限制为私有地址段
//	             （config.LANAllowedIPs），内核拒绝其他来源；关闭时 allow-lan: false
//
// 提供默认值、用户可以覆盖的：tunlr:mirror，geo 数据的下载地址，配置了 GitHub 镜像时另有经镜像的面板
// 下载地址和让镜像直连的规则（用 delete-rules 去掉）；tunlr:filter，有过滤项时在规则最前插入的规则
// （设为直连的设备、拒绝 QUIC、只代理常用端口，见 filterRules 和 DESIGN.md 8.13）。
func managed(secret string, sw switches, set *settings.Settings) []config.Managed {
	geoAuto, geoHours := set.GeoAutoUpdate()
	geo := fmt.Sprintf("geo-auto-update: %t\n", geoAuto)
	if geoAuto {
		geo += fmt.Sprintf("geo-update-interval: %d\n", geoHours)
	}
	controller := fmt.Sprintf("127.0.0.1:%d", APIPort)
	if sw.panelLAN {
		controller = LANController
	}
	panel := marshal(struct {
		Controller string `yaml:"external-controller"`
		Secret     string `yaml:"secret"`
	}{controller, secret})

	// 来源列表开关时都写入（模板中也有，位置紧跟 allow-lan），开关只改 allow-lan。
	proxy := marshal(struct {
		AllowLAN      bool     `yaml:"allow-lan"`
		LANAllowedIPs []string `yaml:"lan-allowed-ips"`
	}{sw.proxyLAN, config.LANAllowedIPs})

	mirror := set.GitHubMirror
	m := struct {
		GeoxURL       map[string]string `yaml:"geox-url,omitempty"`
		ExternalUIURL string            `yaml:"external-ui-url,omitempty"`
		PrependRules  []string          `yaml:"prepend-rules,omitempty"`
	}{GeoxURL: mihomo.GeoxURL(mirror), PrependRules: mirrorRules(mirror)}
	if mirror != "" {
		m.ExternalUIURL = download.MirrorURL(mirror, mihomo.ZashboardURL)
	}
	mirrorData := marshal(m)

	list := []config.Managed{
		{Name: ManagedCore, Data: fmt.Appendf(nil, "external-controller-unix: %s\n", paths.CoreSocketName)},
		{Name: ManagedTun, Command: "tunlr tun on/off", Data: fmt.Appendf(nil, `tun:
  enable: %t
  auto-route: true
  auto-redirect: true
  strict-route: false
  device: %s
  iproute2-table-index: %d
  iproute2-rule-index: %d
`, sw.tun, netfilter.Device, netfilter.RouteTable, netfilter.RuleIndex)},
		{Name: ManagedGeo, Command: "tunlr update set geo", Data: []byte(geo), Owns: []string{"geo-auto-update", "geo-update-interval"}},
		{Name: ManagedPanel, Command: "tunlr panel lan on/off, tunlr panel --reset-secret", Data: panel},
		{Name: ManagedProxy, Command: "tunlr proxy lan on/off", Data: proxy, Owns: []string{"allow-lan", "lan-allowed-ips"}},
		{Name: ManagedMirror, Command: "tunlr config edit", Default: true, Data: mirrorData},
	}
	if rules := filterRules(sw); len(rules) > 0 {
		list = append(list, config.Managed{Name: ManagedFilter, Command: "tunlr filter", Default: true,
			Data: marshal(struct {
				PrependRules []string `yaml:"prepend-rules"`
			}{rules})})
	}
	return list
}

// marshal 以与生成的配置相同的缩进（2 格）编码受管 mixin 的内容，tunlr mixin list 和 plan 照原样显示。
// 内容都是字符串、映射和列表，编码不会失败。
func marshal(v any) []byte {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	_ = enc.Encode(v)
	_ = enc.Close()
	return b.Bytes()
}

// mirrorRules 返回让 GitHub 镜像直连的规则。内核自己的下载（面板、geo 数据）同样按规则分流，
// 实测选中不可用的节点时全部失败；镜像本来就是为直连准备的，因此让它绕开节点。没有配置镜像时为空。
func mirrorRules(mirror string) []string {
	if mirror == "" {
		return nil
	}
	u, err := url.Parse(mirror)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Is4() {
			return []string{"IP-CIDR," + ip.String() + "/32,DIRECT,no-resolve"}
		}
		return []string{"IP-CIDR6," + ip.String() + "/128,DIRECT,no-resolve"}
	}
	return []string{"DOMAIN," + host + ",DIRECT"}
}

// newSecret 生成 32 个十六进制字符的随机 secret。
func newSecret() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
