package config

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// dnsFacts 是配置中 DNS 的开关和用到的全部上游服务器。
type dnsFacts struct {
	enable      bool
	nameservers bool
	servers     []string
}

// dnsServerLists 是 dns 下列出上游服务器的键（nameserver-policy 另外处理）。
var dnsServerLists = []string{"nameserver", "fallback", "default-nameserver", "proxy-server-nameserver", "direct-nameserver"}

// dnsOf 读出 dns 映射中的开关和上游服务器。
func dnsOf(v *yaml.Node) dnsFacts {
	var d dnsFacts
	if v.Kind != yaml.MappingNode {
		return d
	}
	add := func(n *yaml.Node) {
		var list []string
		if n.Decode(&list) == nil {
			d.servers = append(d.servers, list...)
			return
		}
		var s string
		if n.Decode(&s) == nil {
			d.servers = append(d.servers, s)
		}
	}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, val := v.Content[i].Value, v.Content[i+1]
		switch {
		case k == "enable":
			d.enable = isTrue(val)
		case k == "nameserver-policy" && val.Kind == yaml.MappingNode:
			for j := 1; j < len(val.Content); j += 2 {
				add(val.Content[j])
			}
		default:
			for _, l := range dnsServerLists {
				if k == l {
					add(val)
				}
			}
			if k == "nameserver" && len(val.Content) > 0 {
				d.nameservers = true
			}
		}
	}
	return d
}

// DNSWarnings 检查配置中的 DNS 在这台服务器上能否工作，返回不阻止发布的警告。订阅的 DNS 是为桌面
// 客户端写的，常有在服务器上不成立的写法，例如 dhcp://en0（macOS 的网卡）。hasInterface 报告本机
// 是否有某个网卡。DNS 没有开启时不检查。
func DNSWarnings(f *Facts, hasInterface func(name string) bool) []Warning {
	d := f.dns
	if !d.enable {
		return nil
	}
	var ws []Warning
	if !d.nameservers {
		ws = append(ws, Warning{Message: "dns.nameserver is empty: mihomo cannot resolve names"})
	}
	seen := map[string]bool{}
	for _, s := range d.servers {
		iface, ok := strings.CutPrefix(s, "dhcp://")
		if i := strings.IndexAny(iface, "?#"); i >= 0 {
			iface = iface[:i]
		}
		if !ok || iface == "system" || seen[iface] {
			continue
		}
		seen[iface] = true
		if !hasInterface(iface) {
			ws = append(ws, Warning{Message: fmt.Sprintf("DNS server %s: this server has no network interface %s (use a mixin to change the DNS servers)", s, iface)})
		}
	}
	return ws
}
