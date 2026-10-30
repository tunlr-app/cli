// Package pipeline 实现配置流水线：合成 → 校验 → 预览差异 → 原子发布 → 记录历史，以及回滚。
//
// 一次发布分两步：Plan 计算要发布的配置、与生效配置的差异和 mihomo -t 的结果，不改动任何
// 状态（只可能留下 mihomo -t 现场下载的 geo 数据）；用户确认后 Apply 在文件锁内发布这个 Plan。
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
	"net"
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
	"github.com/tunlr-app/cli/pkg/core/subscription"
	"github.com/tunlr-app/cli/templates"
)

// DefaultKeep 是保留的历史修订数。
const DefaultKeep = 10

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
	// Validator 为空时使用 Layout.CoreBinary() 的 mihomo -t，缺失的 geo 数据由它按配置中的 geox-url 下载。
	Validator Validator
	// Download 为空时按工具设置中的镜像下载（内核救援等经 kernels 用到）。
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
	// Rewrites 是受管 mixin 在合成的最后改写的值，如经镜像的 geox-url（仅 KindApply）。
	Rewrites []config.Rewrite
	// GeoSources 是 geo 数据的自动更新和来源的各键来自哪一层：兜底值（tunlr:defaults 或 mixin 中的
	// tunlr.defaults）、订阅或 mixin（仅 KindApply）。
	GeoSources []config.KeySource
	// ProxyFilter 是节点筛选的结果，没有设置时为 nil（仅 KindApply）。
	ProxyFilter *config.ProxyFilterResult
	// Inactive 表示这是按没有启用的订阅合成的计划（见 PlanWithMixin、PlanSubscription），只用来检查，
	// Apply 拒绝它。
	Inactive bool
	// Managed 是 tunlr 的交互功能生成的受管 mixin，按 Managed 函数的顺序排列（仅 KindApply）。
	// 其中含 API secret，展示时只列出键。
	Managed []config.Managed
	// RollbackTo 是回滚的目标修订（仅 KindRollback）。
	RollbackTo int
	// Restart 是有变化、需要重启内核才能生效的顶层键；不为空时发布后重启内核而不是热重载。
	Restart []string
	// GeoReplaced 是来源变了（geox-url 改了，如切换到自带 geo 来源的订阅）的 geo 文件名：mihomo -t 已按新来源
	// 下载到暂存目录并用于校验，发布时换进内核的工作目录并重启内核。
	GeoReplaced []string
	// Proxies 汇总节点的变化，尚未发布过、没有变化或无法解析时为 nil。
	Proxies *config.ProxyChanges
	// Check 是校验结果，nil 表示通过；没有变化时不校验，也为 nil。
	// 配置有误时为 *mihomo.CheckError，内核未安装时满足 errors.Is(err, mihomo.ErrNotInstalled)。
	Check error

	// newSecret 是本次生成、尚未保存的 API secret，oldSecret 是生成时状态文件中的 secret（第一次发布时为空）。
	newSecret, oldSecret string
	// facts 是从 Config 中提取的信息。
	facts *config.Facts
	// geoStaged 是暂存的 geo 文件的 sha256（文件名为键），见 GeoReplaced。
	geoStaged map[string]string
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
// 用于在保存编辑结果之前检查它。m 是没有启用的订阅的 mixin 时，按这个订阅被启用时的样子合成并校验，
// Plan.Inactive 为真：这样的计划只用来检查，不能发布。订阅不存在时报 core.ErrInvalidArgument。
func (p *Pipeline) PlanWithMixin(ctx context.Context, m config.Mixin) (*Plan, error) {
	opts := planOptions{edited: &m}
	if sub, _, ok := strings.Cut(m.Name, "/"); ok {
		l, err := subscription.LoadList(p.Layout.SubscriptionList())
		if err != nil {
			return nil, err
		}
		if l.Find(sub) == nil {
			return nil, fmt.Errorf("%w: no subscription named %s (see tunlr sub)", core.ErrInvalidArgument, sub)
		}
		if sub != l.Active {
			opts.subscription = sub
		}
	}
	return p.plan(ctx, opts)
}

// PlanSubscription 同 Plan，但按订阅 name 被启用时的样子合成，不改动订阅列表，用于 sub use --dry-run。
// name 不是启用的订阅时 Plan.Inactive 为真，这样的计划不能发布。订阅不存在时报 core.ErrInvalidArgument。
func (p *Pipeline) PlanSubscription(ctx context.Context, name string) (*Plan, error) {
	l, err := subscription.LoadList(p.Layout.SubscriptionList())
	if err != nil {
		return nil, err
	}
	if l.Find(name) == nil {
		return nil, fmt.Errorf("%w: no subscription named %s (see tunlr sub)", core.ErrInvalidArgument, name)
	}
	opts := planOptions{}
	if name != l.Active {
		opts.subscription = name
	}
	return p.plan(ctx, opts)
}

// PlanTun 同 Plan，但按透明代理开关为 on 时的样子合成，不看状态文件中的开关。
// 用于开关之前预览和校验；调用方在发布前保存开关。
func (p *Pipeline) PlanTun(ctx context.Context, on bool) (*Plan, error) {
	return p.plan(ctx, planOptions{tun: &on})
}

// APIPort 是内核 API（面板）的端口，与模板中的 external-controller 一致。
const APIPort = 9090

// LANController 是面板的局域网访问开启时 external-controller 的值：监听所有 IPv4 地址，
// 由 nft 表 inet tunlr_access 只放行从选定网卡进来、来自私有地址段的连接。
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

// Restored 是还原备份后的输入，代替设置文件、订阅列表中启用的订阅、mixin.d、状态文件中的 API secret、设备与过滤项。
// 开关仍按状态文件。
type Restored struct {
	// Settings 是还原后的工具设置。
	Settings *settings.Settings
	Mixins   []config.Mixin
	// Active 是启用的订阅，Content 是它的内容；没有启用的订阅时都为空。
	Active    string
	Content   []byte
	APISecret string
	Devices   []state.Device
	Filter    state.Filter
}

// PlanRestore 同 Plan，但按还原备份后的样子合成，不读被代替的文件和状态。调用方在发布前写入它们。
func (p *Pipeline) PlanRestore(ctx context.Context, r Restored) (*Plan, error) {
	return p.plan(ctx, planOptions{restored: &r})
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
	_, err := p.Compose(ctx, "")
	return err
}

// Compose 只合成配置：不检查端口安全、不与生效的配置比较，也不校验。sub 非空时按订阅 sub 启用时合成。
// 返回的计划只有 Config、Subscription、Mixins、Managed、Dropped 和 GeoSources，不能发布；用于查看配置各部分的来源。
func (p *Pipeline) Compose(ctx context.Context, sub string) (*Plan, error) {
	plan, err := p.plan(ctx, planOptions{composeOnly: true, subscription: sub})
	if plan != nil {
		plan.Inactive = true
	}
	return plan, err
}

// Managed 返回按状态文件和工具设置生成的受管 mixin，与发布时所用的相同；还没有生成过 API secret 时
// secret 为空。用于展示（tunlr config）。
func (p *Pipeline) Managed() ([]config.Managed, error) {
	set, err := settings.Load(p.Layout.SettingsFile())
	if err != nil {
		return nil, err
	}
	st, _, err := state.LoadOrNew(p.Layout.StateFile(), p.now())
	if err != nil {
		return nil, err
	}
	return managed(st.APISecret, p.switchesOf(st), set), nil
}

// switches 是决定受管 mixin 内容的开关。
type switches struct {
	tun, panelLAN, proxyLAN bool
	// direct 是走法为直连的设备的地址。
	direct []string
	filter state.Filter
	// coreUID 是内核用户的 UID，TUN 按它排除内核自己的流量；不知道（尚未 init、测试用的布局）时为 -1。
	coreUID int
}

func (p *Pipeline) switchesOf(st *state.State) switches {
	sw := switches{tun: st.TunEnabled(), panelLAN: st.PanelLAN != nil, proxyLAN: st.ProxyLAN != nil, direct: st.DirectIPs(), filter: st.Filter, coreUID: -1}
	if o := p.owner(); o != nil {
		sw.coreUID = o.UID
	}
	return sw
}

type planOptions struct {
	// edited 不为 nil 时代替磁盘上的同名 mixin 文件。
	edited *config.Mixin
	// subscription 非空时代替订阅列表中启用的订阅（检查没有启用的订阅的 mixin）。
	subscription string
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
	// restored 不为 nil 时代替设置文件、mixin、订阅与状态中的对应部分（见 Restored）。
	restored *Restored
	// composeOnly 只合成，不检查端口安全，不比较也不校验。
	composeOnly bool
	// noValidate 合成、检查端口安全并与生效的配置比较，但不校验（不运行 mihomo -t）。
	noValidate bool
}

func (p *Pipeline) plan(ctx context.Context, opts planOptions) (*Plan, error) {
	edited := opts.edited
	set := opts.settings
	if opts.restored != nil {
		set = opts.restored.Settings
	}
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
	var active string
	if r := opts.restored; r != nil {
		active, st.APISecret, st.Devices, st.Filter = r.Active, r.APISecret, r.Devices, r.Filter
	} else {
		l, err := subscription.LoadList(p.Layout.SubscriptionList())
		if err != nil {
			return nil, err
		}
		active = l.Active
	}
	if opts.subscription != "" {
		active = opts.subscription
	}
	plan := &Plan{Kind: KindApply, Subscription: active, Inactive: opts.subscription != ""}

	secret := st.APISecret
	if secret == "" || opts.resetSecret {
		if secret, err = newSecret(); err != nil {
			return nil, err
		}
		plan.newSecret, plan.oldSecret = secret, st.APISecret
	}
	sw := p.switchesOf(st)
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
	if opts.restored != nil {
		sub = opts.restored.Content
	} else if active != "" {
		sub, err = os.ReadFile(p.Layout.SubscriptionFile(active))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("the active subscription %q has no content yet; run: tunlr sub update %s", active, active)
		}
		if err != nil {
			return nil, err
		}
	}
	mixins, err := config.LoadMixinsFor(p.Layout.MixinDir(), active)
	if err != nil {
		return nil, err
	}
	if opts.restored != nil {
		mixins = mixinsFor(opts.restored.Mixins, active)
	}
	if edited != nil {
		mixins = withEdited(mixins, *edited)
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
	if err != nil {
		return nil, err
	}
	plan.Config, plan.Dropped, plan.facts, plan.GeoSources = res.Config, res.Dropped, res.Facts, res.GeoSources
	if opts.composeOnly {
		return plan, nil
	}
	res.Warnings = append(res.Warnings, config.DNSWarnings(res.Facts, hasInterface)...)
	// 只在重新合成时检查端口安全；回滚和重写独占的键不经过这里，以免旧配置无法恢复。
	if err := config.CheckExposure(res.Config); err != nil {
		return nil, err
	}
	plan.Warnings, plan.Overrides, plan.Rewrites, plan.ProxyFilter = res.Warnings, res.Overrides, res.Rewrites, res.ProxyFilter
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

// hasInterface 报告本机是否有名为 name 的网卡，测试时替换。
var hasInterface = func(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

// mixinsFor 从全部 mixin（共用的和所有订阅的，见 config.LoadAllMixins）中取出订阅 sub 启用时应用的那些。
func mixinsFor(all []config.Mixin, sub string) []config.Mixin {
	var res []config.Mixin
	for _, m := range all {
		if s, _, ok := strings.Cut(m.Name, "/"); !ok || (sub != "" && s == sub) {
			res = append(res, m)
		}
	}
	return res
}

// withEdited 用 edited 代替 mixins 中的同名文件，没有时插入到它在应用顺序中的位置：
// 共用的在前、订阅的在后，各自按名称排序。
func withEdited(mixins []config.Mixin, edited config.Mixin) []config.Mixin {
	key := func(name string) (bool, string) { return strings.Contains(name, "/"), name }
	i, found := slices.BinarySearchFunc(mixins, edited.Name, func(m config.Mixin, name string) int {
		as, an := key(m.Name)
		bs, bn := key(name)
		if as != bs {
			if as {
				return 1
			}
			return -1
		}
		return strings.Compare(an, bn)
	})
	if found {
		mixins[i] = edited
		return mixins
	}
	return slices.Insert(mixins, i, edited)
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
	l, err := subscription.LoadList(p.Layout.SubscriptionList())
	if err != nil {
		return nil, err
	}
	plan := &Plan{Kind: KindApply, Subscription: l.Active}
	if plan.Config, plan.facts, err = config.Enforce(current, managed(st.APISecret, p.switchesOf(st), set)); err != nil {
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
			err = fmt.Errorf("%w (the history: tunlr config)", errors.Join(core.ErrInvalidArgument, err))
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
	data, facts, err := config.Enforce(data, managed(st.APISecret, p.switchesOf(st), set))
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
	var fresh []mihomo.GeoFile
	if current != nil {
		plan.Diff, plan.DiffSections = config.Diff("current", "new", current, plan.Config)
		// 无法解析的生效配置（facts 为 nil）交给 mihomo -t 报告。
		facts, _ := config.Inspect(current)
		plan.Restart = config.RestartKeys(facts, plan.facts)
		if c := config.DiffProxies(facts, plan.facts); c != nil && !c.Empty() {
			plan.Proxies = c
		}
		if facts != nil {
			var err error
			if fresh, err = mihomo.GeoSourceChanges(p.Layout.CoreDir(), mihomo.GeoFilesFor(plan.facts.Geo), facts.Geo.URLs, plan.facts.Geo.URLs); err != nil {
				return err
			}
		}
	}
	// 来源变了的 geo 文件（例如切换到用自定义 geosite 分类的订阅）不用旧文件校验，由 mihomo -t 按新来源下载到
	// 暂存目录，发布时换进工作目录。不然内核会继续用旧来源的数据，直到它自己下一次更新，规则引用的分类在旧
	// 文件中可能没有。
	staged := p.Layout.GeoStageDir()
	if err := os.RemoveAll(staged); err != nil {
		return err
	}
	v, err := p.validator(fresh, staged, true)
	if err != nil {
		return err
	}
	plan.Check = v.Validate(ctx, plan.Config, &plan.facts.Geo)
	if len(fresh) == 0 {
		return nil
	}
	if plan.geoStaged, err = mihomo.StagedGeo(staged, fresh); err != nil {
		return err
	}
	for _, f := range fresh {
		if _, ok := plan.geoStaged[f.Name]; ok {
			plan.GeoReplaced = append(plan.GeoReplaced, f.Name)
		}
	}
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
}

// Apply 发布 plan：存档发布前不是由 tunlr 发布的配置，原子写入新配置，让运行中的内核加载，
// 然后记录为新的历史修订（与最新修订相同时沿用它）。内核加载失败时恢复原来的配置并让内核重新加载它，返回满足
// errors.Is(err, core.ErrRolledBack) 的错误。没有变化时什么都不做，返回 nil。
func (p *Pipeline) Apply(ctx context.Context, plan *Plan) (*ApplyResult, error) {
	if plan.Inactive {
		return p.ApplyLocked(ctx, plan)
	}
	if plan.Check != nil {
		return nil, plan.Check
	}
	if !plan.Changed() {
		return nil, nil
	}
	l, err := p.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	return p.ApplyLocked(ctx, plan)
}

// ApplyLocked 同 Apply，但调用方须持有文件锁。用于与其他改动一起在锁内完成的发布，
// 如透明代理开关。
func (p *Pipeline) ApplyLocked(ctx context.Context, plan *Plan) (*ApplyResult, error) {
	if plan.Inactive {
		return nil, fmt.Errorf("the plan was composed for subscription %s, which is not active; it can only be checked", plan.Subscription)
	}
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

	// 来源变了的 geo 文件换成计划时下载、校验用过的；内核的 geo 数据只能重启后重新加载。
	restart, undoGeo := plan.Restart, func() error { return nil }
	if len(plan.geoStaged) > 0 {
		if undoGeo, err = mihomo.InstallStagedGeo(p.Layout.GeoStageDir(), p.Layout.CoreDir(), plan.geoStaged); err != nil {
			return nil, err
		}
		restart = append(slices.Clone(restart), plan.GeoReplaced...)
	}
	if err := p.writeConfig(plan.Config); err != nil {
		return nil, errors.Join(err, undoGeo())
	}
	result := &ApplyResult{}
	if err := p.activate(ctx, restart, result); err != nil {
		if uerr := undoGeo(); uerr != nil {
			err = fmt.Errorf("%w; putting back the previous geo data failed: %v", err, uerr)
		}
		return nil, p.restore(ctx, current, restart, err)
	}
	if len(plan.geoStaged) > 0 {
		// 暂存目录中只剩换下来的旧文件。
		os.RemoveAll(p.Layout.GeoStageDir())
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

// CheckCurrent 用 mihomo -t 校验生效的配置，只读：mihomo -t 现场下载的 geo 数据不保留。
// 没有生效的配置时 exists 为 false。
func (p *Pipeline) CheckCurrent(ctx context.Context) (exists bool, err error) {
	current, err := p.readCurrent()
	if err != nil || current == nil {
		return false, err
	}
	v, err := p.validator(nil, "", false)
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

// validator 返回校验用的 Validator：fresh 是来源变了的 geo 文件，mihomo -t 下载的存到 staged；keep 为 true 时
// 工作目录中缺失、mihomo -t 下载的 geo 文件存进工作目录（见 mihomo.Validator）。
func (p *Pipeline) validator(fresh []mihomo.GeoFile, staged string, keep bool) (Validator, error) {
	if p.Validator != nil {
		return p.Validator, nil
	}
	return &mihomo.Validator{
		Binary:  p.Layout.CoreBinary(),
		HomeDir: p.Layout.CoreDir(),
		Fresh:   fresh,
		Staged:  staged,
		Keep:    keep,
		RunAs:   p.owner(),
	}, nil
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
	ManagedCore     = "tunlr:core"
	ManagedTun      = "tunlr:tun"
	ManagedPanel    = "tunlr:panel"
	ManagedProxy    = "tunlr:proxy"
	ManagedDefaults = "tunlr:defaults"
	ManagedDNS      = "tunlr:dns"
	ManagedMirror   = "tunlr:mirror"
	ManagedFilter   = "tunlr:filter"
)

// DNSListen 是内核 DNS 的监听地址，由 tunlr:dns 提供（tunlr:defaults 中的 dns 不写监听地址）。
const DNSListen = "127.0.0.1:1053"

// managed 生成 tunlr 的交互功能对应的受管 mixin，顺序即展示的顺序。独占键的：
//
//	tunlr:core   内核的 Unix socket，工具经它调用 API
//	tunlr:tun    透明代理：enable 取自开关，其余是固定值，工具补充的规则、清理和检测依赖它们；
//	             面板临时打开 TUN 时也按这些值工作；开启时还要求 dns.enable: true，dns-hijack 靠它；
//	             防回环的 auto-detect-interface 与 exclude-uid（内核用户，不知道时不写，仍然独占）
//	tunlr:panel  API 的监听地址（面板的局域网访问开关）和 secret
//	tunlr:proxy  代理端口的局域网访问开关：开启时 allow-lan: true，lan-allowed-ips 限制为私有地址段
//	             （config.LANAllowedIPs），内核拒绝其他来源；关闭时 allow-lan: false
//
// 提供默认值、用户可以覆盖的：tunlr:defaults，兜底值（templates.Defaults：geo 数据的设置和 DNS），写在
// tunlr.defaults 中，订阅没有写时才用（见 config.Compose）；tunlr:dns，DNS 的监听地址（订阅中的被丢弃）；
// tunlr:mirror，配置了 GitHub 镜像时经镜像的面板下载地址，
// 并在合成的最后把 geox-url 中 GitHub 的地址改为经镜像（RewriteGeoxURL，用户的 mixin 不能取消）；tunlr:filter，有过滤项
// 时在规则最前插入的规则（设为直连的设备、拒绝 QUIC、只代理常用端口，见 filterRules 和 DESIGN.md 8.13）。
func managed(secret string, sw switches, set *settings.Settings) []config.Managed {
	tunDNS := ""
	if sw.tun {
		tunDNS = "  dns:\n    enable: true\n"
	}
	// 内核用户的流量不进 TUN：auto-detect-interface 之外的第二道防回环，见 DESIGN.md 8.4。
	excludeUID := ""
	if sw.coreUID >= 0 {
		excludeUID = fmt.Sprintf("    exclude-uid:\n      - %d\n", sw.coreUID)
	}
	controller := fmt.Sprintf("127.0.0.1:%d", APIPort)
	if sw.panelLAN {
		controller = LANController
	}
	panel := marshal(section{Mihomo: struct {
		Controller string `yaml:"external-controller"`
		Secret     string `yaml:"secret"`
	}{controller, secret}})

	// 来源列表开关时都写入（模板中也有，位置紧跟 allow-lan），开关只改 allow-lan。
	proxy := marshal(section{Mihomo: struct {
		AllowLAN      bool     `yaml:"allow-lan"`
		LANAllowedIPs []string `yaml:"lan-allowed-ips"`
	}{sw.proxyLAN, config.LANAllowedIPs}})

	// 镜像的域名不加直连规则（2026-10-31 决定，见 DESIGN.md 第 7 节）：它和其他流量一样按规则分流。
	mirror := set.GitHubMirror
	var mirrorSection section
	if mirror != "" {
		mirrorSection.Mihomo = struct {
			ExternalUIURL string `yaml:"external-ui-url"`
		}{download.MirrorURL(mirror, mihomo.ZashboardURL)}
	}
	mirrorManaged := config.Managed{Name: ManagedMirror, Command: "tunlr settings edit", Default: true, Data: marshal(mirrorSection)}
	if mirror != "" {
		mirrorManaged.RewriteGeoxURL = func(url string) string { return download.MirrorURL(mirror, url) }
	}

	list := []config.Managed{
		{Name: ManagedCore, Data: fmt.Appendf(nil, "mihomo:\n  external-controller-unix: %s\n", paths.CoreSocketName)},
		{Name: ManagedTun, Command: "tunlr tun on/off", Data: fmt.Appendf(nil, `mihomo:
  tun:
    enable: %t
    auto-route: true
    auto-redirect: true
    strict-route: false
    auto-detect-interface: true
    device: %s
    iproute2-table-index: %d
    iproute2-rule-index: %d
%s%s`, sw.tun, netfilter.Device, netfilter.RouteTable, netfilter.RuleIndex, excludeUID, tunDNS), Owns: []string{"tun.exclude-uid"}},
		{Name: ManagedPanel, Command: "tunlr panel lan on/off, tunlr panel reset-secret", Data: panel},
		{Name: ManagedProxy, Command: "tunlr proxy lan on/off", Data: proxy, Owns: []string{"allow-lan", "lan-allowed-ips"}},
		DefaultsManaged(),
		{Name: ManagedDNS, Default: true, Data: fmt.Appendf(nil, "mihomo:\n  dns:\n    listen: %s\n", DNSListen)},
		mirrorManaged,
	}
	if rules := filterRules(sw); len(rules) > 0 {
		list = append(list, config.Managed{Name: ManagedFilter, Command: "tunlr filter", Default: true,
			Data: marshal(section{Tunlr: prependRules(rules)})})
	}
	return list
}

// DefaultsManaged 返回 tunlr:defaults：订阅没有写时使用的 geo 数据设置和 DNS（templates.Defaults），
// 不随开关和设置变化。
func DefaultsManaged() config.Managed {
	return config.Managed{Name: ManagedDefaults, Default: true, Data: templates.Defaults}
}

// section 是受管 mixin 的两个命名空间，写法与用户的 mixin 相同。
type section struct {
	Mihomo any         `yaml:"mihomo,omitempty"`
	Tunlr  *tunlrRules `yaml:"tunlr,omitempty"`
}

// tunlrRules 是受管 mixin 中 tunlr: 的部分：只有在规则最前插入的规则。
type tunlrRules struct {
	Rules struct {
		Prepend []string `yaml:"prepend"`
	} `yaml:"rules"`
}

// prependRules 返回在规则最前插入 rules 的 tunlr: 部分，rules 为空时为 nil。
func prependRules(rules []string) *tunlrRules {
	if len(rules) == 0 {
		return nil
	}
	t := &tunlrRules{}
	t.Rules.Prepend = rules
	return t
}

// marshal 以与生成的配置相同的缩进（2 格）编码受管 mixin 的内容，tunlr config 和 plan 照原样显示。
// 内容都是字符串、映射和列表，编码不会失败。
func marshal(v any) []byte {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	_ = enc.Encode(v)
	_ = enc.Close()
	return b.Bytes()
}

// newSecret 生成 32 个十六进制字符的随机 secret。
func newSecret() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
