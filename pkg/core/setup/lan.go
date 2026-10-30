package setup

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// LANInterface 是可以作为网关模式局域网网卡的候选。
type LANInterface struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses"`
	// PublicAddresses 是其中不属于私有地址段的地址：这块网卡很可能直接连着公网。
	PublicAddresses []string `json:"public_addresses,omitempty"`
	// Default 报告 IPv4 默认路由是否在这块网卡上。
	Default bool `json:"default,omitempty"`
	// 以下只在 LANAddresses 中：Gateway 报告它是网关模式的局域网网卡，SSH 报告当前的 SSH 会话从它进来，
	// VPN 报告它是点对点的网卡（WireGuard、Tailscale 等），Allowed 报告代理端口和面板放行从它进来的连接
	// （开关关闭时为开启后放行，见 LANAccess）。
	Gateway bool `json:"gateway,omitempty"`
	SSH     bool `json:"ssh,omitempty"`
	VPN     bool `json:"vpn,omitempty"`
	Allowed bool `json:"allowed,omitempty"`
}

func newLANInterface(ifi *netfilter.Interface) LANInterface {
	c := LANInterface{Name: ifi.Name, Addresses: ifi.IPv4}
	for _, a := range ifi.IPv4 {
		if !privateAddress(a) {
			c.PublicAddresses = append(c.PublicAddresses, a)
		}
	}
	return c
}

// physicalInterface 报告网卡 name 是否接着真实的网络：它自己是硬件（或虚拟机的）网卡，即 sysfs 中有
// device；或者它是叠在这样的网卡之上的网桥、bond、VLAN、macvlan（lower_<网卡> 指向下层，逐层查找）。
// 回环、容器网桥（成员只有 veth）、veth、tun/tap、WireGuard 等 VPN 都没有 device，不算。
// 按内核给出的属性判断，不按网卡名猜：名字由各个软件自己取，列不全也可能撞名。
func (i *Installer) physicalInterface(name string) bool {
	return physicalIn(i.Layout.NetDevices(), name, 0)
}

// maxNetDepth 限制逐层查找下层网卡的深度，网桥上的 bond 上的 VLAN 这类叠法也不会超过它。
const maxNetDepth = 4

func physicalIn(dir, name string, depth int) bool {
	if netfilter.ValidInterface(name) != nil {
		return false
	}
	if _, err := os.Lstat(filepath.Join(dir, name, "device")); err == nil {
		return true
	}
	if depth >= maxNetDepth {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if lower, ok := strings.CutPrefix(e.Name(), "lower_"); ok && physicalIn(dir, lower, depth+1) {
			return true
		}
	}
	return false
}

// LANInterfaces 列出可以作为网关模式局域网网卡的候选：已启用、有 IPv4 地址、接着真实网络的网卡
// （见 physicalInterface）。IPv4 默认路由所在的网卡总是候选，排在最前，其余按名称排序。
func (i *Installer) LANInterfaces(ctx context.Context) ([]LANInterface, error) {
	names, err := i.interfaceNames()
	if err != nil {
		return nil, err
	}
	def, _ := i.rules().DefaultInterface(ctx)
	var res []LANInterface
	for _, name := range names {
		if name == netfilter.Device || name != def && !i.physicalInterface(name) {
			continue
		}
		ifi, err := i.lookupInterface(name)
		if err != nil || !ifi.Up || len(ifi.IPv4) == 0 {
			continue
		}
		c := newLANInterface(ifi)
		c.Default = name == def
		res = append(res, c)
	}
	sortLANInterfaces(res)
	return res, nil
}

// LANAddresses 列出其他设备访问本机的代理端口和面板时可用的网卡及其 IPv4 地址：接着真实网络的网卡
// （见 physicalInterface）、点对点的网卡（WireGuard、Tailscale 等 VPN，对端也在私有地址段中，同样放行），
// 以及当前 SSH 会话进来的网卡（sshConnection 为 $SSH_CONNECTION，可为空）。容器网桥、veth 不列：
// 容器经网桥访问本机，用不着这里的地址（放行容器另有选项，见 state.LANAccess）。网关模式的局域网网卡排最前，
// 其次是默认路由所在的网卡，其余按名称排序。列不出网卡时为空。
func (i *Installer) LANAddresses(ctx context.Context, sshConnection string) []LANInterface {
	st, err := state.Load(i.Layout.StateFile())
	if err != nil {
		st = nil
	}
	return i.lanAddresses(ctx, st, sshConnection)
}

func (i *Installer) lanAddresses(ctx context.Context, st *state.State, sshConnection string) []LANInterface {
	names, err := i.interfaceNames()
	if err != nil {
		return nil
	}
	def, _ := i.rules().DefaultInterface(ctx)
	gw := ""
	if st != nil && st.Gateway != nil {
		gw = st.Gateway.Interface
	}
	serverIP, _ := sshServer(sshConnection)
	if isLoopbackIP(serverIP) {
		serverIP = ""
	}
	var res []LANInterface
	for _, name := range names {
		if name == netfilter.Device {
			continue
		}
		ifi, err := i.lookupInterface(name)
		if err != nil || !ifi.Up || len(ifi.IPv4) == 0 {
			continue
		}
		c := newLANInterface(ifi)
		c.Default, c.Gateway, c.VPN = name == def, name == gw, ifi.PointToPoint
		c.SSH = serverIP != "" && slices.Contains(ifi.IPv4, serverIP)
		if c.Default || c.Gateway || c.SSH || c.VPN || i.physicalInterface(name) {
			res = append(res, c)
		}
	}
	sortLANInterfaces(res)
	chosen := defaultInterfaces(res)
	if st != nil && st.LANAccess != nil {
		chosen = st.LANAccess.Interfaces
	}
	for k := range res {
		res[k].Allowed = res[k].Gateway || slices.Contains(chosen, res[k].Name)
	}
	return res
}

// defaultInterfaces 是还没有选过时放行的网卡：没有公网地址的网卡，VPN 只取当前 SSH 会话进来的那块
// （经它登录说明它连着自己的设备；其他 VPN 可能连着机场或商业 VPN）。
func defaultInterfaces(cands []LANInterface) []string {
	var names []string
	for _, c := range cands {
		if len(c.PublicAddresses) == 0 && (!c.VPN || c.SSH) {
			names = append(names, c.Name)
		}
	}
	return names
}

// sortLANInterfaces 把网关模式的局域网网卡排最前，其次是默认路由所在的网卡，其余按名称排序。
func sortLANInterfaces(list []LANInterface) {
	rank := func(c LANInterface) int {
		switch {
		case c.Gateway:
			return 0
		case c.Default:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(list, func(a, b LANInterface) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), strings.Compare(a.Name, b.Name))
	})
}

func (i *Installer) interfaceNames() ([]string, error) {
	if i.InterfaceNames != nil {
		return i.InterfaceNames()
	}
	return systemInterfaceNames()
}

func systemInterfaceNames() ([]string, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(ifs))
	for i, ifi := range ifs {
		names[i] = ifi.Name
	}
	return names, nil
}

// LANConfigured 报告透明代理、网关模式或代理端口的局域网访问是否已经开启（或正在开启）。
// init 据此决定是否还要询问这台服务器的用途：已经设置过的不再问。
func (i *Installer) LANConfigured() (bool, error) {
	st, err := state.Load(i.Layout.StateFile())
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return st.Tun != "" || st.Gateway != nil || st.ProxyLAN != nil, nil
}
