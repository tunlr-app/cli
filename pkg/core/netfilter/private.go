package netfilter

import (
	"net/netip"
	"slices"
	"strings"
)

// PrivateIPv4 和 PrivateIPv6 是不会从公网直接到达的地址段：RFC 1918 私有地址、RFC 6598 共享地址空间
// （运营商级 NAT 的内侧，部分 VPN 也用它）、RFC 3927 链路本地；IPv6 的唯一本地地址（RFC 4193）和链路本地。
// tunlr 用它们在局域网与公网之间划界，对局域网开放的功能都以此为准：代理端口和面板的访问限制（inet tunlr_access
// 与 lan-allowed-ips）、
// 网关断开时放行的目标、设备记录的来源。这是唯一的定义，nft 模板经模板变量 Private、Private6 取用。
var (
	PrivateIPv4 = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"}
	PrivateIPv6 = []string{"fc00::/7", "fe80::/10"}
)

// 回环地址段。内核按 lan-allowed-ips 检查来源时不放过本机，要另外放行。
const (
	LoopbackIPv4 = "127.0.0.0/8"
	LoopbackIPv6 = "::1/128"
)

var privatePrefixes = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range slices.Concat(PrivateIPv4, PrivateIPv6) {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return ps
}()

// PrivateAddress 报告地址 s 是否在 PrivateIPv4 或 PrivateIPv6 中（不含回环）。不是合法地址时为 false。
func PrivateAddress(s string) bool {
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return slices.ContainsFunc(privatePrefixes, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// privateVars 把地址段加入 nft 模板的变量：Private、Private6 写成集合的元素列表（逗号分隔），
// Loopback、Loopback6 是回环地址段。
func privateVars(vars map[string]string) map[string]string {
	vars["Private"] = strings.Join(PrivateIPv4, ", ")
	vars["Private6"] = strings.Join(PrivateIPv6, ", ")
	vars["Loopback"], vars["Loopback6"] = LoopbackIPv4, LoopbackIPv6
	return vars
}
