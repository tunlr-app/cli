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
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/lock"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// LANAccess 是代理端口和面板放行的入口（两个开关共用，见 state.LANAccess）。
type LANAccess struct {
	// Interfaces 是选定放行的网卡：候选中的按候选的顺序，其后是选过、但现在不在候选中的网卡（没有启用、
	// 没有 IPv4 地址或已不存在）。
	Interfaces []string `json:"interfaces"`
	// Gateway 是网关模式的局域网网卡，网关开启时总是放行。
	Gateway string `json:"gateway,omitempty"`
	// Containers 报告放行本机容器网桥（docker0、br-*）进来的连接。
	Containers bool `json:"containers,omitempty"`
	// Default 报告还没有选过，以上是默认值（开启时保存）。
	Default bool `json:"default,omitempty"`
}

// Same 报告保存 b 不会改变放行的入口：已经选过，且网卡（不计顺序）和容器都相同。
func (a LANAccess) Same(b *state.LANAccess) bool {
	if a.Default || b == nil || a.Containers != b.Containers || len(a.Interfaces) != len(b.Interfaces) {
		return false
	}
	for _, name := range b.Interfaces {
		if !slices.Contains(a.Interfaces, name) {
			return false
		}
	}
	return true
}

// With 返回保存 b 之后放行的入口（网关的局域网网卡不变）。
func (a LANAccess) With(b *state.LANAccess) LANAccess {
	return LANAccess{Interfaces: slices.Clone(b.Interfaces), Gateway: a.Gateway, Containers: b.Containers}
}

// lanAccess 由状态和候选网卡（lanAddresses 的结果）得出放行的入口。
func lanAccess(st *state.State, cands []LANInterface) LANAccess {
	a := LANAccess{Interfaces: []string{}}
	chosen := defaultInterfaces(cands)
	if st != nil && st.LANAccess != nil {
		chosen, a.Containers = st.LANAccess.Interfaces, st.LANAccess.Containers
	} else {
		a.Default = true
	}
	for _, c := range cands {
		if slices.Contains(chosen, c.Name) {
			a.Interfaces = append(a.Interfaces, c.Name)
		}
	}
	for _, name := range chosen {
		if !slices.Contains(a.Interfaces, name) {
			a.Interfaces = append(a.Interfaces, name)
		}
	}
	if st != nil && st.Gateway != nil {
		a.Gateway = st.Gateway.Interface
	}
	return a
}

// ResolveLANAccess 得出开启代理端口或面板的局域网访问时保存的入口。interfaces 为 nil 时沿用选过的网卡
// （没有选过时取 cands 中的默认值，见 defaultInterfaces），containers 为 nil 时同样沿用。网卡名不合法时、
// 给出的网卡这里没有（选过的除外：网卡暂时不在时照旧保留）时、结果没有任何入口（没有网卡、不放行容器、也没有
// 开启网关）时返回满足 errors.Is(err, core.ErrInvalidArgument) 的错误。
func (i *Installer) ResolveLANAccess(cands []LANInterface, interfaces []string, containers *bool) (*state.LANAccess, error) {
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	res := &state.LANAccess{}
	var stored []string
	if st != nil && st.LANAccess != nil {
		stored = st.LANAccess.Interfaces
	}
	switch {
	case interfaces != nil:
		res.Interfaces = []string{}
		for _, name := range interfaces {
			if err := netfilter.ValidInterface(name); err != nil {
				return nil, fmt.Errorf("%w: %w", core.ErrInvalidArgument, err)
			}
			if !slices.Contains(stored, name) {
				if err := i.CheckInterfaces(cands, name); err != nil {
					return nil, err
				}
			}
			if !slices.Contains(res.Interfaces, name) {
				res.Interfaces = append(res.Interfaces, name)
			}
		}
	case st != nil && st.LANAccess != nil:
		res.Interfaces = slices.Clone(st.LANAccess.Interfaces)
	default:
		res.Interfaces = defaultInterfaces(cands)
	}
	switch {
	case containers != nil:
		res.Containers = *containers
	case st != nil && st.LANAccess != nil:
		res.Containers = st.LANAccess.Containers
	}
	if len(res.Interfaces) == 0 && !res.Containers && (st == nil || st.Gateway == nil) {
		return nil, fmt.Errorf("%w: no interface would be let in (none without a public address); "+
			"choose with --interface (%s)", core.ErrInvalidArgument, candidateNames(cands))
	}
	return res, nil
}

// CheckInterfaces 检查要放行的网卡 names 的名称合法且这台机器上有（cands 中的，或 lookupInterface 找得到的，
// 如容器网桥），否则返回满足 errors.Is(err, core.ErrInvalidArgument) 的错误。init 在动手之前用它检查
// --lan-interface。
func (i *Installer) CheckInterfaces(cands []LANInterface, names ...string) error {
	for _, name := range names {
		if err := netfilter.ValidInterface(name); err != nil {
			return fmt.Errorf("%w: %w", core.ErrInvalidArgument, err)
		}
		if slices.ContainsFunc(cands, func(c LANInterface) bool { return c.Name == name }) {
			continue
		}
		if _, err := i.lookupInterface(name); err != nil {
			return fmt.Errorf("%w: there is no interface %s here (%s)", core.ErrInvalidArgument, name, candidateNames(cands))
		}
	}
	return nil
}

// candidateNames 列出可以放行的网卡，用于错误信息。
func candidateNames(cands []LANInterface) string {
	if len(cands) == 0 {
		return "this server has no interface that is up with an IPv4 address"
	}
	names := make([]string, len(cands))
	for k, c := range cands {
		names[k] = c.Name
	}
	return "interfaces here: " + strings.Join(names, ", ")
}

// SetLANAccess 保存放行的入口并按它重新加载 inet tunlr_access 表（开关开着时），不发布配置。
func (i *Installer) SetLANAccess(ctx context.Context, access *state.LANAccess) error {
	l, err := i.lock(ctx)
	if err != nil {
		return err
	}
	defer l.Release()
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return err
	}
	previous := st.LANAccess
	st.LANAccess = access
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		return err
	}
	if err := i.syncAccess(ctx, st, nil); err != nil {
		st.LANAccess = previous
		return errors.Join(err, state.Save(i.Layout.StateFile(), st), i.syncAccess(ctx, st, nil))
	}
	i.log().Debug("lan access", "interfaces", access.Interfaces, "containers", access.Containers)
	return nil
}

// proxyPortKeys 是 allow-lan 对其他设备开放的代理端口（mixin 自己写的 listeners 不在其中）。
var proxyPortKeys = []string{"mixed-port", "port", "socks-port", "redir-port", "tproxy-port"}

// accessRules 由状态得出 inet tunlr_access 的内容：代理端口的局域网访问开启时为配置 cfg（为 nil 时取生效的
// 配置）中的代理端口，面板的开启时为 API 端口；网卡为选定的和网关模式的局域网网卡。开关都关闭时 Ports 为空。
func (i *Installer) accessRules(st *state.State, cfg []byte) netfilter.Access {
	var a netfilter.Access
	if st == nil {
		return a
	}
	if st.ProxyLAN != nil {
		if cfg == nil {
			cfg, _ = i.pipeline(nil).Current()
		}
		if l, err := config.Listen(cfg); err == nil {
			for _, ln := range l.Listeners {
				if slices.Contains(proxyPortKeys, ln.Key) {
					a.Ports = append(a.Ports, ln.Port)
				}
			}
		}
	}
	if st.PanelLAN != nil {
		a.Ports = append(a.Ports, pipeline.APIPort)
	}
	if st.LANAccess != nil {
		a.Interfaces, a.Containers = slices.Clone(st.LANAccess.Interfaces), st.LANAccess.Containers
	}
	if st.Gateway != nil {
		a.Interfaces = append(a.Interfaces, st.Gateway.Interface)
	}
	return a
}

// syncAccess 让 inet tunlr_access 与状态一致（整表替换，开关都关闭时删除）。cfg 见 accessRules。
func (i *Installer) syncAccess(ctx context.Context, st *state.State, cfg []byte) error {
	return i.rules().AccessUp(ctx, i.accessRules(st, cfg))
}

// repairAccess 在代理端口或面板对其他设备开放时重新加载 inet tunlr_access（_prestart、_check、_firewall）：
// 补上不见了的表，也让端口跟上生效的配置（mixin 可能改了代理端口）。返回采取的动作，表本来就在时为空。
func (i *Installer) repairAccess(ctx context.Context, st *state.State) (string, error) {
	a := i.accessRules(st, nil)
	if len(a.Ports) == 0 {
		return "", nil
	}
	tables, err := i.rules().Tables(ctx)
	if err != nil {
		return "", err
	}
	if err := i.rules().AccessUp(ctx, a); err != nil {
		return "", err
	}
	if tables[netfilter.AccessTable] {
		return "", nil
	}
	return "reloaded the nftables table inet " + netfilter.AccessTable, nil
}

// checkWanted 报告 tunlr-check.timer 是否要启用：透明代理、网关或局域网访问开着时，它补上被清空的规则。
func checkWanted(st *state.State) bool {
	return st != nil && (st.TunEnabled() || st.Gateway != nil || st.ProxyLAN != nil || st.PanelLAN != nil)
}

// syncCheckTimer 按状态启用或停用 tunlr-check.timer。失败只记日志：开关已经生效，_check 会在下次执行时
// 停用多余的自己，SyncTimers（init）也会改正。
func (i *Installer) syncCheckTimer(ctx context.Context, st *state.State) {
	if err := i.systemd().SetTimer(ctx, service.CheckTimer, checkWanted(st)); err != nil {
		i.log().Warn("update "+service.CheckTimer, "err", err)
	}
}

// repairLockWait 是 _firewall、_check 等待 tunlr 自己的操作（如正在下载的 core upgrade）释放锁的时间。
// _firewall 等不到就失败会让 unit 进入 failed，之后 nftables.service 的重新加载不再传到它，所以不能太短。
const repairLockWait = 5 * time.Minute

// Firewall 是 tunlr-firewall.service 在 nftables.service 启动、重启或重新加载之后执行的 _firewall：
// 它加载的配置以 flush ruleset 开头，会清空 tunlr 的表，这里按状态重新加载（同 _check 中的表，但不重启
// 内核：内核的表由 _check 发现后重启内核重建）。持锁的操作不能同步启动这个 unit（会互相等待）。
func (i *Installer) Firewall(ctx context.Context) (*CheckResult, error) {
	_, actions, err := i.repairLocked(ctx)
	return &CheckResult{Actions: actions}, err
}

// repairLocked 取锁后读取状态，补上 tunlr 的表（repairTables）。_check 与 _firewall 会同时执行（例如重新
// 加载 nftables.service 时 tunlr-check.timer 恰好到期），不持锁时两边会发现缺了同一条规则、各补一遍
// （DOCKER-USER 中的放行规则就会重复）；持锁也避免与 gateway off 等操作交错，按旧状态补回刚删掉的表。
// 返回读到的状态（文件不存在时为 nil）。
func (i *Installer) repairLocked(ctx context.Context) (*state.State, []string, error) {
	lctx, cancel := context.WithTimeout(ctx, repairLockWait)
	defer cancel()
	l, err := lock.Acquire(lctx, i.Layout.LockFile())
	if err != nil {
		return nil, nil, err
	}
	defer l.Release()
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	actions, err := i.repairTables(ctx, st)
	return st, actions, err
}

// repairTables 按状态补上 tunlr 的表（_check、_firewall）：网关开启时它的转发设置和规则，代理端口和面板的
// 访问限制，记录设备的表，内核在运行时的 inet tunlr。返回采取的动作。一项失败时继续其余的。
func (i *Installer) repairTables(ctx context.Context, st *state.State) ([]string, error) {
	var actions []string
	var errs []error
	if acts, err := i.repairGateway(ctx, st); err != nil {
		errs = append(errs, fmt.Errorf("repairing the gateway: %w", err))
	} else {
		actions = append(actions, acts...)
	}
	if action, err := i.repairAccess(ctx, st); err != nil {
		errs = append(errs, fmt.Errorf("repairing the rules for LAN access: %w", err))
	} else if action != "" {
		actions = append(actions, action)
	}
	if action, err := i.repairDevices(ctx, st); err != nil {
		errs = append(errs, fmt.Errorf("repairing the table that records LAN devices: %w", err))
	} else if action != "" {
		actions = append(actions, action)
	}
	// inet tunlr 与内核服务同生共死（见 DESIGN.md 8.4），内核在运行时才补。
	if status, err := i.systemd().Status(ctx, service.CoreUnit); err == nil && status.Active() {
		tables, err := i.rules().Tables(ctx)
		switch {
		case err != nil:
			errs = append(errs, err)
		case !tables[netfilter.Table]:
			if err := i.rules().Up(ctx); err != nil {
				errs = append(errs, err)
			} else {
				actions = append(actions, "reloaded the nftables table inet "+netfilter.Table)
			}
		}
	}
	return actions, errors.Join(errs...)
}
