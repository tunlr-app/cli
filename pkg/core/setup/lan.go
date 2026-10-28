package setup

import (
	"context"
	"errors"
	"io/fs"
	"net"
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

// virtualPrefixes 是不作为局域网网卡候选的虚拟网卡名前缀（容器网桥、虚拟网卡、VPN）。
var virtualPrefixes = []string{"docker", "br-", "veth", "virbr", "vnet", "tailscale", "wg", "zt", "tun", "tap", "cni", "flannel", "cali", "kube"}

// LANInterfaces 列出可以作为网关模式局域网网卡的候选：已启用、有 IPv4 地址，排除回环、透明代理的
// TUN 和常见的虚拟网卡。IPv4 默认路由所在的网卡排在最前，其余按名称排序。
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
		if name == "lo" || name == netfilter.Device || name != def && slices.ContainsFunc(virtualPrefixes, func(p string) bool {
			return strings.HasPrefix(name, p)
		}) {
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
