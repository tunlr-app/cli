package mihomo

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/lock"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
)

const (
	// DefaultKeepKernels 是保留的已知可用版本数。
	DefaultKeepKernels = 3
	// defaultStartTimeout 是启动后等待内核响应 API 的时限。
	defaultStartTimeout = 15 * time.Second
	// defaultSettle 是内核响应后继续观察的时间，确认它没有随即退出或被 systemd 重启。
	defaultSettle = 3 * time.Second
	// lockWait 是等待其他 tunlr 操作释放锁的最长时间。
	lockWait = 30 * time.Second
	// rescueLockWait 是救援等待锁的时间：拿不到锁说明升级等操作正在进行并会自己回滚，救援不与它争抢。
	rescueLockWait = time.Second
)

var (
	// ErrUnhealthy 表示内核启动后没有通过健康检查。
	ErrUnhealthy = errors.New("mihomo failed the health check")
	// ErrNoKnownGood 表示没有可以换回的已知可用版本。
	ErrNoKnownGood = errors.New("no known-good mihomo version to switch to")
	// ErrServiceNotInstalled 表示内核的 systemd 服务尚未安装（需要 tunlr init）。
	ErrServiceNotInstalled = errors.New("the mihomo service is not installed; run: tunlr init")
)

// Service 控制内核的 systemd 服务，通常是 *service.Systemd。
type Service interface {
	Installed() bool
	Status(ctx context.Context, unit string) (service.Status, error)
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	Restart(ctx context.Context, unit string) error
	ResetFailed(ctx context.Context, unit string) error
}

// Manager 管理运行中的内核：安装与升级、已知可用版本、服务的启停与健康检查、救援，
// 以及面板升级后的善后。会改动系统的方法自己获取文件锁，名称带 Locked 的除外。
type Manager struct {
	Layout  paths.Layout
	Service Service
	// Download 为空时按工具设置中的镜像下载。
	Download *download.Client
	// Arch 为零值时检测本机。
	Arch Arch
	// Owner 是内核用户：bin/ 中的二进制归它所有，mihomo -t 也以它运行。为 nil 时保持当前身份。
	Owner *fsutil.Owner
	// Keep 为零时使用 DefaultKeepKernels。
	Keep int
	// StartTimeout、Settle 为零时使用默认值，见健康检查。
	StartTimeout time.Duration
	Settle       time.Duration
	// Now 为空时使用 time.Now。
	Now func() time.Time
	// Log 为空时不记录日志。
	Log *slog.Logger
}

// Health 是一次通过的健康检查。
type Health struct {
	// Version 是运行中的内核版本。
	Version string `json:"version"`
	// MainPID 是内核进程。
	MainPID int `json:"main_pid"`
}

// Start 启动内核服务并做健康检查（服务因多次失败进入 failed 时先清除该状态）。通过后，
// 运行的二进制若是工具下载校验过的版本，记为已知可用。
func (m *Manager) Start(ctx context.Context) (*Health, error) {
	if !m.Service.Installed() {
		return nil, ErrServiceNotInstalled
	}
	if err := m.Service.ResetFailed(ctx, service.CoreUnit); err != nil {
		m.log().Debug("reset-failed", "err", err)
	}
	return m.cycle(ctx, m.Service.Start)
}

// Restart 重启内核服务（没有运行时启动它）并做健康检查，同 Start。
func (m *Manager) Restart(ctx context.Context) (*Health, error) {
	if !m.Service.Installed() {
		return nil, ErrServiceNotInstalled
	}
	return m.cycle(ctx, m.Service.Restart)
}

// Stop 停止内核服务。
func (m *Manager) Stop(ctx context.Context) error {
	if !m.Service.Installed() {
		return ErrServiceNotInstalled
	}
	return m.Service.Stop(ctx, service.CoreUnit)
}

// RestartLocked 重启内核服务并做健康检查（配置发布时使用，调用方须持有文件锁）。服务没有安装，
// 或 ifRunning 时服务没有运行，返回 ErrNotRunning。
func (m *Manager) RestartLocked(ctx context.Context, ifRunning bool) error {
	if !m.Service.Installed() {
		return ErrNotRunning
	}
	if ifRunning {
		if running, err := m.running(ctx); err != nil || !running {
			return cmp.Or(err, ErrNotRunning)
		}
	} else if err := m.Service.ResetFailed(ctx, service.CoreUnit); err != nil {
		m.log().Debug("reset-failed", "err", err)
	}
	h, err := m.restartAndCheck(ctx, m.Service.Restart, "")
	if err != nil {
		return err
	}
	return m.confirm(h.Version)
}

func (m *Manager) cycle(ctx context.Context, action func(context.Context, string) error) (*Health, error) {
	l, err := m.lock(ctx, lockWait)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	h, err := m.restartAndCheck(ctx, action, "")
	if err != nil {
		return nil, err
	}
	if err := m.confirm(h.Version); err != nil {
		m.log().Warn("record the known-good kernel", "err", err)
	}
	return h, nil
}

// restartAndCheck 执行 action（启动或重启）后做健康检查：StartTimeout 内经 socket 得到 /version
// 的响应且版本与 want 一致（want 为空时不比较），再观察 Settle，服务仍为 active、MainPID 不变、
// systemd 的重启计数没有增加。
func (m *Manager) restartAndCheck(ctx context.Context, action func(context.Context, string) error, want string) (*Health, error) {
	if err := action(ctx, service.CoreUnit); err != nil {
		return nil, err
	}
	before, err := m.Service.Status(ctx, service.CoreUnit)
	if err != nil {
		return nil, err
	}
	client := NewClient(m.Layout.CoreSocket())
	deadline := time.Now().Add(orDefault(m.StartTimeout, defaultStartTimeout))
	var version string
	for {
		v, err := client.Version(ctx)
		if err == nil {
			version = v
			break
		}
		if st, serr := m.Service.Status(ctx, service.CoreUnit); serr == nil && st.ActiveState == "failed" {
			return nil, fmt.Errorf("%w: the service failed to start (see: tunlr logs)", ErrUnhealthy)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: no API response within %s: %v (see: tunlr logs)", ErrUnhealthy, orDefault(m.StartTimeout, defaultStartTimeout), err)
		}
		if err := sleep(ctx, 300*time.Millisecond); err != nil {
			return nil, err
		}
	}
	if want != "" && version != want {
		return nil, fmt.Errorf("%w: running version is %s, expected %s", ErrUnhealthy, version, want)
	}
	up, err := m.Service.Status(ctx, service.CoreUnit)
	if err != nil {
		return nil, err
	}
	if err := sleep(ctx, orDefault(m.Settle, defaultSettle)); err != nil {
		return nil, err
	}
	after, err := m.Service.Status(ctx, service.CoreUnit)
	if err != nil {
		return nil, err
	}
	if !after.Active() || after.MainPID != up.MainPID || after.NRestarts != before.NRestarts {
		return nil, fmt.Errorf("%w: mihomo exited shortly after starting (see: tunlr logs)", ErrUnhealthy)
	}
	return &Health{Version: version, MainPID: after.MainPID}, nil
}

// UpgradeResult 是安装、升级或回退内核的结果。
type UpgradeResult struct {
	// From 是原来的二进制对应的已知可用版本，没有安装过或不是已知可用版本时为空。
	From string `json:"from,omitempty"`
	// To 是现在的版本。
	To string `json:"to"`
	// Asset 是 To 在官方发布中的资产名。
	Asset string `json:"asset"`
	// Changed 报告二进制是否被替换；已经是该版本时为 false。
	Changed bool `json:"changed"`
	// Restarted 报告内核是否已重启并通过健康检查；服务没有运行时只替换，为 false。
	Restarted bool `json:"restarted"`
}

// Upgrade 安装或升级到 version（为空时为最新的正式发布）：下载并按官方 sha256 校验，
// 降权用新内核对生效的配置执行 -t（不通过时返回满足 errors.Is(err, core.ErrCheckFailed) 的错误，
// 运行中的内核不受影响），然后在锁内原子替换 bin/mihomo；服务在运行时重启并做健康检查，
// 失败则换回原来的二进制并再次重启，返回满足 errors.Is(err, core.ErrRolledBack) 的错误。
func (m *Manager) Upgrade(ctx context.Context, version string) (*UpgradeResult, error) {
	arch, err := m.arch()
	if err != nil {
		return nil, err
	}
	d, err := m.downloader()
	if err != nil {
		return nil, err
	}
	k, err := FetchKernel(ctx, d, arch, version)
	if err != nil {
		return nil, err
	}
	return m.switchTo(ctx, k, true)
}

// Rollback 换到最近一个与当前二进制不同的已知可用版本，同样经过 -t、重启和健康检查。
func (m *Manager) Rollback(ctx context.Context) (*UpgradeResult, error) {
	cur, err := m.currentSum()
	if err != nil {
		return nil, err
	}
	st, err := m.loadState()
	if err != nil {
		return nil, err
	}
	for _, kg := range st.Kernels {
		if kg.SHA256 == cur {
			continue
		}
		k, err := m.storedKernel(kg)
		if err != nil {
			return nil, err
		}
		return m.switchTo(ctx, k, true)
	}
	return nil, ErrNoKnownGood
}

// switchTo 把 bin/mihomo 换成 k。validate 时先降权用 k 对生效的配置执行 -t。
func (m *Manager) switchTo(ctx context.Context, k *Kernel, validate bool) (*UpgradeResult, error) {
	res := &UpgradeResult{To: k.Version, Asset: k.Asset}
	if err := m.stage(k); err != nil {
		return nil, err
	}
	if validate {
		if err := m.validate(ctx, m.Layout.KernelBinary(k.Version)); err != nil {
			return nil, err
		}
	}

	l, err := m.lock(ctx, lockWait)
	if err != nil {
		return nil, err
	}
	defer l.Release()

	prev, err := os.ReadFile(m.Layout.CoreBinary())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	st, err := m.loadState()
	if err != nil {
		return nil, err
	}
	if prev != nil {
		if kg := st.FindKernel(Checksum(prev)); kg != nil {
			res.From = kg.Version
		}
	}
	if bytes.Equal(prev, k.Binary) {
		res.From = k.Version
		return res, m.confirmIfRunning(ctx, k.Version)
	}
	if err := m.writeBinary(k.Binary); err != nil {
		return nil, err
	}
	res.Changed = true

	running, err := m.running(ctx)
	if err != nil || !running {
		// 服务没有运行时只替换，下次启动并通过健康检查后才记为已知可用。
		return res, err
	}
	if _, err := m.restartAndCheck(ctx, m.Service.Restart, k.Version); err != nil {
		return nil, m.revert(ctx, prev, err)
	}
	res.Restarted = true
	return res, m.confirm(k.Version)
}

// revert 在新二进制没有通过健康检查后换回 prev 并再次重启。
func (m *Manager) revert(ctx context.Context, prev []byte, cause error) error {
	if prev == nil {
		_ = m.Service.Stop(ctx, service.CoreUnit)
		return cause
	}
	err := m.writeBinary(prev)
	if err == nil {
		_ = m.Service.ResetFailed(ctx, service.CoreUnit)
		_, err = m.restartAndCheck(ctx, m.Service.Restart, "")
	}
	if err != nil {
		return fmt.Errorf("%w; switching back to the previous binary also failed: %v", cause, err)
	}
	return fmt.Errorf("%w: %w", core.ErrRolledBack, cause)
}

// RescueResult 是一次救援的结果。
type RescueResult struct {
	// To 是换回的已知可用版本。
	To string `json:"to"`
}

// Rescue 在内核服务多次启动失败后由 systemd 触发：当前二进制不是已知可用版本时，换回最近的
// 已知可用版本，清除 failed 状态后启动并做健康检查。已经是已知可用版本时多半是配置或环境问题，
// 保持停止，返回说明原因的错误。拿不到锁时（升级等操作正在进行，并会自己回滚）返回 lock.ErrBusy。
func (m *Manager) Rescue(ctx context.Context) (*RescueResult, error) {
	l, err := m.lock(ctx, rescueLockWait)
	if err != nil {
		return nil, err
	}
	defer l.Release()

	cur, err := m.currentSum()
	if err != nil {
		return nil, err
	}
	st, err := m.loadState()
	if err != nil {
		return nil, err
	}
	if kg := st.FindKernel(cur); kg != nil {
		return nil, fmt.Errorf("mihomo %s is a known-good version but keeps failing; probably a configuration or environment problem, leaving it stopped (see: tunlr logs)", kg.Version)
	}
	if len(st.Kernels) == 0 {
		return nil, ErrNoKnownGood
	}
	k, err := m.storedKernel(st.Kernels[0])
	if err != nil {
		return nil, err
	}
	if err := m.writeBinary(k.Binary); err != nil {
		return nil, err
	}
	if err := m.Service.ResetFailed(ctx, service.CoreUnit); err != nil {
		m.log().Debug("reset-failed", "err", err)
	}
	if _, err := m.restartAndCheck(ctx, m.Service.Start, k.Version); err != nil {
		return nil, err
	}
	return &RescueResult{To: k.Version}, m.confirm(k.Version)
}

// 面板升级善后的结果。
const (
	AftercareNone     = "none"     // 没有内核，或内核没有运行
	AftercareKnown    = "known"    // 已经是已知可用版本
	AftercareVerified = "verified" // 与官方资产一致，记为已知可用
	AftercareReplaced = "replaced" // 换成了校验过的二进制
)

// AftercareResult 是面板升级善后的结果。
type AftercareResult struct {
	Action string `json:"action"`
	// Version 是运行中的版本（Action 为 verified、replaced 时）。
	Version string `json:"version,omitempty"`
	// Reason 说明替换的原因（Action 为 replaced 时）。
	Reason string `json:"reason,omitempty"`
}

// Aftercare 核对面板升级后的内核：bin/mihomo 不是已知可用版本时，查询运行中的版本，下载该版本的
// 官方资产比较。一致则记为已知可用；不一致则换成校验过的二进制，下载不到时换回最近的已知可用版本，
// 并重启做健康检查。
func (m *Manager) Aftercare(ctx context.Context) (*AftercareResult, error) {
	cur, err := m.currentSum()
	if err != nil || cur == "" {
		return &AftercareResult{Action: AftercareNone}, err
	}
	st, err := m.loadState()
	if err != nil {
		return nil, err
	}
	if st.FindKernel(cur) != nil {
		return &AftercareResult{Action: AftercareKnown}, nil
	}
	version, err := NewClient(m.Layout.CoreSocket()).Version(ctx)
	if errors.Is(err, ErrNotRunning) {
		// 没有运行的内核交给救援：起不来时由 systemd 触发。
		return &AftercareResult{Action: AftercareNone}, nil
	}
	if err != nil {
		return nil, err
	}

	k, ferr := m.fetchOfficial(ctx, version)
	if ferr == nil && k.SHA256() == cur {
		l, err := m.lock(ctx, lockWait)
		if err != nil {
			return nil, err
		}
		defer l.Release()
		if err := m.stage(k); err != nil {
			return nil, err
		}
		return &AftercareResult{Action: AftercareVerified, Version: version}, m.confirm(version)
	}

	res := &AftercareResult{Action: AftercareReplaced}
	if ferr == nil {
		res.Reason = fmt.Sprintf("bin/mihomo differs from the official %s", k.Asset)
	} else {
		res.Reason = fmt.Sprintf("could not get the official build of %s: %v", version, ferr)
		if len(st.Kernels) == 0 {
			return nil, fmt.Errorf("%s, and %w", res.Reason, ErrNoKnownGood)
		}
		if k, err = m.storedKernel(st.Kernels[0]); err != nil {
			return nil, err
		}
	}
	m.log().Warn("replacing an unverified kernel", "reason", res.Reason, "with", k.Version)
	up, err := m.switchTo(ctx, k, true)
	if err != nil {
		return nil, err
	}
	res.Version = up.To
	return res, nil
}

// fetchOfficial 下载 version 对应本机平台的官方资产。
func (m *Manager) fetchOfficial(ctx context.Context, version string) (*Kernel, error) {
	if !versionPattern.MatchString(version) {
		return nil, fmt.Errorf("%q is not a release version", version)
	}
	arch, err := m.arch()
	if err != nil {
		return nil, err
	}
	d, err := m.downloader()
	if err != nil {
		return nil, err
	}
	return FetchKernel(ctx, d, arch, version)
}

// Installed 报告是否已安装内核（bin/mihomo 存在）。
func (m *Manager) Installed() bool {
	_, err := os.Stat(m.Layout.CoreBinary())
	return err == nil
}

// KernelStatus 是内核的安装情况。
type KernelStatus struct {
	// Installed 报告 bin/mihomo 是否存在。
	Installed bool `json:"installed"`
	// KnownGood 报告 bin/mihomo 是否为已知可用版本，Version 是该版本。
	KnownGood bool   `json:"known_good"`
	Version   string `json:"version,omitempty"`
	// Kernels 是已知可用版本，最近确认的在前。
	Kernels []state.Kernel `json:"kernels"`
}

// Status 返回内核的安装情况。
func (m *Manager) Status() (*KernelStatus, error) {
	cur, err := m.currentSum()
	if err != nil {
		return nil, err
	}
	st, err := m.loadState()
	if err != nil {
		return nil, err
	}
	ks := &KernelStatus{Installed: cur != "", Kernels: st.Kernels}
	if ks.Kernels == nil {
		ks.Kernels = []state.Kernel{}
	}
	if kg := st.FindKernel(cur); cur != "" && kg != nil {
		ks.KnownGood, ks.Version = true, kg.Version
	}
	return ks, nil
}

// confirm 在内核通过健康检查后调用：bin/mihomo 与 kernels/<version>/ 中工具下载校验过的二进制一致时，
// 把它记为最近确认的已知可用版本，并清理多余的版本。调用方须持有锁。
func (m *Manager) confirm(version string) error {
	cur, err := m.currentSum()
	if err != nil || cur == "" {
		return err
	}
	stored, err := FileChecksum(m.Layout.KernelBinary(version))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if stored != cur {
		return nil
	}
	st, err := m.loadState()
	if err != nil {
		return err
	}
	kg := state.Kernel{Version: version, SHA256: cur, VerifiedAt: m.now().UTC()}
	arch, err := m.arch()
	if err == nil {
		kg.Asset = arch.KernelAsset(version)
	}
	st.Kernels = slices.DeleteFunc(st.Kernels, func(k state.Kernel) bool { return k.Version == version || k.SHA256 == cur })
	st.Kernels = slices.Insert(st.Kernels, 0, kg)
	if keep := orDefault(m.Keep, DefaultKeepKernels); len(st.Kernels) > keep {
		st.Kernels = st.Kernels[:keep]
	}
	if err := state.Save(m.Layout.StateFile(), st); err != nil {
		return err
	}
	return m.prune(st)
}

// confirmIfRunning 在二进制没有变化时补记已知可用：服务在运行且版本一致。
func (m *Manager) confirmIfRunning(ctx context.Context, version string) error {
	if v, err := NewClient(m.Layout.CoreSocket()).Version(ctx); err == nil && v == version {
		return m.confirm(version)
	}
	return nil
}

// prune 删除 kernels/ 中不在已知可用记录里的版本（包括校验未通过、下载后没有用上的）。
func (m *Manager) prune(st *state.State) error {
	entries, err := os.ReadDir(m.Layout.KernelsDir())
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if slices.ContainsFunc(st.Kernels, func(k state.Kernel) bool { return k.Version == e.Name() }) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.Layout.KernelsDir(), e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stage 把下载校验过的内核存到 kernels/<版本>/（root 所有，内核用户可以执行，用于 -t）。
func (m *Manager) stage(k *Kernel) error {
	path := m.Layout.KernelBinary(k.Version)
	if have, err := FileChecksum(path); err == nil && have == k.SHA256() {
		return nil
	}
	for _, dir := range []string{m.Layout.KernelsDir(), filepath.Dir(path)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return fsutil.WriteFile(path, k.Binary, 0o755)
}

// storedKernel 读取 kernels/ 中的已知可用版本，并核对记录的 sha256。
func (m *Manager) storedKernel(kg state.Kernel) (*Kernel, error) {
	data, err := os.ReadFile(m.Layout.KernelBinary(kg.Version))
	if err != nil {
		return nil, fmt.Errorf("known-good mihomo %s: %w", kg.Version, err)
	}
	if Checksum(data) != kg.SHA256 {
		return nil, fmt.Errorf("known-good mihomo %s: the stored binary does not match the recorded sha256", kg.Version)
	}
	return &Kernel{Version: kg.Version, Asset: kg.Asset, Binary: data}, nil
}

// validate 降权用 binary 对生效的配置执行 -t；尚未发布配置时跳过。
func (m *Manager) validate(ctx context.Context, binary string) error {
	cfg, err := os.ReadFile(m.Layout.CoreConfig())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	d, err := m.downloader()
	if err != nil {
		return err
	}
	v := &Validator{Binary: binary, HomeDir: m.Layout.CoreDir(), Download: d, RunAs: m.Owner}
	if err := v.Validate(ctx, cfg); err != nil {
		return fmt.Errorf("the published configuration does not work with the new kernel: %w", err)
	}
	return nil
}

// writeBinary 原子替换 bin/mihomo，归内核用户所有（面板升级要能原地改写它和所在目录）。
func (m *Manager) writeBinary(data []byte) error {
	dir := m.Layout.BinDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := m.Owner.Chown(dir); err != nil {
		return err
	}
	return fsutil.WriteFileOwned(m.Layout.CoreBinary(), data, 0o755, m.Owner)
}

func (m *Manager) running(ctx context.Context) (bool, error) {
	if !m.Service.Installed() {
		return false, nil
	}
	st, err := m.Service.Status(ctx, service.CoreUnit)
	return st.Active(), err
}

// currentSum 返回 bin/mihomo 的 sha256，不存在时为空。
func (m *Manager) currentSum() (string, error) {
	sum, err := FileChecksum(m.Layout.CoreBinary())
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return sum, err
}

func (m *Manager) loadState() (*state.State, error) {
	st, _, err := state.LoadOrNew(m.Layout.StateFile(), m.now())
	return st, err
}

func (m *Manager) arch() (Arch, error) {
	if m.Arch.GOARCH != "" {
		return m.Arch, nil
	}
	return DetectArch()
}

func (m *Manager) downloader() (*download.Client, error) {
	if m.Download != nil {
		return m.Download, nil
	}
	set, err := settings.Load(m.Layout.SettingsFile())
	if err != nil {
		return nil, err
	}
	return &download.Client{Mirror: set.GitHubMirror}, nil
}

func (m *Manager) lock(ctx context.Context, wait time.Duration) (*lock.Lock, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return lock.Acquire(ctx, m.Layout.LockFile())
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.New(slog.DiscardHandler)
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
