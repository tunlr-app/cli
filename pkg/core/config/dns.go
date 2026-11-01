package config

import (
	"bytes"
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

// DNSMode 读出 tunlr 生成的配置 cfg 中的 dns.enhanced-mode（fake-ip、redir-host），没有写或读不出时为空。
// 同 GeoSettings，只解析顶层 dns 这一段，不解析整份配置：主菜单每 10 秒刷新一次状态。
func DNSMode(cfg []byte) string {
	return dnsSection(cfg).Mode
}

// DNSEnabled 报告 tunlr 生成的配置 cfg 是否开启了内核的 DNS（dns.enable；tunlr 的兜底值开启它，TUN 开启时必须开启）。
func DNSEnabled(cfg []byte) bool {
	return dnsSection(cfg).Enable
}

// ChinaPolicy 是 nameserver-policy 中中国的域名的键，tunlr dns servers 改的是它。
const ChinaPolicy = "geosite:cn"

// DNSServers 读出 tunlr 生成的配置 cfg 中问谁：china 是 nameserver-policy 中 ChinaPolicy 的服务器，other 是
// nameserver。没有写时为空。
func DNSServers(cfg []byte) (china, other []string) {
	d := dnsSection(cfg)
	return serverList(d.Policy[ChinaPolicy]), serverList(d.Nameserver)
}

// DNSLacks 返回 tunlr 生成的配置 cfg 的 dns 中 redir-host 模式需要、却没有的设置：respect-rules（经规则问境外的
// 服务器，免得拿到污染的地址）和 nameserver-policy（中国的域名问中国的服务器）。tunlr 的兜底值两者都有，机场的
// DNS 常常没有。
func DNSLacks(cfg []byte) []string {
	d := dnsSection(cfg)
	var lacks []string
	if !d.RespectRules {
		lacks = append(lacks, "respect-rules")
	}
	if len(d.Policy) == 0 {
		lacks = append(lacks, "nameserver-policy")
	}
	return lacks
}

// serverList 读出一项服务器的设置：列表，或只写了一个时的字符串。
func serverList(n yaml.Node) []string {
	var list []string
	if n.Decode(&list) == nil {
		return list
	}
	var s string
	if n.Kind == yaml.ScalarNode && n.Decode(&s) == nil && s != "" {
		return []string{s}
	}
	return nil
}

// dnsFields 是 DNSMode、FakeIPFilter、DNSServers 读出的 dns 中的键。服务器的设置可以是列表或字符串，按节点读。
type dnsFields struct {
	Enable       bool                 `yaml:"enable"`
	Mode         string               `yaml:"enhanced-mode"`
	FakeIPFilter []string             `yaml:"fake-ip-filter"`
	Nameserver   yaml.Node            `yaml:"nameserver"`
	Policy       map[string]yaml.Node `yaml:"nameserver-policy"`
	RespectRules bool                 `yaml:"respect-rules"`
}

// dnsSection 读出 cfg 中顶层 dns 这一段，读不出时为零值。
func dnsSection(cfg []byte) dnsFields {
	var part bytes.Buffer
	keep := false
	for line := range bytes.Lines(cfg) {
		if len(line) > 0 && line[0] != ' ' && line[0] != '\n' && line[0] != '#' {
			keep = bytes.HasPrefix(line, []byte("dns:"))
		}
		if keep {
			part.Write(line)
		}
	}
	var v struct {
		DNS dnsFields `yaml:"dns"`
	}
	if yaml.Unmarshal(part.Bytes(), &v) != nil {
		return dnsFields{}
	}
	return v.DNS
}
