package config

import (
	"fmt"
	"net"
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
	if str("allow-lan") == "true" && !globalAuth {
		problems = append(problems, "allow-lan is on without authentication, so anyone who can reach this server can use the proxy ports; "+
			"add authentication (e.g. authentication: [\"user:password\"]) in a mixin, or set allow-lan: false")
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

func nonEmptySeq(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.SequenceNode && len(n.Content) > 0
}
