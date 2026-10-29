package config

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/tunlr-app/cli/pkg/core"
)

// ExposureError 报告配置会让其他主机无需凭据就能使用代理或 DNS（见 DESIGN.md 第 6 节“端口安全”），
// 满足 errors.Is(err, core.ErrCheckFailed)。
type ExposureError struct {
	Problems []string
}

func (e *ExposureError) Error() string {
	return "unsafe configuration: " + strings.Join(e.Problems, "; ")
}

func (e *ExposureError) Unwrap() error { return core.ErrCheckFailed }

// CheckExposure 检查配置是否会开放无需认证的代理端口、DNS 或无 secret 的 API，有问题时返回 *ExposureError。
func CheckExposure(cfg []byte) error {
	doc, err := parseNode(cfg)
	if err != nil || doc == nil {
		return err
	}
	root := doc.Content[0]
	str := func(path ...string) string {
		if n := lookup(root, path); n != nil && n.Kind == yaml.ScalarNode && !isNull(n) {
			return n.Value
		}
		return ""
	}
	globalAuth := nonEmptySeq(lookup(root, []string{"authentication"}))

	var problems []string
	if str("allow-lan") == "true" && !globalAuth && !privateOnly(lookup(root, []string{"lan-allowed-ips"})) {
		problems = append(problems, "allow-lan is on without authentication and lan-allowed-ips does not limit it to private addresses, "+
			"so anyone who can reach this server can use the proxy ports; use tunlr proxy lan on (private addresses only), "+
			"or add authentication in a mixin")
	}
	if ls := lookup(root, []string{"listeners"}); ls != nil && ls.Kind == yaml.SequenceNode {
		for _, l := range ls.Content {
			if l.Kind != yaml.MappingNode {
				continue
			}
			field := func(k string) string {
				if n := lookup(l, []string{k}); n != nil && n.Kind == yaml.ScalarNode && !isNull(n) {
					return n.Value
				}
				return ""
			}
			switch field("type") {
			case "mixed", "http", "socks":
			default:
				continue
			}
			if isLoopback(strings.Trim(field("listen"), "[]")) {
				continue
			}
			// 不写 users 时沿用全局的 authentication；写了 users: [] 则不认证。
			users := lookup(l, []string{"users"})
			if (users == nil && globalAuth) || nonEmptySeq(users) {
				continue
			}
			problems = append(problems, fmt.Sprintf("the listener %q (%s) accepts connections from other hosts without users; "+
				"add users, or listen on 127.0.0.1", field("name"), field("type")))
		}
	}
	if str("dns", "enable") == "true" {
		if addr := str("dns", "listen"); addr != "" {
			if host, _, err := net.SplitHostPort(addr); err != nil || !isLoopback(host) {
				problems = append(problems, "dns.listen is "+addr+", which answers DNS queries from other hosts (an open resolver on a public server); "+
					"keep it on 127.0.0.1 (devices using this server as a gateway are served through TUN)")
			}
		}
	}
	if str("external-controller") != "" && str("secret") == "" {
		problems = append(problems, "secret is empty, so anyone who can reach external-controller can control mihomo; do not set secret in a mixin")
	}
	if len(problems) > 0 {
		return &ExposureError{Problems: problems}
	}
	return nil
}

// LANAllowedIPs 是代理端口对局域网开放时 lan-allowed-ips 的值：私有地址段、共享地址空间 100.64/10（RFC 6598，
// 同样不会从公网直接到达）和链路本地地址。内核对回环地址同样按它检查，所以本机的 127.0.0.0/8 和 ::1 也在其中（tunlr 自己经 127.0.0.1 检测代理）。
var LANAllowedIPs = []string{
	"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16",
	"::1/128", "fc00::/7", "fe80::/10",
}

// LANAccess 报告配置是否打开了代理端口的局域网访问（allow-lan），以及来源是否只限于私有地址段。
func LANAccess(cfg []byte) (allowLAN, private bool, err error) {
	doc, err := parseNode(cfg)
	if err != nil || doc == nil {
		return false, false, err
	}
	root := doc.Content[0]
	n := lookup(root, []string{"allow-lan"})
	return n != nil && n.Kind == yaml.ScalarNode && n.Value == "true", privateOnly(lookup(root, []string{"lan-allowed-ips"})), nil
}

// privateOnly 报告 lan-allowed-ips（n）是否只允许 LANAllowedIPs 范围内的来源。不写时内核允许所有来源。
func privateOnly(n *yaml.Node) bool {
	if !nonEmptySeq(n) {
		return false
	}
	for _, item := range n.Content {
		p, err := netip.ParsePrefix(item.Value)
		if err != nil || item.Kind != yaml.ScalarNode {
			return false
		}
		within := false
		for _, s := range LANAllowedIPs {
			q := netip.MustParsePrefix(s)
			if q.Bits() <= p.Bits() && q.Contains(p.Masked().Addr()) {
				within = true
				break
			}
		}
		if !within {
			return false
		}
	}
	return true
}

func nonEmptySeq(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.SequenceNode && len(n.Content) > 0
}
