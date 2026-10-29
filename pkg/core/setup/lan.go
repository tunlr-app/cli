package setup

import (
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
	list := i.InterfaceNames
	if list == nil {
		list = interfaceNames
	}
	names, err := list()
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
		c := LANInterface{Name: name, Addresses: ifi.IPv4, Default: name == def}
		for _, a := range ifi.IPv4 {
			if !privateAddress(a) {
				c.PublicAddresses = append(c.PublicAddresses, a)
			}
		}
		res = append(res, c)
	}
	slices.SortStableFunc(res, func(a, b LANInterface) int {
		switch {
		case a.Default == b.Default:
			return strings.Compare(a.Name, b.Name)
		case a.Default:
			return -1
		default:
			return 1
		}
	})
	return res, nil
}

func interfaceNames() ([]string, error) {
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
