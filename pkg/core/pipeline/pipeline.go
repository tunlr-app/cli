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
	"github.com/tunlr-app/cli/pkg/core/paths"
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

// Validator 校验配置，通常是 *mihomo.Validator。
type Validator interface {
	Validate(ctx context.Context, cfg []byte) error
}

// Reloader 让运行中的内核加载已写入磁盘的配置。
type Reloader interface {
	Reload(ctx context.Context) error
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
	// GeneratedRules 是工具插在订阅规则前面的规则（仅 KindApply），目前是让 GitHub 镜像直连的规则。
	GeneratedRules []string
	// RollbackTo 是回滚的目标修订（仅 KindRollback）。
	RollbackTo int
	// Check 是校验结果，nil 表示通过；没有变化时不校验，也为 nil。
	// 配置有误时为 *mihomo.CheckError，内核未安装时满足 errors.Is(err, mihomo.ErrNotInstalled)。
	Check error

	// newSecret 是本次生成、尚未保存的 API secret。
	newSecret string
}

// Changed 报告 Config 与生效的配置是否不同。
func (p *Plan) Changed() bool {
	return p.Current == nil || !bytes.Equal(p.Current, p.Config)
}

// Plan 用启用的订阅、mixin 和工具设置合成新配置，与生效的配置比较并校验。
// mixin 有错时返回 *config.MixinError。
func (p *Pipeline) Plan(ctx context.Context) (*Plan, error) {
	return p.plan(ctx, nil)
}

// PlanWithMixin 同 Plan，但用 m 代替磁盘上的同名 mixin 文件（不存在时按文件名顺序加入），
// 用于在保存编辑结果之前检查它。
func (p *Pipeline) PlanWithMixin(ctx context.Context, m config.Mixin) (*Plan, error) {
	return p.plan(ctx, &m)
}

func (p *Pipeline) plan(ctx context.Context, edited *config.Mixin) (*Plan, error) {
	set, err := settings.Load(p.Layout.SettingsFile())
	if err != nil {
		return nil, err
	}
	st, _, err := state.LoadOrNew(p.Layout.StateFile(), p.now())
	if err != nil {
		return nil, err
	}
	plan := &Plan{Kind: KindApply, Subscription: st.Active}

	secret := st.APISecret
	if secret == "" {
		if secret, err = newSecret(); err != nil {
			return nil, err
		}
		plan.newSecret = secret
	}
	defaults, err := defaultsLayer(secret, set.GitHubMirror)
	if err != nil {
		return nil, err
	}
	plan.GeneratedRules = mirrorRules(set.GitHubMirror)
	generated, err := generatedLayer(plan.GeneratedRules)
	if err != nil {
		return nil, err
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

	res, err := config.Compose(config.Layers{
		Template:     templates.Config,
		Defaults:     defaults,
		Subscription: sub,
		Generated:    generated,
		Mixins:       mixins,
		Enforced:     enforcedLayer(),
	})
	if err != nil {
		return nil, err
	}
	plan.Config, plan.Dropped = res.Config, res.Dropped
	plan.Warnings, plan.Overrides = res.Warnings, res.Overrides
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
	plan := &Plan{Kind: KindRollback, Config: data, Subscription: rev.Subscription, RollbackTo: n}
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
		plan.Diff = config.Diff("current", "new", current, plan.Config)
	}
	v, err := p.validator()
	if err != nil {
		return err
	}
	plan.Check = v.Validate(ctx, plan.Config)
	return nil
}

// ApplyResult 是一次发布的结果。
type ApplyResult struct {
	// Revision 是记录下的历史修订。
	Revision *config.Revision
	// Reloaded 报告运行中的内核是否已加载新配置；为 false 表示内核没有运行。
	Reloaded bool
	// UIInstalled 是本次新安装的面板目录，没有安装时为空。
	UIInstalled string
	// UIError 是安装面板失败的原因。面板不是发布的必要条件，失败时照常发布，
	// 由内核按 external-ui-url 自己下载。
	UIError error
}

// Apply 发布 plan：存档发布前不是由 tunlr 发布的配置，原子写入新配置，让运行中的内核加载，
// 然后记录为新的历史修订。内核加载失败时恢复原来的配置并让内核重新加载它，返回满足
// errors.Is(err, core.ErrRolledBack) 的错误。没有变化时什么都不做，返回 nil。
func (p *Pipeline) Apply(ctx context.Context, plan *Plan) (*ApplyResult, error) {
	if plan.Check != nil {
		return nil, plan.Check
	}
	if !plan.Changed() {
		return nil, nil
	}
	// 面板下载可能较慢，在加锁之前进行，不阻塞定时任务；安装是原子的。
	uiDir, uiErr := p.installUI(ctx, plan.Config)

	l, err := p.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()

	current, err := p.readCurrent()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(current, plan.Current) || (current == nil) != (plan.Current == nil) {
		return nil, ErrStale
	}
	if plan.newSecret != "" {
		if err := p.saveSecret(plan.newSecret); err != nil {
			return nil, err
		}
	}

	h := p.history()
	if current != nil {
		revs, err := h.List()
		if err != nil {
			return nil, err
		}
		if len(revs) == 0 || revs[0].SHA256 != config.Checksum(current) {
			if _, err := h.Add(current, config.Revision{CreatedAt: p.now().UTC(), Reason: config.ReasonFound, ToolVersion: p.Version}); err != nil {
				return nil, fmt.Errorf("archive the current configuration: %w", err)
			}
		}
	}

	if err := p.writeConfig(plan.Config); err != nil {
		return nil, err
	}
	result := &ApplyResult{Reloaded: true, UIInstalled: uiDir, UIError: uiErr}
	if err := p.reloader().Reload(ctx); errors.Is(err, mihomo.ErrNotRunning) {
		result.Reloaded = false
	} else if err != nil {
		return nil, p.restore(ctx, current, err)
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

// restore 在内核加载新配置失败后恢复原来的配置，并让内核重新加载它。
// 内核在加载新配置时退出的，恢复文件后由它下次启动时加载。
func (p *Pipeline) restore(ctx context.Context, previous []byte, cause error) error {
	var err error
	if previous == nil {
		err = os.Remove(p.Layout.CoreConfig())
	} else if err = p.writeConfig(previous); err == nil {
		if err = p.reloader().Reload(ctx); errors.Is(err, mihomo.ErrNotRunning) {
			err = nil
		}
	}
	if err != nil {
		return fmt.Errorf("mihomo failed to load the new configuration: %w; restoring the previous one also failed: %v", cause, err)
	}
	return fmt.Errorf("%w: mihomo failed to load the new configuration: %w", core.ErrRolledBack, cause)
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

// saveSecret 保存第一次发布时生成的 API secret。并发的另一次发布已经保存了不同的 secret 时，
// 本次计划中的配置已经过时。
func (p *Pipeline) saveSecret(secret string) error {
	st, _, err := state.LoadOrNew(p.Layout.StateFile(), p.now())
	if err != nil {
		return err
	}
	switch st.APISecret {
	case secret:
		return nil
	case "":
		st.APISecret = secret
		return state.Save(p.Layout.StateFile(), st)
	}
	return ErrStale
}

// writeConfig 原子写入生效的配置。工作目录中的 API socket 由内核以 0666 创建且访问免 secret，
// 只能靠目录权限阻止其他本地用户，因此每次发布都把目录权限收紧到 0750。
func (p *Pipeline) writeConfig(data []byte) error {
	if err := p.ensureCoreDir(); err != nil {
		return err
	}
	return fsutil.WriteFile(p.Layout.CoreConfig(), data, 0o640)
}

func (p *Pipeline) ensureCoreDir() error {
	dir := p.Layout.CoreDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	return os.Chmod(dir, 0o750)
}

// installUI 在面板目录不存在时下载并校验 zashboard，免得内核自己从未经校验的地址下载。
// 只处理 external-ui-url 仍是 zashboard 官方地址（或经镜像的地址）的配置，
// 用户在 mixin 中换了面板时交给内核。返回新安装的目录。
func (p *Pipeline) installUI(ctx context.Context, cfg []byte) (string, error) {
	set, err := settings.Load(p.Layout.SettingsFile())
	if err != nil {
		return "", err
	}
	dir, url, err := config.ExternalUI(cfg, p.Layout.CoreDir())
	if err != nil || dir == "" {
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
	installed, err := mihomo.EnsureUI(ctx, p.downloader(set), dir)
	if !installed {
		return "", err
	}
	return dir, nil
}

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

// defaultsLayer 生成默认值层：API secret，以及配置了 GitHub 镜像时经镜像的面板和 geo 数据地址。
func defaultsLayer(secret, mirror string) ([]byte, error) {
	d := struct {
		Secret        string            `yaml:"secret"`
		ExternalUIURL string            `yaml:"external-ui-url,omitempty"`
		GeoxURL       map[string]string `yaml:"geox-url,omitempty"`
	}{Secret: secret, GeoxURL: mihomo.GeoxURL(mirror)}
	if mirror != "" {
		d.ExternalUIURL = download.MirrorURL(mirror, mihomo.ZashboardURL)
	}
	return yaml.Marshal(d)
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

// generatedLayer 把工具生成的规则写成 mixin，插在订阅规则的前面。
func generatedLayer(rules []string) ([]byte, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	return yaml.Marshal(map[string][]string{"prepend-rules": rules})
}

// enforcedLayer 生成强制项：工具运行所依赖、mixin 不能覆盖的键。
func enforcedLayer() []byte {
	return []byte("external-controller-unix: " + paths.CoreSocketName + "\n")
}

// newSecret 生成 32 个十六进制字符的随机 secret。
func newSecret() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
