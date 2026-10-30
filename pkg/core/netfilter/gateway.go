package netfilter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

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
	err = t.Execute(&b, privateVars(map[string]string{
		"Table":     GatewayTable,
		"Device":    Device,
		"Interface": iface,
		"Window":    fmt.Sprintf("%dm", int(GatewayWindow.Minutes())),
	}))
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
	// PointToPoint 报告它是点对点的网卡：WireGuard、Tailscale 等 VPN 隧道（tunlr0 也是）。
	PointToPoint bool
	// IPv4 是网卡的 IPv4 地址（不含前缀长度）。
	IPv4 []string
}

// LookupInterface 返回名为 name 的网卡；不存在时返回错误。
func LookupInterface(name string) (*Interface, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	res := &Interface{Name: name, Up: ifi.Flags&net.FlagUp != 0, PointToPoint: ifi.Flags&net.FlagPointToPoint != 0}
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

// GatewayWindow 是 inet tunlr_gateway 记住局域网设备新连接的时长（见 templates/nft/gateway.nft）。
const GatewayWindow = 5 * time.Minute

// 局域网设备的新连接经网关的路（GatewayUse.Path），也是 inet tunlr_gateway 中集合名的前缀。
const (
	// PathDNS 是被内核劫持（DNAT）、转发进 TUN 的 DNS。
	PathDNS = "dns"
	// PathForward 是其余转发进 TUN 的：UDP、ping 等。
	PathForward = "forward"
	// PathInput 是被内核重定向（DNAT）到本机端口的，即 TCP。
	PathInput = "input"
)

// PathHook 返回 path 所经的钩子：forward 或 input。
func PathHook(path string) string {
	if path == PathInput {
		return "input"
	}
	return "forward"
}

// GatewayUse 是一台局域网设备最近 GatewayWindow 内经网关一条路、一种协议的新连接。
type GatewayUse struct {
	// Device 是设备的 IPv4 地址。
	Device string `json:"device"`
	// Path 是 PathDNS、PathForward 或 PathInput。
	Path string `json:"path"`
	// Protocol 如 tcp、udp、icmp；nft 不认识的协议为协议号。
	Protocol string `json:"protocol"`
	// Passed 报告其中有通过了其他防火墙的；为 false 时都被丢弃了。
	Passed bool `json:"passed"`
}

// GatewayUses 返回 inet tunlr_gateway 记下的新连接，按设备、路、协议排序。
func (r *Rules) GatewayUses(ctx context.Context) ([]GatewayUse, error) {
	out, err := r.run(ctx, nil, "nft", "-j", "list", "table", "inet", GatewayTable)
	if err != nil {
		return nil, err
	}
	return parseUses(out)
}

// parseUses 解析 nft -j list table 的输出中的集合 <路>_seen 与 <路>_passed，元素形如
// {"elem": {"val": {"concat": ["192.168.1.5", "udp"]}, "timeout": 300, "expires": 298}}。
func parseUses(out []byte) ([]GatewayUse, error) {
	var doc struct {
		Nftables []struct {
			Set *struct {
				Name  string `json:"name"`
				Table string `json:"table"`
				Elem  []struct {
					Elem struct {
						Val struct {
							Concat []any `json:"concat"`
						} `json:"val"`
					} `json:"elem"`
				} `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("nft -j list table inet %s: %w", GatewayTable, err)
	}
	type key struct {
		device      netip.Addr
		path, proto string
	}
	passed := map[key]bool{}
	sets := 0
	for _, item := range doc.Nftables {
		s := item.Set
		if s == nil || s.Table != GatewayTable {
			continue
		}
		path, which, _ := strings.Cut(s.Name, "_")
		if !slices.Contains([]string{PathDNS, PathForward, PathInput}, path) || (which != "seen" && which != "passed") {
			continue
		}
		sets++
		for _, e := range s.Elem {
			c := e.Elem.Val.Concat
			if len(c) != 2 {
				continue
			}
			ip, _ := c[0].(string)
			addr, err := netip.ParseAddr(ip)
			var proto string
			switch v := c[1].(type) {
			case string:
				proto = v
			case float64:
				proto = strconv.Itoa(int(v))
			}
			if err != nil || proto == "" {
				continue
			}
			k := key{addr, path, proto}
			// 两个集合同时刷新，只在 *_passed 中的（*_seen 中的刚好先过期）也算通过。
			passed[k] = passed[k] || which == "passed"
		}
	}
	if sets != 6 {
		return nil, fmt.Errorf("the sets in the nftables table inet %s are missing", GatewayTable)
	}
	keys := slices.SortedFunc(maps.Keys(passed), func(a, b key) int {
		if c := a.device.Compare(b.device); c != 0 {
			return c
		}
		return strings.Compare(a.path+" "+a.proto, b.path+" "+b.proto)
	})
	uses := make([]GatewayUse, len(keys))
	for n, k := range keys {
		uses[n] = GatewayUse{Device: k.device.String(), Path: k.path, Protocol: k.proto, Passed: passed[k]}
	}
	return uses, nil
}

// 认出的规则链所属的程序（Blocker.Owner）。
const (
	OwnerUFW       = "ufw"
	OwnerFirewalld = "firewalld"
	OwnerDocker    = "docker"
	OwnerIptables  = "iptables"
)

// Blocker 是其他程序挂在转发或入站上、默认丢弃的规则链：默认策略为 drop，或最后一条规则无条件丢弃、拒绝
// （firewalld 的链即是如此）。它们可能挡住局域网设备经网关的流量；挡没挡住要看实际的新连接（GatewayUses）：
// 排在前面的规则可能已经放行。
type Blocker struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Chain  string `json:"chain"`
	// Hook 是 forward 或 input。
	Hook string `json:"hook"`
	// Owner 是从表名、链名认出的程序（OwnerUFW 等），认不出时为空。
	Owner string `json:"owner,omitempty"`
	// DockerChain 报告同一张表中有 DOCKER-USER 链：Docker 把跳到它的规则放在转发链最前面，tunlr 在其中的放行
	// 让转发不再经过这条链后面的规则。
	DockerChain bool `json:"docker_chain,omitempty"`
	// AcceptsDNAT 报告链中有无条件放行 DNAT 连接的规则（ct status dnat accept，firewalld 即是如此）：被内核
	// 劫持的 DNS、重定向的 TCP 不会被它丢弃。
	AcceptsDNAT bool `json:"accepts_dnat,omitempty"`
}

func (b Blocker) String() string {
	s := b.Family + " " + b.Table + " " + b.Chain
	switch b.Owner {
	case OwnerUFW:
		s += " (UFW)"
	case OwnerFirewalld, OwnerDocker:
		s += " (" + b.Owner + ")"
	}
	return s
}

// BlockerNames 返回每条链的 String()。
func BlockerNames(list []Blocker) []string {
	names := make([]string, len(list))
	for n, b := range list {
		names[n] = b.String()
	}
	return names
}

// Suspects 返回 blockers 中可能丢弃 path 上的流量的：挂在它所经的钩子上；DNAT 的连接（DNS、重定向的 TCP）
// 不算先放行 DNAT 的链；dockerHandled 为 true 时不算有 DOCKER-USER 链的表中的转发链（tunlr 在 DOCKER-USER
// 中的放行让转发不再经过它后面的规则）。
func Suspects(blockers []Blocker, path string, dockerHandled bool) []Blocker {
	hook := PathHook(path)
	var list []Blocker
	for _, b := range blockers {
		switch {
		case b.Hook != hook:
		case path != PathForward && b.AcceptsDNAT:
		case dockerHandled && hook == "forward" && b.DockerChain:
		default:
			list = append(list, b)
		}
	}
	return list
}

// Fixes 返回放行 path 上来自网卡 iface 的流量的方法，每个可疑的规则链一句（同样的只列一次）；suspects 为空时
// 提示去哪里找。
func Fixes(iface, path string, suspects []Blocker) []string {
	var fixes []string
	add := func(s string) {
		if !slices.Contains(fixes, s) {
			fixes = append(fixes, s)
		}
	}
	hook := PathHook(path)
	forward := hook == "forward"
	for _, b := range suspects {
		switch {
		case b.Owner == OwnerFirewalld:
			// 局域网网卡和 tunlr0 都不在任何区域时落到默认区域，转发不放行；trusted 区域放行它的一切。
			add(fmt.Sprintf("firewall-cmd --permanent --zone=trusted --add-interface=%s, then firewall-cmd --reload", iface))
		case b.Owner == OwnerUFW && forward:
			add(fmt.Sprintf("ufw route allow in on %s out on %s", iface, Device))
		case b.Owner == OwnerUFW:
			add("ufw allow in on " + iface)
		case b.Owner == OwnerDocker && forward:
			add(fmt.Sprintf("tunlr tun gateway on --interface %s restores the accept rules in %s", iface, DockerChain))
		case b.Owner == OwnerIptables && forward:
			add(fmt.Sprintf("iptables -I %s -i %s -o %s -j ACCEPT", b.Chain, iface, Device))
		case b.Owner == OwnerIptables:
			add(fmt.Sprintf("iptables -I %s -i %s -m conntrack --ctstate DNAT -j ACCEPT", b.Chain, iface))
		case forward:
			add(fmt.Sprintf(`accept it in %s %s %s before the drop: iifname "%s" oifname "%s" accept`, b.Family, b.Table, b.Chain, iface, Device))
		default:
			add(fmt.Sprintf(`accept it in %s %s %s before the drop: iifname "%s" ct status dnat accept`, b.Family, b.Table, b.Chain, iface))
		}
	}
	if len(suspects) == 0 {
		add("no nftables chain that drops by default explains it; look for other rules on the " + hook + " hook, or in iptables-legacy (iptables-legacy -S)")
	}
	return fixes
}

// Blockers 返回其他程序挂在转发、入站上默认丢弃的规则链。只看 IPv4 经过的 ip、inet 族；不含 tunlr 和内核的表。
// iptables-legacy 的规则不在 nftables 中，看不到。
func (r *Rules) Blockers(ctx context.Context) ([]Blocker, error) {
	out, err := r.run(ctx, nil, "nft", "-j", "list", "ruleset")
	if err != nil {
		return nil, err
	}
	return parseBlockers(out)
}

// parseBlockers 解析 nft -j list ruleset 的输出：基础链形如
// {"chain": {"family": "ip", "table": "filter", "name": "FORWARD", "hook": "forward", "prio": 0, "policy": "drop"}}，
// 规则按顺序列出，形如 {"rule": {"family": "inet", "table": "firewalld", "chain": "filter_FORWARD", "expr": [{"reject": {...}}]}}。
func parseBlockers(out []byte) ([]Blocker, error) {
	type chain struct {
		Family string `json:"family"`
		Table  string `json:"table"`
		Name   string `json:"name"`
		Hook   string `json:"hook"`
		Policy string `json:"policy"`
	}
	var doc struct {
		Nftables []struct {
			Chain *chain `json:"chain"`
			Rule  *struct {
				Family string                       `json:"family"`
				Table  string                       `json:"table"`
				Chain  string                       `json:"chain"`
				Expr   []map[string]json.RawMessage `json:"expr"`
			} `json:"rule"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("nft -j list ruleset: %w", err)
	}
	type key struct{ family, table, chain string }
	var bases []chain
	chains := map[key][]string{} // 表（chain 为空）→ 其中的链名
	final := map[key]bool{}      // 链 → 最后一条规则是否无条件丢弃、拒绝
	dnat := map[key]bool{}       // 链 → 是否有 ct status dnat accept
	for _, item := range doc.Nftables {
		switch {
		case item.Chain != nil:
			c := *item.Chain
			t := key{c.Family, c.Table, ""}
			chains[t] = append(chains[t], c.Name)
			if (c.Hook == "forward" || c.Hook == "input") && (c.Family == "ip" || c.Family == "inet") &&
				!strings.HasPrefix(c.Table, Table) && c.Table != KernelTable {
				bases = append(bases, c)
			}
		case item.Rule != nil:
			r := item.Rule
			k := key{r.Family, r.Table, r.Chain}
			final[k] = dropsAll(r.Expr)
			dnat[k] = dnat[k] || acceptsDNAT(r.Expr)
		}
	}
	var blockers []Blocker
	for _, c := range bases {
		if c.Policy != "drop" && !final[key{c.Family, c.Table, c.Name}] {
			continue
		}
		names := chains[key{c.Family, c.Table, ""}]
		b := Blocker{Family: c.Family, Table: c.Table, Chain: c.Name, Hook: c.Hook,
			DockerChain: c.Family == "ip" && slices.Contains(names, DockerChain), AcceptsDNAT: dnat[key{c.Family, c.Table, c.Name}]}
		switch {
		case c.Table == "firewalld":
			b.Owner = OwnerFirewalld
		case c.Family == "ip" && c.Table == "filter" && slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, "ufw-") }):
			b.Owner = OwnerUFW
		case c.Family == "ip" && c.Table == "filter" && b.DockerChain:
			b.Owner = OwnerDocker
		case c.Family == "ip" && c.Table == "filter":
			b.Owner = OwnerIptables
		}
		blockers = append(blockers, b)
	}
	return blockers, nil
}

// dropsAll 报告规则是否无条件丢弃或拒绝：除计数外只有一个 drop 或 reject（iptables-nft 的规则总带计数）。
func dropsAll(expr []map[string]json.RawMessage) bool {
	verdict := false
	for _, e := range expr {
		for k := range e {
			switch {
			case k == "counter":
			case (k == "drop" || k == "reject") && !verdict:
				verdict = true
			default:
				return false
			}
		}
	}
	return verdict
}

// acceptsDNAT 报告规则是否只凭 DNAT 放行：除计数外只有 ct status dnat 和 accept，JSON 中的匹配形如
// {"match": {"op": "in", "left": {"ct": {"key": "status"}}, "right": "dnat"}}。
func acceptsDNAT(expr []map[string]json.RawMessage) bool {
	var match, accept bool
	for _, e := range expr {
		for k, v := range e {
			switch k {
			case "counter":
			case "accept":
				accept = true
			case "match":
				var m struct {
					Op   string `json:"op"`
					Left struct {
						Ct struct {
							Key string `json:"key"`
						} `json:"ct"`
					} `json:"left"`
					Right any `json:"right"`
				}
				if json.Unmarshal(v, &m) != nil || (m.Op != "in" && m.Op != "==") || m.Left.Ct.Key != "status" || m.Right != "dnat" || match {
					return false
				}
				match = true
			default:
				return false
			}
		}
	}
	return match && accept
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
