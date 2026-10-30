package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// sysctlContent 是网关模式的 sysctl 配置，开机时由 systemd-sysctl 加载。
const sysctlContent = "# 由 tunlr 生成：网关模式需要 IPv4 转发（tunlr tun gateway off 时删除）。\nnet.ipv4.ip_forward = 1\n"

// GatewayStatus 是网关模式的状态。开关以状态文件为准，其余以实际情况为准。
type GatewayStatus struct {
	// On 是开关，Interface 是开启时的局域网网卡，Addresses 是它的 IPv4 地址（局域网设备的网关和 DNS）。
	On        bool     `json:"on"`
	Interface string   `json:"interface,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	// Tun 是透明代理的开关（off、pending、on），网关依赖它。
	Tun string `json:"tun"`
	// Forward 报告 net.ipv4.ip_forward 是否为 1；Sysctl 报告 sysctl 配置文件是否存在。
	Forward bool `json:"forward"`
	Sysctl  bool `json:"sysctl"`
	// Rules 是 nftables 表和 DOCKER-USER 规则的情况（只在开关开启时检查）。
	Rules *netfilter.GatewayState `json:"rules,omitempty"`
}

// GatewayResult 是开启网关的结果。
type GatewayResult struct {
	Interface string   `json:"interface"`
	Addresses []string `json:"addresses"`
	// Docker 报告是否在 DOCKER-USER 链中放行了转发。
	Docker bool `json:"docker"`
}

// GatewayStatus 返回网关模式的状态。
func (i *Installer) GatewayStatus(ctx context.Context) (*GatewayStatus, error) {
	s := &GatewayStatus{Tun: "off"}
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if st != nil && st.Tun != "" {
		s.Tun = st.Tun
	}
	s.Forward = i.ipForward() == "1"
	_, err = os.Stat(i.Layout.SysctlFile())
	s.Sysctl = err == nil
	if st == nil || st.Gateway == nil {
		return s, nil
	}
	s.On, s.Interface = true, st.Gateway.Interface
	if ifi, err := i.lookupInterface(s.Interface); err == nil {
		s.Addresses = ifi.IPv4
	}
	if rs, err := i.rules().Gateway(ctx, s.Interface); err == nil {
		s.Rules = &rs
	}
	return s, nil
}

// GatewayPlan 是开启网关前的检查结果，用于预览将要做的改动。
type GatewayPlan struct {
	// Interface 是要使用的局域网网卡，Addresses 是它的 IPv4 地址。
	Interface string   `json:"interface"`
	Addresses []string `json:"addresses"`
	// Forward 是 net.ipv4.ip_forward 当前的值。
	Forward string `json:"forward"`
	// Docker 报告是否存在 DOCKER-USER 链（存在时在其中放行转发）。
	Docker bool `json:"docker"`
	// Current 是网关已经开启时的状态，关闭时为 nil。
	Current *GatewayStatus `json:"current,omitempty"`
	// Warnings 是不阻止开启、但可能影响使用的情况。
	Warnings []string `json:"warnings,omitempty"`
}

// InPlace 报告网关已经按这个计划开启、各项都在，不需要再做什么。
func (p *GatewayPlan) InPlace() bool {
	c := p.Current
	return c != nil && c.On && c.Interface == p.Interface && c.Forward && c.Sysctl && c.Rules != nil && c.Rules.Complete()
}

// gatewayInterface 是开启网关要用的局域网网卡：给出的 iface；为空时沿用网关模式正在使用的网卡（再次执行
// 是为了恢复缺失的部分），没有开启时取 IPv4 默认路由所在的网卡。
func (i *Installer) gatewayInterface(ctx context.Context, iface string, s *GatewayStatus) (string, error) {
	switch {
	case iface != "":
		return iface, nil
	case s.On:
		return s.Interface, nil
	}
	def, err := i.rules().DefaultInterface(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %w", core.ErrCheckFailed, err)
	}
	return def, nil
}

// GatewayPreflight 检查能否开启网关，包括透明代理的前置检查（TunPreflight）。iface 为空时的取法见
// gatewayInterface。不满足时返回满足 errors.Is(err, core.ErrCheckFailed) 的错误；网卡名不合法时满足
// errors.Is(err, core.ErrInvalidArgument)。透明代理是否开启不在这里检查。
func (i *Installer) GatewayPreflight(ctx context.Context, iface string) (*GatewayPlan, error) {
	warnings, err := i.TunPreflight(ctx)
	if err != nil {
		return nil, err
	}
	s, err := i.GatewayStatus(ctx)
	if err != nil {
		return nil, err
	}
	if iface, err = i.gatewayInterface(ctx, iface, s); err != nil {
		return nil, err
	}
	if err := netfilter.ValidInterface(iface); err != nil {
		return nil, fmt.Errorf("%w: %w", core.ErrInvalidArgument, err)
	}
	if iface == "lo" || iface == netfilter.Device {
		return nil, fmt.Errorf("%w: %s cannot be the LAN interface", core.ErrInvalidArgument, iface)
	}
	ifi, err := i.lookupInterface(iface)
	if err != nil {
		return nil, fmt.Errorf("%w: the interface %s: %w", core.ErrCheckFailed, iface, err)
	}
	if !ifi.Up {
		return nil, fmt.Errorf("%w: the interface %s is down", core.ErrCheckFailed, iface)
	}
	if len(ifi.IPv4) == 0 {
		return nil, fmt.Errorf("%w: the interface %s has no IPv4 address", core.ErrCheckFailed, iface)
	}
	if i.strictRPFilter(iface) {
		warnings = append(warnings, fmt.Sprintf("rp_filter is strict (1) on %s; replies from TUN may be dropped (not tested yet); "+
			"if LAN devices cannot connect, set net.ipv4.conf.all.rp_filter and net.ipv4.conf.%s.rp_filter to 2", iface, iface))
	}
	p := &GatewayPlan{Interface: iface, Addresses: ifi.IPv4, Forward: i.ipForward(), Warnings: warnings}
	rs, err := i.rules().Gateway(ctx, iface)
	if err != nil {
		return nil, err
	}
	p.Docker = rs.DockerChain
	if s.On {
		p.Current = s
	}
	return p, nil
}

// GatewayOn 开启网关模式（已经通过 GatewayPreflight，并经用户确认）：加锁 → 记下 ip_forward 的值 →
// 写 sysctl 文件并开启转发 → 加载 nft 表和 DOCKER-USER 规则 → 开关记为开启。透明代理须已开启（on）。
// 已经开启时按 iface 重新应用。任何一步失败都撤销已做的步骤，返回满足 errors.Is(err, core.ErrRolledBack) 的错误。
func (i *Installer) GatewayOn(ctx context.Context, iface string) (*GatewayResult, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	st, err := state.Load(i.Layout.StateFile())
	if err != nil {
		return nil, err
	}
	if st.Tun != state.TunOn {
		return nil, fmt.Errorf("%w: the gateway needs TUN; turn it on first: tunlr tun on", core.ErrCheckFailed)
	}
	if st.Gateway != nil && st.Gateway.Interface != iface {
		// 换网卡：先撤掉旧网卡的规则，保留最初记下的 ip_forward。
		if err := i.rules().GatewayDown(ctx, st.Gateway.Interface); err != nil {
			return nil, err
		}
	}
	gw := &state.Gateway{Interface: iface, ForwardBefore: i.ipForward(), Since: i.now().UTC()}
	if st.Gateway != nil {
		gw.ForwardBefore, gw.Since = st.Gateway.ForwardBefore, st.Gateway.Since
	}

	err = i.gatewayUp(ctx, iface)
	if err == nil {
		st.Gateway = gw
		err = state.Save(i.Layout.StateFile(), st)
	}
	if err != nil {
		// 撤销后网关是关闭的（换网卡时旧网卡的规则已经撤掉），开关也记为关闭。
		st.Gateway = gw
		uerr := i.gatewayDown(ctx, st)
		st.Gateway = nil
		if uerr = errors.Join(uerr, state.Save(i.Layout.StateFile(), st)); uerr != nil {
			return nil, fmt.Errorf("turning the gateway on failed (%w), and undoing it failed: %v", err, uerr)
		}
		return nil, fmt.Errorf("%w: turning the gateway on failed, the changes are undone: %w", core.ErrRolledBack, err)
	}
	// 网关的局域网网卡总是放行到代理端口和面板。失败时 _check 会再补；内核的 lan-allowed-ips 照样只放行私有地址。
	if err := i.syncAccess(ctx, st, nil); err != nil {
		i.log().Warn("update the nftables table for LAN access", "err", err)
	}
	// 设备列表是附带的：记录设备的表加载失败不影响网关，_prestart 和 _check 会再补。
	if err := i.syncDevices(ctx, st); err != nil {
		i.log().Warn("load the nftables table that records LAN devices", "err", err)
	}
	res := &GatewayResult{Interface: iface}
	if ifi, err := i.lookupInterface(iface); err == nil {
		res.Addresses = ifi.IPv4
	}
	if rs, err := i.rules().Gateway(ctx, iface); err == nil {
		res.Docker = rs.DockerChain
	}
	i.log().Debug("gateway on", "interface", iface)
	return res, nil
}

// GatewayOff 关闭网关模式，透明代理保持开启。已经关闭时什么都不做，on 为 false。
func (i *Installer) GatewayOff(ctx context.Context) (on bool, err error) {
	l, err := i.lock(ctx)
	if err != nil {
		return false, err
	}
	defer l.Release()
	st, err := state.Load(i.Layout.StateFile())
	if err != nil || st.Gateway == nil {
		return false, ignoreNotExist(err)
	}
	if err := i.gatewayOffLocked(ctx, st); err != nil {
		return true, err
	}
	return true, nil
}

// gatewayOffLocked 在持锁时关闭网关：撤掉规则，开关记为关闭并保存。st.Gateway 为 nil 时什么都不做。
func (i *Installer) gatewayOffLocked(ctx context.Context, st *state.State) error {
	if st.Gateway == nil {
		return nil
	}
	if err := i.gatewayDown(ctx, st); err != nil {
		return fmt.Errorf("turning the gateway off: %w", err)
	}
	iface := st.Gateway.Interface
	st.Gateway = nil
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		return err
	}
	if err := i.syncAccess(ctx, st, nil); err != nil {
		i.log().Warn("update the nftables table for LAN access", "err", err)
	}
	if err := i.syncDevices(ctx, st); err != nil {
		i.log().Warn("update the nftables table that records LAN devices", "err", err)
	}
	i.log().Debug("gateway off", "interface", iface)
	return nil
}

// gatewayUp 写 sysctl 文件、开启转发、加载规则。可以重复执行。
func (i *Installer) gatewayUp(ctx context.Context, iface string) error {
	if err := os.MkdirAll(filepath.Dir(i.Layout.SysctlFile()), 0o755); err != nil {
		return err
	}
	if err := fsutil.WriteFile(i.Layout.SysctlFile(), []byte(sysctlContent), 0o644); err != nil {
		return err
	}
	if err := i.setIPForward("1"); err != nil {
		return err
	}
	return i.rules().GatewayUp(ctx, iface)
}

// gatewayDown 撤掉 st.Gateway 的规则和 sysctl 文件；开启前转发是关闭的、且没有 Docker（它需要转发）时
// 恢复为关闭。不修改 st。
func (i *Installer) gatewayDown(ctx context.Context, st *state.State) error {
	gw := st.Gateway
	rs, _ := i.rules().Gateway(ctx, gw.Interface)
	errs := []error{i.rules().GatewayDown(ctx, gw.Interface)}
	if err := os.Remove(i.Layout.SysctlFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	if gw.ForwardBefore == "0" && !rs.DockerChain {
		errs = append(errs, i.setIPForward("0"))
	}
	return errors.Join(errs...)
}

// repairGateway 在网关开启时补上缺少的转发设置和规则（_prestart、_check），返回采取的动作。
func (i *Installer) repairGateway(ctx context.Context, st *state.State) ([]string, error) {
	if st == nil || st.Gateway == nil {
		return nil, nil
	}
	var actions []string
	iface := st.Gateway.Interface
	if i.ipForward() != "1" {
		if err := i.setIPForward("1"); err != nil {
			return actions, err
		}
		actions = append(actions, "turned IPv4 forwarding on again")
	}
	if _, err := os.Stat(i.Layout.SysctlFile()); errors.Is(err, fs.ErrNotExist) {
		if err := fsutil.WriteFile(i.Layout.SysctlFile(), []byte(sysctlContent), 0o644); err != nil {
			return actions, err
		}
		actions = append(actions, "restored "+i.Layout.SysctlFile())
	}
	rs, err := i.rules().Gateway(ctx, iface)
	if err != nil {
		return actions, err
	}
	if !rs.Complete() {
		if err := i.rules().GatewayUp(ctx, iface); err != nil {
			return actions, err
		}
		actions = append(actions, "reloaded the gateway rules for "+iface)
	}
	return actions, nil
}

func (i *Installer) ipForward() string {
	data, err := os.ReadFile(i.Layout.IPForward())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (i *Installer) setIPForward(v string) error {
	// procfs 中的文件不能用临时文件加改名的方式替换。
	return os.WriteFile(i.Layout.IPForward(), []byte(v+"\n"), 0o644)
}

// strictRPFilter 报告 iface 上实际生效的 rp_filter（all 与网卡二者中的较大值）是否为严格模式。
func (i *Installer) strictRPFilter(iface string) bool {
	v := 0
	for _, name := range []string{"all", iface} {
		data, err := os.ReadFile(i.Layout.RPFilter(name))
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			v = max(v, n)
		}
	}
	return v == 1
}

func (i *Installer) lookupInterface(name string) (*netfilter.Interface, error) {
	if i.Interfaces != nil {
		return i.Interfaces(name)
	}
	return netfilter.LookupInterface(name)
}
