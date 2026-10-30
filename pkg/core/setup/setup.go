// Package setup 编排跨模块的操作：安装（init）、卸载、定时更新（_update）和状态汇总。
// 各步骤的实现在 mihomo、service、subscription、pipeline 等包中，这里只决定顺序和失败时的处理。
package setup

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
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/doctor"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/lock"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// lockWait 是等待其他 tunlr 操作释放锁的最长时间。
const lockWait = 30 * time.Second

// Installer 执行安装、卸载、定时更新和状态汇总。
type Installer struct {
	Layout paths.Layout
	// Version 是 tunlr 的版本。
	Version string
	// Systemd 为空时使用默认的 systemctl。
	Systemd *service.Systemd
	// Download 为空时按工具设置中的镜像下载。
	Download *download.Client
	// Now 为空时使用 time.Now。
	Now func() time.Time
	// Log 为空时不记录日志。
	Log *slog.Logger
	// Validator、Reloader 供测试替换配置的校验与内核的热重载，为空时使用默认实现。
	Validator pipeline.Validator
	Reloader  pipeline.Reloader
	// Rules 为空时使用默认的 nft 与 ip 命令。
	Rules *netfilter.Rules
	// CheckNetwork 是开启透明代理后的检测，为空时使用默认的检测（测试时替换）。
	CheckNetwork func(ctx context.Context, opts TunOptions) []TunCheck
	// Host 是 doctor 检查的本机，为空时使用真实的系统（测试时替换）。
	Host *doctor.Host
	// Interfaces 查找网卡，为空时使用 netfilter.LookupInterface（测试时替换）。
	Interfaces func(name string) (*netfilter.Interface, error)
	// InterfaceNames 列出本机的网卡名，为空时使用 net.Interfaces（测试时替换）。
	InterfaceNames func() ([]string, error)
	// LookupNames 按地址查询设备名称，为空时向路由器反向解析（测试时替换）。
	LookupNames func(ctx context.Context, ips []string) map[string]string
	// Connections 返回内核当前连接按来源地址的计数，为空时经 API 获取（测试时替换）。
	Connections func(ctx context.Context) (map[string]int, error)
	// RunTool 执行 path 处的 tunlr 二进制并返回它的标准输出，为空时真的执行（测试时替换）。升级时用它确认
	// 下载的二进制能运行，并让新版本执行 init。
	RunTool func(ctx context.Context, path string, args ...string) ([]byte, error)
}

// InitOptions 是 init 的参数。
type InitOptions struct {
	// Mirror 不为 nil 时把工具设置的 github-mirror 设为它（空字符串表示直连）。
	Mirror *string
	// SubName、SubURL 给出时添加订阅；已有同名订阅时不再添加。
	SubName, SubURL string
	// SubFilter、SubExclude 是添加订阅时写入它的节点筛选（见 subscription.AddOptions）。
	SubFilter, SubExclude string
	// KernelVersion 是没有内核时安装的版本，为空时为最新正式发布。
	KernelVersion string
	// KernelFile 不为空时，没有内核时从这个事先下载好的官方资产文件安装（mihomo.LoadKernelFile），
	// 与 KernelVersion 不能同时给出。
	KernelFile string
	// Progress 在每一步开始时调用，可为空。
	Progress func(step string)
}

// InitResult 是 init 的结果。
type InitResult struct {
	// ToolInstalled 报告是否把工具复制（或更新）到了 /usr/local/bin/tunlr。
	ToolInstalled bool `json:"tool_installed"`
	// UserCreated 报告是否新建了内核用户。
	UserCreated bool `json:"user_created"`
	// Kernel 是新安装的内核，已有内核时为 nil。
	Kernel *mihomo.UpgradeResult `json:"kernel,omitempty"`
	// SubscriptionAdded 是新添加的订阅。
	SubscriptionAdded string `json:"subscription_added,omitempty"`
	// Revision 是本次发布的配置修订，没有发布（没有变化或没有启用的订阅）时为 0。
	Revision int `json:"revision,omitempty"`
	// Started 是启动后的健康检查结果；没有启动时为 nil，原因见 NotStarted。
	Started    *mihomo.Health `json:"started,omitempty"`
	NotStarted string         `json:"not_started,omitempty"`
	// PanelLAN 报告面板的局域网访问是否开启（局域网设备可以直接打开面板）。
	PanelLAN bool `json:"panel_lan"`
}

// Init 安装或修复 tunlr，可以重复执行：检查环境 → 把自身复制到 /usr/local/bin/tunlr → 创建内核用户 →
// 建目录并设置属主与权限 → 没有内核时安装 → 写入 unit、启用服务和定时更新 → 添加订阅 →
// 有启用的订阅时发布配置 → 有配置时（重新）启动内核并做健康检查。
func (i *Installer) Init(ctx context.Context, opts InitOptions) (*InitResult, error) {
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	res := &InitResult{}

	progress("Checking the environment")
	if err := i.CheckSupport(ctx); err != nil {
		return nil, err
	}
	// 筛选写错时在动手之前拒绝，免得装好内核后才在添加订阅时失败。
	for _, s := range []string{opts.SubFilter, opts.SubExclude} {
		if err := config.CheckPatterns(s); err != nil {
			return nil, fmt.Errorf("%w: %v", core.ErrInvalidArgument, err)
		}
	}
	arch, err := mihomo.DetectArch()
	if err != nil {
		return nil, err
	}

	progress("Installing tunlr to " + i.Layout.ToolBinary())
	if res.ToolInstalled, err = i.installSelf(); err != nil {
		return nil, fmt.Errorf("install tunlr: %w", err)
	}

	progress("Creating the user " + service.CoreUser)
	existed, err := service.LookupUser()
	if err != nil {
		return nil, err
	}
	owner, err := i.systemd().EnsureUser(ctx)
	if err != nil {
		return nil, err
	}
	res.UserCreated = existed == nil

	if opts.Mirror != nil {
		progress("Saving the GitHub mirror setting")
		if err := settings.Set(i.Layout.SettingsFile(), "github-mirror", *opts.Mirror); err != nil {
			return nil, err
		}
	}

	progress("Preparing directories")
	if err := i.prepareDirs(owner); err != nil {
		return nil, err
	}

	kernels := i.kernels(owner, arch)
	if !kernels.Installed() {
		progress(fmt.Sprintf("Installing mihomo for %s", arch))
		if opts.KernelFile != "" {
			res.Kernel, err = kernels.UpgradeFrom(ctx, opts.KernelFile)
		} else {
			res.Kernel, err = kernels.Upgrade(ctx, opts.KernelVersion)
		}
		if err != nil {
			return nil, fmt.Errorf("install mihomo: %w", err)
		}
	}

	progress("Installing the systemd units")
	if err := i.systemd().Install(ctx); err != nil {
		return nil, err
	}

	list, err := subscription.LoadList(i.Layout.SubscriptionList())
	if err != nil {
		return nil, err
	}
	if opts.SubName != "" && list.Find(opts.SubName) == nil {
		progress("Adding the subscription " + opts.SubName)
		if _, err := i.subscriptions().Add(ctx, opts.SubName, opts.SubURL, subscription.AddOptions{Filter: opts.SubFilter, Exclude: opts.SubExclude}); err != nil {
			return nil, err
		}
		res.SubscriptionAdded = opts.SubName
		if list, err = subscription.LoadList(i.Layout.SubscriptionList()); err != nil {
			return nil, err
		}
	}

	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	res.PanelLAN = st != nil && st.PanelLAN != nil
	if list.Active != "" {
		progress("Publishing the configuration")
		p := i.pipeline(owner)
		plan, err := p.Plan(ctx)
		if err != nil {
			return nil, err
		}
		if plan.Check != nil {
			return nil, plan.Check
		}
		if plan.Changed() {
			applied, err := p.Apply(ctx, plan)
			if err != nil {
				return nil, err
			}
			res.Revision = applied.Revision.Number
		}
	}

	if _, err := os.Stat(i.Layout.CoreConfig()); err != nil {
		res.NotStarted = "no configuration has been published yet: add a subscription (tunlr sub add), then run tunlr config apply and tunlr core start"
	} else {
		progress("Starting mihomo")
		if res.Started, err = kernels.Restart(ctx); err != nil {
			return nil, err
		}
	}

	// timer 最后启用：重写过的 timer 重新启用时可能立即触发（上次触发已过了一个周期），
	// 定时更新不能与上面的发布同时进行。
	progress("Scheduling the updates")
	if err := i.SyncTimers(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

// InitSteps 按当前状态列出 Init(ctx, opts) 将做的事，供执行前确认；只读取，不做改动。
func (i *Installer) InitSteps(ctx context.Context, opts InitOptions) ([]string, error) {
	var steps []string
	self, err := selfPath()
	if err != nil {
		return nil, err
	}
	if same, err := sameFile(self, i.Layout.ToolBinary()); err != nil {
		return nil, err
	} else if !same {
		steps = append(steps, "Copy tunlr to "+i.Layout.ToolBinary())
	}
	if u, err := service.LookupUser(); err != nil {
		return nil, err
	} else if u == nil {
		steps = append(steps, "Create the system user "+service.CoreUser+" that runs mihomo")
	}
	if opts.Mirror != nil {
		set, err := settings.Load(i.Layout.SettingsFile())
		if err != nil {
			return nil, err
		}
		switch {
		case *opts.Mirror == set.GitHubMirror:
		case *opts.Mirror == "":
			steps = append(steps, "Download from GitHub directly (remove the mirror setting)")
		default:
			steps = append(steps, "Download from GitHub through "+*opts.Mirror)
		}
	}
	if !i.kernels(nil, mihomo.Arch{}).Installed() {
		v := cmp.Or(opts.KernelVersion, "(latest release)")
		if opts.KernelFile != "" {
			_, version, err := mihomo.ParseKernelAsset(filepath.Base(opts.KernelFile))
			if err != nil {
				return nil, err
			}
			v = version + " from " + opts.KernelFile
		}
		steps = append(steps, "Install mihomo "+v+", verified against the official sha256")
	}
	sd := i.systemd()
	steps = append(steps, fmt.Sprintf("Install and enable %s, %s (subscriptions %s unless set otherwise), %s (checks mihomo after the panel upgrades it) "+
		"and %s (reloads tunlr's nftables rules after nftables.service)",
		service.CoreUnit, service.UpdateTimer, settings.DescribePeriod(subscription.DefaultUpdate), service.AftercarePath, service.FirewallUnit))

	list, err := subscription.LoadList(i.Layout.SubscriptionList())
	if err != nil {
		return nil, err
	}
	active := list.Active
	if opts.SubName != "" && list.Find(opts.SubName) == nil {
		steps = append(steps, fmt.Sprintf("Add the subscription %q", opts.SubName))
		active = cmp.Or(active, opts.SubName)
	}
	if active != "" {
		steps = append(steps, fmt.Sprintf("Publish the configuration from %q if it changed", active))
	}
	if _, err := os.Stat(i.Layout.CoreConfig()); err != nil && active == "" {
		return steps, nil
	}
	if sd.Installed() {
		if s, err := sd.Status(ctx, service.CoreUnit); err == nil && s.Active() {
			return append(steps, "Restart mihomo (open connections drop)"), nil
		}
	}
	return append(steps, "Start mihomo"), nil
}

// selfPath 是正在运行的 tunlr 的路径（解析符号链接）。
func selfPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(self)
}

// sameFile 报告 a、b 的内容是否相同；b 不存在时为 false。边读边算校验值，不把二进制读进内存。
func sameFile(a, b string) (bool, error) {
	sa, err := mihomo.FileChecksum(a)
	if err != nil {
		return false, err
	}
	sb, err := mihomo.FileChecksum(b)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return sa == sb, err
}

// installSelf 把正在运行的 tunlr 复制到 /usr/local/bin/tunlr（unit 引用这个路径）。内容相同时什么都不做。
func (i *Installer) installSelf() (bool, error) {
	self, err := selfPath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return false, err
	}
	dst := i.Layout.ToolBinary()
	if have, err := os.ReadFile(dst); err == nil && bytes.Equal(have, data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	return true, fsutil.WriteFile(dst, data, 0o755)
}

// prepareDirs 建立目录并设置属主与权限：/var/lib/tunlr 为 0711，内核用户只能进入它拥有的 bin/、core/，
// 不能读取订阅、历史和状态文件；bin/、core/ 及其中已有的文件（如 MVP 1 时由 root 写入的）归内核用户。
func (i *Installer) prepareDirs(owner *fsutil.Owner) error {
	l := i.Layout
	if err := os.MkdirAll(l.DataDir(), 0o711); err != nil {
		return err
	}
	if err := os.Chmod(l.DataDir(), 0o711); err != nil {
		return err
	}
	if err := os.MkdirAll(l.KernelsDir(), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(l.BinDir(), 0o755); err != nil {
		return err
	}
	if err := owner.ChownTree(l.BinDir()); err != nil {
		return err
	}
	if err := mihomo.EnsureHome(l.CoreDir(), owner); err != nil {
		return err
	}
	return owner.ChownTree(l.CoreDir())
}

// UninstallResult 是卸载的结果。
type UninstallResult struct {
	// Removed 是删除的路径和用户。
	Removed []string `json:"removed"`
	// Kept 是保留的路径（未指定 purge 时）。
	Kept []string `json:"kept,omitempty"`
}

// Uninstall 停止并禁用内核服务与定时更新，删除 unit、bin/、core/、kernels/ 和内核用户。
// 保留 /etc/tunlr、订阅、历史、状态文件和 /usr/local/bin/tunlr，purge 时一并删除。
func (i *Installer) Uninstall(ctx context.Context, purge bool) (*UninstallResult, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()

	res := &UninstallResult{}
	if st, err := state.Load(i.Layout.StateFile()); err == nil && st.Gateway != nil {
		if err := i.gatewayOffLocked(ctx, st); err != nil {
			return nil, err
		}
		res.Removed = append(res.Removed, "gateway mode ("+i.Layout.SysctlFile()+", nftables table inet "+netfilter.GatewayTable+")")
	}
	// 停止内核服务时 _poststop 删除规则；服务没在运行（或 unit 是旧版）时这里再删一次。
	if err := i.systemd().Uninstall(ctx); err != nil {
		return nil, err
	}
	if left, err := i.rules().Down(ctx); err != nil {
		i.log().Warn("remove the nftables rules", "err", err)
	} else if left {
		res.Removed = append(res.Removed, "nftables table inet "+netfilter.KernelTable)
	}
	// 内核已经停止，访问限制的表不再需要；开关留在状态文件中，重新 init 后 _prestart 照样加载。
	if tables, err := i.rules().Tables(ctx); err == nil && tables[netfilter.AccessTable] {
		if err := i.rules().AccessDown(ctx); err != nil {
			return nil, err
		}
		res.Removed = append(res.Removed, "nftables table inet "+netfilter.AccessTable)
	}
	if tables, err := i.rules().Tables(ctx); err == nil && tables[netfilter.DevicesTable] {
		if err := i.rules().DevicesDown(ctx); err != nil {
			return nil, err
		}
		res.Removed = append(res.Removed, "nftables table inet "+netfilter.DevicesTable)
	}
	res.Removed = append(res.Removed, filepath.Join(i.Layout.UnitDir(), "tunlr-*"))

	lay := i.Layout
	targets := []string{lay.BinDir(), lay.CoreDir(), lay.KernelsDir()}
	if purge {
		targets = append(targets, lay.ConfigDir(), lay.DataDir(), lay.ToolBinary())
	}
	for _, p := range targets {
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return res, err
		}
		res.Removed = append(res.Removed, p)
	}

	if o, err := service.LookupUser(); err != nil {
		return res, err
	} else if o != nil {
		if err := i.systemd().RemoveUser(ctx); err != nil {
			return res, err
		}
		res.Removed = append(res.Removed, "user "+service.CoreUser)
	}

	if !purge {
		// 已知可用版本的二进制已经删除；生效配置也已删除，重新 init 时透明代理保持关闭。
		if st, err := state.Load(lay.StateFile()); err == nil && (len(st.Kernels) > 0 || st.Tun != "") {
			st.Kernels = nil
			st.SetTun("", time.Time{})
			if err := state.Save(lay.StateFile(), st); err != nil {
				return res, err
			}
		}
		for _, p := range []string{lay.ConfigDir(), lay.DataDir(), lay.ToolBinary()} {
			if _, err := os.Stat(p); err == nil {
				res.Kept = append(res.Kept, p)
			}
		}
	}
	return res, nil
}

func (i *Installer) systemd() *service.Systemd {
	if i.Systemd != nil {
		return i.Systemd
	}
	return &service.Systemd{Layout: i.Layout}
}

// kernels 返回内核管理器；owner 为 nil 时查找内核用户。
func (i *Installer) kernels(owner *fsutil.Owner, arch mihomo.Arch) *mihomo.Manager {
	if owner == nil {
		owner, _ = service.KernelUser(i.Layout)
	}
	return &mihomo.Manager{
		Layout: i.Layout, Service: i.systemd(), Download: i.Download,
		Arch: arch, Owner: owner, Now: i.Now, Log: i.Log,
	}
}

// downloader 返回下载 GitHub release 的客户端：Download 为空时按工具设置中的镜像下载。
func (i *Installer) downloader() (*download.Client, error) {
	if i.Download != nil {
		return i.Download, nil
	}
	set, err := settings.Load(i.Layout.SettingsFile())
	if err != nil {
		return nil, err
	}
	return &download.Client{Mirror: set.GitHubMirror, Log: i.Log}, nil
}

func (i *Installer) pipeline(owner *fsutil.Owner) *pipeline.Pipeline {
	return &pipeline.Pipeline{
		Layout: i.Layout, Version: i.Version, Download: i.Download, Owner: owner, Now: i.Now,
		Validator: i.Validator, Reloader: i.Reloader, Restarter: i.kernels(owner, mihomo.Arch{}),
	}
}

func (i *Installer) lock(ctx context.Context) (*lock.Lock, error) {
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	return lock.Acquire(ctx, i.Layout.LockFile())
}

func (i *Installer) log() *slog.Logger {
	if i.Log != nil {
		return i.Log
	}
	return slog.New(slog.DiscardHandler)
}

// subscriptions 返回管理订阅的 subscription.Manager。
func (i *Installer) subscriptions() *subscription.Manager {
	return &subscription.Manager{Layout: i.Layout, Version: i.Version, Now: i.Now}
}
