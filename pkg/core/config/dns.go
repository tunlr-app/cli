package config

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
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

// PlainDNS 是一台以明文直连询问的上游 DNS 服务器。Key 是它所在的设置：dns.nameserver、dns.fallback，或
// nameserver-policy 中的一项，如 dns.nameserver-policy["+.google.com"]。
type PlainDNS struct {
	Key    string `json:"key"`
	Server string `json:"server"`
}

// DNSLeaks 找出 tunlr 生成的配置 cfg 中会以明文、不经代理询问的上游 DNS 服务器：运营商能看到、改写问了哪些
// 域名（DNS 泄露）。off 报告没有开启内核的 DNS，这时内核经系统的解析设置直连询问。
//
// 只看决定一般域名问谁的 nameserver、fallback 和 nameserver-policy：只含中国、局域网域名的 policy 项（见
// localPolicy），以及只解析节点、直连域名的 proxy-server-nameserver、default-nameserver、direct-nameserver 不算。
// 明文是不写协议、udp://、tcp://、dhcp:// 和 system。经代理是指服务器带 #节点名（#DIRECT 为直连），或开启了
// respect-rules（system、dhcp 总是直连）；respect-rules 下规则仍可能让它直连，这里不再解析规则。
func DNSLeaks(cfg []byte) (plain []PlainDNS, off bool) {
	d := dnsSection(cfg)
	if !d.Enable {
		return nil, true
	}
	add := func(key string, n yaml.Node) {
		for _, s := range serverList(n) {
			if plainDirect(s, d.RespectRules) {
				plain = append(plain, PlainDNS{Key: key, Server: s})
			}
		}
	}
	add("dns.nameserver", d.Nameserver)
	add("dns.fallback", d.Fallback)
	for _, k := range slices.Sorted(maps.Keys(d.Policy)) {
		if !localPolicy(k) {
			add(fmt.Sprintf("dns.nameserver-policy[%q]", k), d.Policy[k])
		}
	}
	return plain, false
}

// HasDNS 报告配置 cfg 是否写了顶层的 dns（订阅没写时，采用订阅的 DNS 也用 tunlr 的兜底值）。
func HasDNS(cfg []byte) bool {
	for line := range bytes.Lines(cfg) {
		if bytes.HasPrefix(line, []byte("dns:")) {
			return true
		}
	}
	return false
}

// DNSLeakText 用一句话说明 DNSLeaks 的结果，没有泄露时为空。
func DNSLeakText(plain []PlainDNS, off bool) string {
	if off {
		return "the DNS of mihomo is off (dns.enable): it looks names up through the system's resolver, asked directly in plain text"
	}
	var keys []string
	servers := map[string][]string{}
	for _, p := range plain {
		if _, ok := servers[p.Key]; !ok {
			keys = append(keys, p.Key)
		}
		servers[p.Key] = append(servers[p.Key], p.Server)
	}
	var parts []string
	for _, k := range keys {
		s := servers[k]
		noun := "server"
		if len(s) > 1 {
			noun = "servers"
		}
		parts = append(parts, fmt.Sprintf("%s has %d plain %s asked directly (%s)", k, len(s), noun, strings.Join(s, ", ")))
	}
	return strings.Join(parts, "; ")
}

// plainDirect 报告上游服务器 s 是否以明文直连询问，respectRules 是 dns.respect-rules。
func plainDirect(s string, respectRules bool) bool {
	addr, frag, _ := strings.Cut(s, "#")
	scheme, _, ok := strings.Cut(addr, "://")
	if !ok {
		scheme = ""
		if addr == "system" {
			scheme = "system"
		}
	}
	switch strings.ToLower(scheme) {
	case "system", "dhcp":
		return true
	case "", "udp", "tcp":
	default:
		return false
	}
	// # 后面是节点名，再用 & 连接参数；只写参数时（如 #ecs=...）没有节点名。
	proxy, _, _ := strings.Cut(frag, "&")
	if strings.Contains(proxy, "=") {
		proxy = ""
	}
	if proxy != "" {
		return proxy == "DIRECT"
	}
	return !respectRules
}

// localSuffixes 是只在局域网中、或属于中国的域名后缀。
var localSuffixes = []string{"cn", "lan", "local", "localdomain", "localhost", "internal", "arpa"}

// localPolicy 报告 nameserver-policy 的键 key 是否只含中国或局域网的域名（问中国的服务器、路由器不算泄露）。
// 键可以用逗号连接多项；geosite: 后用逗号连接的是多个分类，与 mihomo 的解读相同。
func localPolicy(key string) bool {
	prefix := ""
	for p := range strings.SplitSeq(strings.ToLower(key), ",") {
		p = strings.TrimSpace(p)
		for _, pre := range []string{"geosite:", "rule-set:"} {
			if rest, ok := strings.CutPrefix(p, pre); ok {
				prefix, p = pre, rest
			}
		}
		if prefix != "" {
			if p != "cn" && p != "private" && !strings.HasSuffix(p, "-cn") && !strings.HasSuffix(p, "@cn") && !strings.HasSuffix(p, "_cn") {
				return false
			}
			continue
		}
		name := strings.TrimLeft(p, "+*.")
		if !slices.ContainsFunc(localSuffixes, func(s string) bool { return name == s || strings.HasSuffix(name, "."+s) }) {
			return false
		}
	}
	return true
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
	Fallback     yaml.Node            `yaml:"fallback"`
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
