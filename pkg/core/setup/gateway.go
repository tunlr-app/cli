package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	// Recent 是最近 5 分钟（netfilter.GatewayWindow）用过网关的局域网设备（IPv4 地址，表在时才有），
	// Blocks 是其中被其他防火墙丢弃的流量。
	Recent []string       `json:"recent,omitempty"`
	Blocks []GatewayBlock `json:"blocks,omitempty"`
}

// recentText 是 GatewayStatus.Recent 所指的时段，用于提示。
var recentText = fmt.Sprintf("the last %d minutes", int(netfilter.GatewayWindow.Minutes()))

// GatewayBlock 是被其他防火墙丢弃的一类流量：最近 5 分钟有局域网设备的这类新连接，却一个都没有通过
// （由 inet tunlr_gateway 记下的新连接得出，DESIGN.md 8.10）。
type GatewayBlock struct {
	// Class 是流量的名称：DNS、TCP、UDP、ping，其余为协议名。
	Class string `json:"class"`
	// Path 是 netfilter.PathDNS 等，决定它经过转发还是入站。
	Path string `json:"path"`
	// Devices 是这类流量被丢弃的设备（IPv4 地址）。
	Devices []string `json:"devices"`
	// Suspects 是挂在同一处、可能丢弃它的规则链；为空时可能是其他规则或 iptables-legacy 的规则。
	Suspects []netfilter.Blocker `json:"suspects,omitempty"`
	// Fixes 是放行的方法，每条一句。
	Fixes []string `json:"fixes,omitempty"`
}

// Describe 用一句话说明，如 “DNS from 192.168.1.20 and 192.168.1.21 is dropped by inet firewalld
// filter_FORWARD (firewalld)”。
func (b GatewayBlock) Describe() string {
	by := "another firewall"
	if len(b.Suspects) > 0 {
		by = strings.Join(netfilter.BlockerNames(b.Suspects), ", ")
	}
	devices := b.Devices
	more := ""
	if len(devices) > 3 {
		devices, more = devices[:3], fmt.Sprintf(" and %d more", len(devices)-3)
	} else if len(devices) > 1 {
		devices, more = devices[:len(devices)-1], " and "+devices[len(devices)-1]
	}
	return fmt.Sprintf("%s from %s%s is dropped by %s", b.Class, strings.Join(devices, ", "), more, by)
}

// GatewayFixes 返回放行 blocks 的方法，同样的只列一次。
func GatewayFixes(blocks []GatewayBlock) []string {
	var fixes []string
	for _, b := range blocks {
		for _, f := range b.Fixes {
			if !slices.Contains(fixes, f) {
				fixes = append(fixes, f)
			}
		}
	}
	return fixes
}

// trafficClass 是一类新连接的名称（GatewayBlock.Class）：被劫持的 DNS 按协议分开（设备解析先用 UDP，超时
// 不会改用 TCP，UDP 被丢就是解析失败），转发的 ICMP 算作 ping，nft 不认识的协议是协议号。
func trafficClass(u netfilter.GatewayUse) string {
	proto := strings.ToUpper(u.Protocol)
	if _, err := strconv.Atoi(u.Protocol); err == nil {
		proto = "protocol " + u.Protocol
	}
	switch {
	case u.Path == netfilter.PathDNS && u.Protocol == "udp":
		return "DNS"
	case u.Path == netfilter.PathDNS:
		return "DNS over " + proto
	case u.Protocol == "icmp":
		return "ping"
	}
	return proto
}

// recentDevices 返回 uses 中的设备，去掉重复的（uses 已按设备排序）。
func recentDevices(uses []netfilter.GatewayUse) []string {
	var devices []string
	for _, u := range uses {
		if len(devices) == 0 || devices[len(devices)-1] != u.Device {
			devices = append(devices, u.Device)
		}
	}
	return devices
}

// gatewayBlocks 找出被其他防火墙丢弃的流量：某台设备的一类新连接一个都没有通过。按路与类别归并设备，并列出
// 可疑的规则链和放行的方法。dockerHandled 报告 DOCKER-USER 中的放行规则都在（见 suspects）。
func (i *Installer) gatewayBlocks(ctx context.Context, iface string, uses []netfilter.GatewayUse, dockerHandled bool) []GatewayBlock {
	type key struct{ device, path, class string }
	var keys []key
	passed := map[key]bool{}
	for _, u := range uses {
		k := key{u.Device, u.Path, trafficClass(u)}
		if _, ok := passed[k]; !ok {
			keys = append(keys, k)
		}
		passed[k] = passed[k] || u.Passed
	}
	var blocks []GatewayBlock
	for _, k := range keys {
		if passed[k] {
			continue
		}
		n := slices.IndexFunc(blocks, func(b GatewayBlock) bool { return b.Path == k.path && b.Class == k.class })
		if n < 0 {
			blocks = append(blocks, GatewayBlock{Class: k.class, Path: k.path})
			n = len(blocks) - 1
		}
		blocks[n].Devices = append(blocks[n].Devices, k.device)
	}
	if len(blocks) == 0 {
		return nil
	}
	slices.SortStableFunc(blocks, func(a, b GatewayBlock) int {
		return strings.Compare(a.Path+" "+a.Class, b.Path+" "+b.Class)
	})
	blockers, err := i.rules().Blockers(ctx)
	if err != nil {
		i.log().Debug("list the chains that drop by default", "err", err)
	}
	for n, b := range blocks {
		blocks[n].Suspects = netfilter.Suspects(blockers, b.Path, dockerHandled)
		blocks[n].Fixes = netfilter.Fixes(iface, b.Path, blocks[n].Suspects)
	}
	return blocks
}

// blockerWarnings 是开启网关前的提示：挂在转发、入站上默认丢弃的规则链可能挡住局域网设备（DOCKER-USER
// 能处理的、先放行 DNAT 连接的见 netfilter.Suspects）。挡没挡住要等设备用上网关后由 doctor 判断。
func blockerWarnings(iface string, blockers []netfilter.Blocker) []string {
	var warnings []string
	for _, path := range []string{netfilter.PathForward, netfilter.PathInput} {
		list := netfilter.Suspects(blockers, path, true)
		if len(list) == 0 {
			continue
		}
		names := netfilter.BlockerNames(list)
		kind, classes := "incoming", "TCP"
		if path == netfilter.PathForward {
			// 被劫持的 DNS 也是转发的，先放行 DNAT 连接的链不会丢弃它。
			kind, classes = "forwarded", "UDP and ping"
			if slices.ContainsFunc(list, func(b netfilter.Blocker) bool { return !b.AcceptsDNAT }) {
				classes = "DNS, UDP and ping"
			}
		}
		warnings = append(warnings, fmt.Sprintf("%s drops %s traffic by default, which may block LAN devices' %s; "+
			"to let it through: %s. Once LAN devices use the gateway, tunlr doctor tells whether anything is dropped",
			strings.Join(names, ", "), kind, classes, strings.Join(netfilter.Fixes(iface, path, list), "; ")))
	}
	return warnings
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
		if rs.Table {
			if uses, err := i.rules().GatewayUses(ctx); err == nil {
				s.Recent = recentDevices(uses)
				s.Blocks = i.gatewayBlocks(ctx, s.Interface, uses, rs.DockerRules)
			}
		}
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
	// 默认丢弃入站的防火墙由下文的 blockerWarnings 按局域网网卡提示，不要 tun 组的通用版。
	warnings, err := i.tunPreflight(ctx, "tun.firewall")
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
	p := &GatewayPlan{Interface: iface, Addresses: ifi.IPv4, Forward: i.ipForward(), Warnings: warnings}
	rs, err := i.rules().Gateway(ctx, iface)
	if err != nil {
		return nil, err
	}
	p.Docker = rs.DockerChain
	if blockers, err := i.rules().Blockers(ctx); err == nil {
		p.Warnings = append(p.Warnings, blockerWarnings(iface, blockers)...)
	}
	if len(ifi.IPv6) > 0 {
		p.Warnings = append(p.Warnings, "the LAN on "+iface+" has IPv6, which the gateway does not forward: devices may ask the router's "+
			"IPv6 DNS server and connect over IPv6 directly. Turn IPv6 off on the router, or at least the IPv6 DNS it hands out (RDNSS, DHCPv6)")
	}
	if w := i.publishedGatewayIPv6Warning(); w != "" {
		p.Warnings = append(p.Warnings, w+". tunlr ipv6 off turns the IPv6 answers off")
	}
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

func (i *Installer) lookupInterface(name string) (*netfilter.Interface, error) {
	if i.Interfaces != nil {
		return i.Interfaces(name)
	}
	return netfilter.LookupInterface(name)
}
