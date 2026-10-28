package config

import (
	"net"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// Listener 是配置让内核监听的一个端口。
type Listener struct {
	// Key 是配置中的键，如 mixed-port、dns.listen。
	Key string `json:"key"`
	// Network 是 tcp 或 udp。
	Network string `json:"network"`
	Port    int    `json:"port"`
}

// Listening 是配置中内核监听的端口，以及代理端口是否要求认证。
type Listening struct {
	Listeners []Listener
	// MixedPort 是 mixed-port，没有时为 0。
	MixedPort int
	// Auth 报告是否设置了 authentication（代理端口需要用户名和密码）。
	Auth bool
}

// Listen 从配置中读取内核监听的端口：各代理端口、external-controller、启用时的 dns.listen。
func Listen(cfg []byte) (*Listening, error) {
	doc, err := parseNode(cfg)
	if err != nil || doc == nil {
		return &Listening{}, err
	}
	root := doc.Content[0]
	l := &Listening{}
	for _, k := range []string{"mixed-port", "port", "socks-port", "redir-port", "tproxy-port"} {
		n := lookup(root, []string{k})
		var port int
		if n == nil || n.Decode(&port) != nil || port <= 0 {
			continue
		}
		l.Listeners = append(l.Listeners, Listener{Key: k, Network: "tcp", Port: port})
		if k == "mixed-port" {
			l.MixedPort = port
		}
	}
	if port := addrPort(lookup(root, []string{"external-controller"})); port > 0 {
		l.Listeners = append(l.Listeners, Listener{Key: "external-controller", Network: "tcp", Port: port})
	}
	var on bool
	if n := lookup(root, []string{"dns", "enable"}); n != nil && n.Decode(&on) == nil && on {
		if port := addrPort(lookup(root, []string{"dns", "listen"})); port > 0 {
			l.Listeners = append(l.Listeners, Listener{Key: "dns.listen", Network: "udp", Port: port})
		}
	}
	if n := lookup(root, []string{"authentication"}); n != nil && n.Kind == yaml.SequenceNode && len(n.Content) > 0 {
		l.Auth = true
	}
	return l, nil
}

// addrPort 返回 host:port 形式的值中的端口，不是这种形式时为 0。
func addrPort(n *yaml.Node) int {
	if n == nil || n.Kind != yaml.ScalarNode {
		return 0
	}
	_, p, err := net.SplitHostPort(n.Value)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return 0
	}
	return port
}
