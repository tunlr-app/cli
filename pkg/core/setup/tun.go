package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/doctor"
	"github.com/tunlr-app/cli/pkg/core/lock"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// GuardTimeout 是开启透明代理后等待确认的期限：检测没有在期限内通过（例如 SSH 断开、tunlr 被杀），
// 回滚定时器关闭 TUN。
const GuardTimeout = 3 * time.Minute

// TunStatus 是透明代理的状态。开关以状态文件为准，其余以实际情况为准。
type TunStatus struct {
	// Switch 是开关：off、pending（已开启、等待检测通过）或 on。
	Switch string `json:"switch"`
	// Deadline 是 pending 的期限，过期仍未确认时关闭。
	Deadline time.Time `json:"deadline,omitzero"`
	// Running 报告内核实际是否开启了 TUN（面板可以临时切换）；内核没有响应时为 nil。
	Running *bool `json:"running,omitempty"`
	// Device 报告 TUN 网卡是否存在。
	Device bool `json:"device"`
	// Rules 报告工具的 inet tunlr 表是否存在，KernelRules 报告内核的 inet mihomo 表是否存在。
	Rules       bool `json:"rules"`
	KernelRules bool `json:"kernel_rules"`
	// Gateway 是网关模式的局域网网卡，网关关闭时为空。
	Gateway string `json:"gateway,omitempty"`
}

// TunCheck 是开启透明代理后一项检测的结果。
type TunCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// TunResult 是开关透明代理的结果。
type TunResult struct {
	// Revision 是发布的配置修订；配置已经是目标状态（如上次开启被中断）时为 nil。
	Revision *config.Revision `json:"revision,omitempty"`
	// Checks 是开启后的检测结果（仅 tun on）。
	Checks []TunCheck `json:"checks,omitempty"`
}

// TunOptions 是 tun on 的参数。
type TunOptions struct {
	// SSHConnection 是 $SSH_CONNECTION，在 SSH 会话中运行时用于检测会话是否仍然通畅；为空时跳过。
	SSHConnection string
	// SkipSSH 不为空时跳过 SSH 检测，说明原因（如在 tmux 中运行，输出不一定经过 $SSH_CONNECTION 那条连接）。
	SkipSSH string
	// Probe 在检测 SSH 会话时调用，须向用户的终端输出一些文字，让 sshd 有数据要发。
	Probe func()
	// Progress 在每一步开始时调用，可为空。
	Progress func(step string)
}

// TunStatus 返回透明代理的状态。
func (i *Installer) TunStatus(ctx context.Context) (*TunStatus, error) {
	s := &TunStatus{Switch: "off", Device: netfilter.DeviceUp()}
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if st != nil && st.Tun != "" {
		s.Switch, s.Deadline = st.Tun, st.TunDeadline
	}
	if st != nil && st.Gateway != nil {
		s.Gateway = st.Gateway.Interface
	}
	if on, err := mihomo.NewClient(i.Layout.CoreSocket()).TunEnabled(ctx); err == nil {
		s.Running = &on
	}
	if tables, err := i.rules().Tables(ctx); err == nil {
		s.Rules, s.KernelRules = tables[netfilter.Table], tables[netfilter.KernelTable]
	}
	return s, nil
}

// TunPreflight 检查能否开启透明代理：服务已安装、内核在运行（否则无法检测），以及 doctor 的 tun 组
// （TUN 设备、nft、ip、ip rule 优先级等）。不满足时返回满足 errors.Is(err, core.ErrCheckFailed) 的错误；
// warnings 是不阻止开启、但可能影响使用的情况。
func (i *Installer) TunPreflight(ctx context.Context) (warnings []string, err error) {
	if !i.systemd().Installed() {
		return nil, fmt.Errorf("%w: %w", core.ErrCheckFailed, mihomo.ErrServiceNotInstalled)
	}
	if _, err := mihomo.NewClient(i.Layout.CoreSocket()).Version(ctx); err != nil {
		return nil, fmt.Errorf("%w: mihomo is not responding (%v); start it first: tunlr core start", core.ErrCheckFailed, err)
	}
	return doctor.Failures(i.host().Tun(ctx, i.rules(), i.coreUID()))
}

// TunOn 开启透明代理并发布 plan（pipeline.PlanTun(ctx, true) 的结果，已经用户确认）：
// 加锁 → 开关记为 pending 并布置回滚定时器 → 发布 → 释放锁并检测 → 通过则记为 on、撤掉定时器；
// 检测失败立即关闭，返回满足 errors.Is(err, core.ErrRolledBack) 的错误，结果中有各项检测的情况。
// 发布失败时开关恢复原状，配置由流水线回滚。
func (i *Installer) TunOn(ctx context.Context, plan *pipeline.Plan, opts TunOptions) (*TunResult, error) {
	p := i.pipeline(nil)
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	res := &TunResult{}
	sd := i.systemd()

	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		l.Release()
		return nil, err
	}
	previous, previousDeadline := st.Tun, st.TunDeadline
	st.SetTun(state.TunPending, i.now().Add(GuardTimeout))
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		l.Release()
		return nil, err
	}
	progress(fmt.Sprintf("Arming the rollback timer (TUN is turned off in %s unless the checks pass)", GuardTimeout))
	err = sd.ArmGuard(ctx, GuardTimeout)
	if err == nil {
		// 规则被清空的检查随开关启用；开启失败、开关回到关闭后由 _check 停用自己。
		err = sd.SetTimer(ctx, service.CheckTimer, true)
	}
	if err == nil {
		// 通常 _prestart 已经加载过；内核服务由旧版 unit 启动时还没有。在开启 TUN 之前加载，
		// 已经建立的连接（如这个 SSH 会话）在对方发来下一个包时就会标记，回包不进 TUN。
		err = i.rules().Up(ctx)
	}
	if err == nil {
		progress("Publishing the configuration")
		var applied *pipeline.ApplyResult
		if applied, err = p.ApplyLocked(ctx, plan); applied != nil {
			res.Revision = applied.Revision
		}
	}
	if err != nil {
		st.SetTun(previous, previousDeadline)
		if serr := state.Save(i.Layout.StateFile(), st); serr != nil {
			err = errors.Join(err, serr)
		}
		if previous != state.TunPending {
			sd.DisarmGuard(ctx)
		}
		l.Release()
		return nil, err
	}
	l.Release()

	progress("Checking the network")
	check := i.CheckNetwork
	if check == nil {
		check = i.checkNetwork
	}
	res.Checks = check(ctx, opts)
	var failed []string
	for _, c := range res.Checks {
		if c.Status == StepFailed {
			failed = append(failed, c.Name+": "+c.Message)
		}
	}
	if len(failed) > 0 {
		progress("Turning TUN off again")
		if err := i.tunGuard(ctx, true); err != nil {
			return res, fmt.Errorf("the checks failed (%s), and turning TUN off failed: %w", strings.Join(failed, "; "), err)
		}
		return res, fmt.Errorf("%w: TUN is off again because the checks failed: %s", core.ErrRolledBack, strings.Join(failed, "; "))
	}

	l, err = i.lock(ctx)
	if err != nil {
		return res, err
	}
	defer l.Release()
	if st, err = state.Load(i.Layout.StateFile()); err != nil {
		return res, err
	}
	if st.Tun != state.TunPending {
		return res, fmt.Errorf("%w: the rollback timer turned TUN off before the checks finished", core.ErrRolledBack)
	}
	st.SetTun(state.TunOn, time.Time{})
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		return res, err
	}
	sd.DisarmGuard(ctx)
	return res, nil
}

// TunOff 关闭透明代理并发布 plan（pipeline.PlanTun(ctx, false) 的结果，已经用户确认），
// 撤掉可能存在的回滚定时器。网关开启时先关闭网关（它依赖 TUN）。发布失败时开关恢复原状，
// 配置由流水线回滚；已经关闭的网关不再恢复。
func (i *Installer) TunOff(ctx context.Context, plan *pipeline.Plan) (*TunResult, error) {
	p := i.pipeline(nil)
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return nil, err
	}
	if err := i.gatewayOffLocked(ctx, st); err != nil {
		return nil, err
	}
	previous, previousDeadline := st.Tun, st.TunDeadline
	st.SetTun("", time.Time{})
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		return nil, err
	}
	applied, err := p.ApplyLocked(ctx, plan)
	if err != nil {
		st.SetTun(previous, previousDeadline)
		return nil, errors.Join(err, state.Save(i.Layout.StateFile(), st))
	}
	i.systemd().DisarmGuard(ctx)
	// 局域网访问开着时 tunlr-check.timer 照样要在。
	i.syncCheckTimer(ctx, st)
	res := &TunResult{}
	if applied != nil {
		res.Revision = applied.Revision
	}
	return res, nil
}

// TunGuard 是回滚定时器执行的 _tun-guard：开关仍为 pending（检测没有在期限内通过）时关闭透明代理。
// reverted 报告是否关闭了。
func (i *Installer) TunGuard(ctx context.Context) (reverted bool, err error) {
	st, err := state.Load(i.Layout.StateFile())
	if err != nil || st.Tun != state.TunPending {
		return false, ignoreNotExist(err)
	}
	return true, i.tunGuard(ctx, true)
}

// tunGuard 在开关为 pending 时关闭透明代理：开关记为 off，只改写生效配置中受管 mixin 独占的键并发布。
// 发布失败时停止内核，由 _poststop 清理规则，保证网络恢复直连。
//
// wait 为 false 用于内核服务启动前（_prestart）：只尝试一次加锁，锁被占用（有人正在操作）就什么都
// 不做；发布失败时也不停止内核服务，在 ExecStartPre 中停止自己所在的服务会被 systemd 终止。
func (i *Installer) tunGuard(ctx context.Context, wait bool) error {
	var l *lock.Lock
	var err error
	if wait {
		l, err = i.lock(ctx)
	} else {
		l, err = lock.TryAcquire(i.Layout.LockFile())
		if errors.Is(err, lock.ErrBusy) {
			i.log().Info("skip turning the unconfirmed TUN off: another tunlr operation is in progress")
			return nil
		}
	}
	if err != nil {
		return err
	}
	defer l.Release()

	st, err := state.Load(i.Layout.StateFile())
	if err != nil || st.Tun != state.TunPending {
		return ignoreNotExist(err)
	}
	if err := i.gatewayOffLocked(ctx, st); err != nil {
		// 继续关闭 TUN：网关的表还在时局域网设备断网（失败时关闭），不影响本机。
		i.log().Error("turn the unconfirmed TUN off: turning the gateway off failed", "err", err)
	}
	st.Gateway = nil
	st.SetTun("", time.Time{})
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		return err
	}
	i.systemd().DisarmGuard(ctx)
	p := i.pipeline(nil)
	plan, err := p.PlanEnforce(ctx)
	if err == nil && plan != nil {
		_, err = p.ApplyLocked(ctx, plan)
	}
	if err != nil && !wait {
		return fmt.Errorf("publishing the configuration without TUN failed: %w", err)
	}
	if err != nil {
		i.log().Error("turn the unconfirmed TUN off: publishing failed, stopping mihomo", "err", err)
		if serr := i.systemd().Stop(ctx, service.CoreUnit); serr != nil {
			return errors.Join(err, serr)
		}
		return fmt.Errorf("publishing the configuration without TUN failed, so mihomo is stopped: %w", err)
	}
	i.log().Info("turned the unconfirmed TUN off")
	return nil
}

// PreStart 是内核服务启动前（ExecStartPre，以 root）执行的 _prestart：开关仍为 pending 且已过期限时
// 先关闭 TUN（重启机器会丢掉回滚定时器），再加载工具的规则；网关开启、代理端口或面板对其他设备开放时重新
// 加载它们的规则（nft 规则不跨重启保存），访问限制在内核开始监听之前就位。不等待文件锁：tunlr core start
// 等命令持锁调用 systemctl start 时会死锁。
func (i *Installer) PreStart(ctx context.Context) error {
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var errs []error
	if st != nil && st.Tun == state.TunPending && i.now().After(st.TunDeadline) {
		errs = append(errs, i.tunGuard(ctx, false))
		if st, err = state.Load(i.Layout.StateFile()); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	errs = append(errs, i.rules().Up(ctx))
	if actions, err := i.repairGateway(ctx, st); err != nil {
		errs = append(errs, err)
	} else if len(actions) > 0 {
		i.log().Info("gateway: " + strings.Join(actions, "; "))
	}
	if action, err := i.repairAccess(ctx, st); err != nil {
		errs = append(errs, err)
	} else if action != "" {
		i.log().Info("LAN access: " + action)
	}
	// 记录设备的表：同时按生效配置更新代理端口（mixin 可能改了 mixed-port）。失败不阻止内核启动。
	if err := i.syncDevices(ctx, st); err != nil {
		i.log().Warn("load the nftables table that records LAN devices", "err", err)
	}
	return errors.Join(errs...)
}

// PostStop 是内核以任何方式退出后（ExecStopPost，以 root）执行的 _poststop：删除工具的规则，以及内核
// 没能自己清理的规则（被 SIGKILL 或崩溃时）。网关的规则保留：内核不在时局域网设备断网，而不是直连。
func (i *Installer) PostStop(ctx context.Context) error {
	left, err := i.rules().Down(ctx)
	if left {
		i.log().Warn("removed the nftables table mihomo left behind (it did not exit cleanly)")
	}
	return err
}

// CheckResult 是定时检查（_check）的结果。
type CheckResult struct {
	// Actions 是采取的修复动作，没有问题时为空。
	Actions []string `json:"actions,omitempty"`
}

// Check 是 tunlr-check.timer 每 5 分钟执行的 _check：补上被清空的规则（例如手动 nft flush ruleset；
// 重新加载 nftables.service 时由 _firewall 立即补上）：网关的转发设置和规则（也包括 Docker 晚于内核启动、
// 重建了 DOCKER-USER 链）、代理端口和面板的访问限制、记录设备的表，内核在运行时工具的 inet tunlr；
// 内核开着 TUN 而它的表不见了时重启内核，由内核重建。开关为 pending 且已过期限一分钟以上（回滚定时器
// 没有生效）时关闭 TUN。
//
// tunlr-check.timer 在透明代理、网关或局域网访问开着时启用（见 checkWanted），都关闭后（如回滚定时器
// 或 _prestart 关闭了 TUN）停用自己。面板临时打开的 TUN 不在保护范围内。
func (i *Installer) Check(ctx context.Context) (*CheckResult, error) {
	res := &CheckResult{}
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if st != nil && st.Tun == state.TunPending && i.now().After(st.TunDeadline.Add(time.Minute)) {
		if err := i.tunGuard(ctx, true); err != nil {
			return res, err
		}
		res.Actions = append(res.Actions, "turned off the TUN that was never confirmed")
		if st, err = state.Load(i.Layout.StateFile()); err != nil {
			return res, err
		}
	}
	if !checkWanted(st) {
		if err := i.systemd().SetTimer(ctx, service.CheckTimer, false); err != nil {
			return res, err
		}
		res.Actions = append(res.Actions, "stopped "+service.CheckTimer+": TUN, the gateway and LAN access are off")
		return res, nil
	}
	actions, err := i.repairTables(ctx, st)
	res.Actions = append(res.Actions, actions...)
	if err != nil {
		return res, err
	}
	// tunlr-firewall.service 随 nftables.service 停止后不随它启动（PartOf 只传播停止和重启），停着时收不到
	// 之后的重新加载：这里拉起来。_check 不持锁，_firewall 取锁不会等它。
	if status, err := i.systemd().Status(ctx, service.FirewallUnit); err == nil && !status.Active() {
		if err := i.systemd().SetTimer(ctx, service.FirewallUnit, true); err != nil {
			return res, fmt.Errorf("starting %s: %w", service.FirewallUnit, err)
		}
		res.Actions = append(res.Actions, "started "+service.FirewallUnit)
	}
	if !st.TunEnabled() {
		return res, nil
	}

	status, err := i.systemd().Status(ctx, service.CoreUnit)
	if err != nil || !status.Active() {
		return res, err
	}
	tables, err := i.rules().Tables(ctx)
	if err != nil {
		return res, err
	}
	on, err := mihomo.NewClient(i.Layout.CoreSocket()).TunEnabled(ctx)
	if err != nil || !on || tables[netfilter.KernelTable] {
		return res, nil
	}
	if _, err := i.kernels(nil, mihomo.Arch{}).Restart(ctx); err != nil {
		return res, fmt.Errorf("mihomo's nftables table is gone; restarting mihomo failed: %w", err)
	}
	res.Actions = append(res.Actions, "restarted mihomo: its nftables table inet "+netfilter.KernelTable+" was gone")
	return res, nil
}

func (i *Installer) rules() *netfilter.Rules {
	if i.Rules != nil {
		return i.Rules
	}
	return &netfilter.Rules{}
}

// coreUID 返回内核用户的 UID，不知道（尚未 init、测试用的布局）时为 -1。
func (i *Installer) coreUID() int {
	if o, err := service.KernelUser(i.Layout); err == nil && o != nil {
		return o.UID
	}
	return -1
}

func (i *Installer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func ignoreNotExist(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
