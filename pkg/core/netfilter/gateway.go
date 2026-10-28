package netfilter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"text/template"

	"github.com/tunlr-app/cli/templates"
)

const (
	// GatewayTable 是网关模式的 nftables 表（inet 族）：局域网转发来的流量只能经 TUN 出去。
	GatewayTable = "tunlr_gateway"
	// DockerChain 是 Docker 留给用户的 iptables 链，转发链策略为 drop 时在这里放行（DESIGN.md 8.10）。
	DockerChain = "DOCKER-USER"
	// ruleComment 标记工具在 DockerChain 中插入的规则。
	ruleComment = "tunlr"
)

// ifaceName 是合法的网卡名：最多 15 个字符，不含空白、引号和斜杠。
var ifaceName = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,15}$`)

// ValidInterface 检查网卡名能否安全地写进 nft 脚本和 procfs 路径。
func ValidInterface(iface string) error {
	if !ifaceName.MatchString(iface) || iface == "." || iface == ".." {
		return fmt.Errorf("invalid interface name %q", iface)
	}
	return nil
}

// RenderGateway 生成 inet tunlr_gateway 表的 nft 脚本，iface 是局域网网卡。
func RenderGateway(iface string) ([]byte, error) {
	if err := ValidInterface(iface); err != nil {
		return nil, err
	}
	t, err := template.New("gateway.nft").Option("missingkey=error").Parse(string(templates.GatewayNft))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = t.Execute(&b, map[string]string{
		"Table":     GatewayTable,
		"Device":    Device,
		"Interface": iface,
	})
	return b.Bytes(), err
}

// GatewayState 是网关规则的实际情况。
type GatewayState struct {
	// Table 报告 inet tunlr_gateway 表是否存在。
	Table bool `json:"table"`
	// DockerChain 报告 DOCKER-USER 链是否存在；DockerRules 报告两条放行规则是否都在。
	DockerChain bool `json:"docker_chain"`
	DockerRules bool `json:"docker_rules"`
}

// Complete 报告网关规则是否都已到位。
func (s GatewayState) Complete() bool {
	return s.Table && (!s.DockerChain || s.DockerRules)
}

// Gateway 返回局域网网卡为 iface 时网关规则的实际情况。
func (r *Rules) Gateway(ctx context.Context, iface string) (GatewayState, error) {
	var s GatewayState
	tables, err := r.Tables(ctx)
	if err != nil {
		return s, err
	}
	s.Table = tables[GatewayTable]
	if s.DockerChain = r.dockerChain(ctx); s.DockerChain {
		s.DockerRules = true
		for _, rule := range dockerRules(iface) {
			if !r.hasDockerRule(ctx, rule) {
				s.DockerRules = false
			}
		}
	}
	return s, nil
}

// GatewayUp 加载（或替换）inet tunlr_gateway 表；DOCKER-USER 链存在时插入缺少的放行规则。可以重复执行。
func (r *Rules) GatewayUp(ctx context.Context, iface string) error {
	script, err := RenderGateway(iface)
	if err != nil {
		return err
	}
	if _, err := r.run(ctx, script, "nft", "-f", "-"); err != nil {
		return err
	}
	if !r.dockerChain(ctx) {
		return nil
	}
	for _, rule := range dockerRules(iface) {
		if r.hasDockerRule(ctx, rule) {
			continue
		}
		if _, err := r.run(ctx, nil, "iptables", append([]string{"-I", DockerChain}, rule...)...); err != nil {
			return err
		}
	}
	return nil
}

// GatewayDown 删除 DOCKER-USER 中的放行规则（可能因重复插入而有多条）和 inet tunlr_gateway 表。
func (r *Rules) GatewayDown(ctx context.Context, iface string) error {
	var errs []error
	if ValidInterface(iface) == nil && r.dockerChain(ctx) {
		for _, rule := range dockerRules(iface) {
			// 每次删除一条，最多删 10 次，避免 -C 与 -D 的判断不一致时死循环。
			for n := 0; n < 10 && r.hasDockerRule(ctx, rule); n++ {
				if _, err := r.run(ctx, nil, "iptables", append([]string{"-D", DockerChain}, rule...)...); err != nil {
					errs = append(errs, err)
					break
				}
			}
		}
	}
	tables, err := r.Tables(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if tables[GatewayTable] {
		if _, err := r.run(ctx, nil, "nft", "delete", "table", "inet", GatewayTable); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DefaultInterface 返回 IPv4 默认路由所在的网卡。ip route 的输出形如 “default via 192.168.1.1 dev ens3 proto dhcp”。
func (r *Rules) DefaultInterface(ctx context.Context) (string, error) {
	out, err := r.run(ctx, nil, "ip", "-4", "route", "show", "default")
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "dev" && f[i+1] != Device {
				return f[i+1], nil
			}
		}
	}
	return "", errors.New("there is no IPv4 default route; give the LAN interface with --interface")
}

// Interface 是一块网卡的情况。
type Interface struct {
	Name string
	Up   bool
	// IPv4 是网卡的 IPv4 地址（不含前缀长度）。
	IPv4 []string
}

// LookupInterface 返回名为 name 的网卡；不存在时返回错误。
func LookupInterface(name string) (*Interface, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	res := &Interface{Name: name, Up: ifi.Flags&net.FlagUp != 0}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			res.IPv4 = append(res.IPv4, ipn.IP.String())
		}
	}
	return res, nil
}

// dockerRules 是 DOCKER-USER 中放行局域网与 TUN 之间转发的两条规则（不含链名）。
func dockerRules(iface string) [][]string {
	rule := func(in, out string) []string {
		return []string{"-i", in, "-o", out, "-m", "comment", "--comment", ruleComment, "-j", "ACCEPT"}
	}
	return [][]string{rule(iface, Device), rule(Device, iface)}
}

// dockerChain 报告 DOCKER-USER 链是否存在。没有 iptables 命令或没有这条链时 iptables -S 都会失败。
func (r *Rules) dockerChain(ctx context.Context) bool {
	_, err := r.run(ctx, nil, "iptables", "-S", DockerChain)
	return err == nil
}

func (r *Rules) hasDockerRule(ctx context.Context, rule []string) bool {
	_, err := r.run(ctx, nil, "iptables", append([]string{"-C", DockerChain}, rule...)...)
	return err == nil
}
