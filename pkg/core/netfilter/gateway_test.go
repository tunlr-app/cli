package netfilter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestIPv6Egress 验证从 ip -6 route get 的输出读出 IPv6 的出口网卡：没有路由、不可达类的路由时为空。
func TestIPv6Egress(t *testing.T) {
	for out, want := range map[string]string{
		"2001:4860:4860::8888 from :: via fe80::1 dev ens3 proto ra src 2001:db8::2 metric 100 pref medium\n":       "ens3",
		"2001:4860:4860::8888 from :: dev tunlr0 table 2022 src fdfe:dcba:9876::1 metric 1024 pref medium\n":        "tunlr0",
		"unreachable 2001:4860:4860::8888 from :: dev lo table unspec proto kernel metric 4294967295 pref medium\n": "",
		"": "",
	} {
		r := &Rules{Run: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			if out == "" {
				return nil, fmt.Errorf("RTNETLINK answers: Network is unreachable")
			}
			return []byte(out), nil
		}}
		if got := r.IPv6Egress(context.Background()); got != want {
			t.Errorf("IPv6Egress(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestIPv6Route(t *testing.T) {
	for out, want := range map[string]string{
		"default via fe80::1 dev ens3 proto ra metric 100 expires 1797sec pref medium\n": "ens3",
		"default proto ra metric 100 expires 1797sec pref medium\n" +
			"\tnexthop via fe80::1 dev ens3 weight 1\n" +
			"\tnexthop via fe80::2 dev ens4 weight 1\n": "ens3",
		"unreachable default dev lo proto kernel metric 4294967295 pref medium\n": "",
		"": "",
	} {
		r := &Rules{Run: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			if got := name + " " + strings.Join(args, " "); got != "ip -6 route show default table main" {
				t.Errorf("ran %s", got)
			}
			return []byte(out), nil
		}}
		if got := r.IPv6Route(context.Background()); got != want {
			t.Errorf("IPv6Route(%q) = %q, want %q", out, got, want)
		}
	}
}

// TestRenderGateway 把生成的脚本与 testdata/gateway.nft 逐字比对；用 -update 重新生成。
func TestRenderGateway(t *testing.T) {
	got, err := RenderGateway("ens3")
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "gateway.nft")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("script mismatch (run with -update to accept)\n--- got ---\n%s", got)
	}
	for _, bad := range []string{"", "a b", `x"; flush ruleset; "`, "../x", "..", "sixteen-chars-xx"} {
		if _, err := RenderGateway(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// usesJSON 返回 nft -j list table inet tunlr_gateway 的输出，sets 是集合名到元素（地址与协议）的映射，
// 没给的集合为空（空集合没有 elem）。
func usesJSON(sets map[string][][2]string) string {
	var items []string
	for _, name := range []string{"dns_seen", "dns_passed", "forward_seen", "forward_passed", "input_seen", "input_passed"} {
		var elems []string
		for _, e := range sets[name] {
			proto := strconv.Quote(e[1])
			if _, err := strconv.Atoi(e[1]); err == nil {
				proto = e[1]
			}
			elems = append(elems, fmt.Sprintf(`{"elem": {"val": {"concat": [%q, %s]}, "timeout": 300, "expires": 298}}`, e[0], proto))
		}
		set := fmt.Sprintf(`{"set": {"family": "inet", "name": %q, "table": "tunlr_gateway", "type": ["ipv4_addr", "inet_proto"], "flags": ["timeout"], "timeout": 300`, name)
		if len(elems) > 0 {
			set += `, "elem": [` + strings.Join(elems, ", ") + `]`
		}
		items = append(items, set+"}}")
	}
	return `{"nftables": [{"metainfo": {"version": "1.0.6"}}, {"table": {"family": "inet", "name": "tunlr_gateway"}}, ` + strings.Join(items, ", ") +
		`, {"chain": {"family": "inet", "table": "tunlr_gateway", "name": "forward", "hook": "forward", "prio": -1, "policy": "accept"}}]}`
}

func TestParseUses(t *testing.T) {
	out := usesJSON(map[string][][2]string{
		"dns_seen":       {{"192.168.1.10", "udp"}, {"192.168.1.9", "udp"}},
		"dns_passed":     {{"192.168.1.9", "udp"}},
		"forward_seen":   {{"192.168.1.9", "icmp"}, {"192.168.1.9", "47"}},
		"forward_passed": {{"192.168.1.9", "udp"}},
		"input_seen":     {{"192.168.1.9", "tcp"}, {"bad", "tcp"}},
		"input_passed":   {{"192.168.1.9", "tcp"}},
	})
	got, err := parseUses([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	// 按地址的数值排序；只在 *_passed 中的也算通过；nft 不认识的协议是协议号。
	want := []GatewayUse{
		{Device: "192.168.1.9", Path: PathDNS, Protocol: "udp", Passed: true},
		{Device: "192.168.1.9", Path: PathForward, Protocol: "47"},
		{Device: "192.168.1.9", Path: PathForward, Protocol: "icmp"},
		{Device: "192.168.1.9", Path: PathForward, Protocol: "udp", Passed: true},
		{Device: "192.168.1.9", Path: PathInput, Protocol: "tcp", Passed: true},
		{Device: "192.168.1.10", Path: PathDNS, Protocol: "udp"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("uses =\n%+v\nwant\n%+v", got, want)
	}
	if got, err := parseUses([]byte(usesJSON(nil))); err != nil || len(got) != 0 {
		t.Errorf("empty: %+v, %v", got, err)
	}
	// 集合不全时报错。
	if _, err := parseUses([]byte(`{"nftables": [{"set": {"family": "inet", "name": "dns_seen", "table": "tunlr_gateway"}}]}`)); err == nil {
		t.Error("missing sets accepted")
	}
}

func TestParseBlockers(t *testing.T) {
	// UFW 与 Docker 共用 ip filter；firewalld 的链默认策略为 accept，但最后一条无条件拒绝，入站链先放行 DNAT
	// 连接（转发链的那条还带别的条件，不算）；只是有条件的 drop 不算；ip6 族、tunlr 和内核自己的表不看。
	out := `{"nftables": [{"metainfo": {"version": "1.0.6"}},
{"table": {"family": "ip", "name": "filter", "handle": 1}},
{"chain": {"family": "ip", "table": "filter", "name": "INPUT", "handle": 1, "type": "filter", "hook": "input", "prio": 0, "policy": "drop"}},
{"chain": {"family": "ip", "table": "filter", "name": "FORWARD", "handle": 2, "type": "filter", "hook": "forward", "prio": 0, "policy": "drop"}},
{"chain": {"family": "ip", "table": "filter", "name": "OUTPUT", "handle": 3, "type": "filter", "hook": "output", "prio": 0, "policy": "drop"}},
{"chain": {"family": "ip", "table": "filter", "name": "DOCKER-USER", "handle": 4}},
{"chain": {"family": "ip", "table": "filter", "name": "ufw-before-input", "handle": 5}},
{"rule": {"family": "ip", "table": "filter", "chain": "FORWARD", "handle": 9, "expr": [{"counter": {"packets": 0, "bytes": 0}}, {"jump": {"target": "DOCKER-USER"}}]}},
{"table": {"family": "inet", "name": "firewalld", "handle": 2}},
{"chain": {"family": "inet", "table": "firewalld", "name": "filter_INPUT", "handle": 1, "type": "filter", "hook": "input", "prio": 10, "policy": "accept"}},
{"chain": {"family": "inet", "table": "firewalld", "name": "filter_FORWARD", "handle": 2, "type": "filter", "hook": "forward", "prio": 10, "policy": "accept"}},
{"rule": {"family": "inet", "table": "firewalld", "chain": "filter_INPUT", "handle": 3, "expr": [{"match": {"op": "in", "left": {"ct": {"key": "status"}}, "right": "dnat"}}, {"accept": null}]}},
{"rule": {"family": "inet", "table": "firewalld", "chain": "filter_FORWARD", "handle": 6, "expr": [{"match": {"op": "in", "left": {"ct": {"key": "status"}}, "right": "dnat"}}, {"match": {"op": "==", "left": {"meta": {"key": "iifname"}}, "right": "eth1"}}, {"accept": null}]}},
{"rule": {"family": "inet", "table": "firewalld", "chain": "filter_INPUT", "handle": 4, "expr": [{"counter": {"packets": 3, "bytes": 180}}, {"reject": null}]}},
{"rule": {"family": "inet", "table": "firewalld", "chain": "filter_FORWARD", "handle": 5, "expr": [{"reject": {"type": "icmpx", "expr": "admin-prohibited"}}]}},
{"table": {"family": "inet", "name": "mine", "handle": 3}},
{"chain": {"family": "inet", "table": "mine", "name": "fwd", "handle": 1, "type": "filter", "hook": "forward", "prio": 5, "policy": "accept"}},
{"rule": {"family": "inet", "table": "mine", "chain": "fwd", "handle": 2, "expr": [{"match": {"op": "==", "left": {"meta": {"key": "iifname"}}, "right": "eth1"}}, {"drop": null}]}},
{"chain": {"family": "inet", "table": "mine", "name": "in", "handle": 3, "type": "filter", "hook": "input", "prio": 0, "policy": "drop"}},
{"chain": {"family": "ip6", "table": "filter", "name": "FORWARD", "handle": 1, "type": "filter", "hook": "forward", "prio": 0, "policy": "drop"}},
{"chain": {"family": "inet", "table": "tunlr_gateway", "name": "forward", "handle": 1, "type": "filter", "hook": "forward", "prio": -1, "policy": "drop"}},
{"chain": {"family": "inet", "table": "mihomo", "name": "forward", "handle": 1, "type": "filter", "hook": "forward", "prio": 0, "policy": "drop"}}]}`
	got, err := parseBlockers([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := []Blocker{
		{Family: "ip", Table: "filter", Chain: "INPUT", Hook: "input", Owner: OwnerUFW, DockerChain: true},
		{Family: "ip", Table: "filter", Chain: "FORWARD", Hook: "forward", Owner: OwnerUFW, DockerChain: true},
		{Family: "inet", Table: "firewalld", Chain: "filter_INPUT", Hook: "input", Owner: OwnerFirewalld, AcceptsDNAT: true},
		{Family: "inet", Table: "firewalld", Chain: "filter_FORWARD", Hook: "forward", Owner: OwnerFirewalld},
		{Family: "inet", Table: "mine", Chain: "in", Hook: "input"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("blockers =\n%+v\nwant\n%+v", got, want)
	}
	if s := got[1].String(); s != "ip filter FORWARD (UFW)" {
		t.Errorf("String() = %q", s)
	}

	// 只有 Docker：转发链属于 Docker；其他 iptables 规则的链认作 iptables。
	out = `{"nftables": [
{"chain": {"family": "ip", "table": "filter", "name": "FORWARD", "hook": "forward", "prio": 0, "policy": "drop"}},
{"chain": {"family": "ip", "table": "filter", "name": "DOCKER-USER"}},
{"chain": {"family": "ip", "table": "nat", "name": "INPUT", "hook": "input", "prio": 100, "policy": "accept"}}]}`
	if got, err := parseBlockers([]byte(out)); err != nil || len(got) != 1 || got[0].Owner != OwnerDocker || !got[0].DockerChain {
		t.Errorf("docker only: %+v, %v", got, err)
	}
	out = `{"nftables": [{"chain": {"family": "ip", "table": "filter", "name": "INPUT", "hook": "input", "prio": 0, "policy": "drop"}}]}`
	if got, _ := parseBlockers([]byte(out)); len(got) != 1 || got[0].Owner != OwnerIptables || got[0].DockerChain {
		t.Errorf("plain iptables: %+v", got)
	}
}

func TestGatewayWithoutDocker(t *testing.T) {
	f, r := newFake()
	ctx := context.Background()
	for range 2 {
		if err := r.GatewayUp(ctx, "ens3"); err != nil {
			t.Fatal(err)
		}
	}
	s, err := r.Gateway(ctx, "ens3")
	if err != nil || s != (GatewayState{Table: true}) || !s.Complete() {
		t.Fatalf("state = %+v, %v", s, err)
	}
	for _, c := range f.calls {
		if c != "iptables -S "+DockerChain && strings.HasPrefix(c, "iptables") {
			t.Errorf("called %q without the DOCKER-USER chain", c)
		}
	}
	if err := r.GatewayDown(ctx, "ens3"); err != nil {
		t.Fatal(err)
	}
	if len(f.tables) != 0 {
		t.Errorf("tables = %v", f.tables)
	}
}

func TestGatewayDocker(t *testing.T) {
	f, r := newFake()
	ctx := context.Background()
	f.docker = []string{"-j RETURN"}
	f.tables = []string{Table}
	for range 2 {
		if err := r.GatewayUp(ctx, "ens3"); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"-i tunlr0 -o ens3 -m comment --comment tunlr -j ACCEPT",
		"-i ens3 -o tunlr0 -m comment --comment tunlr -j ACCEPT",
		"-j RETURN",
	}
	if !slices.Equal(f.docker, want) {
		t.Fatalf("DOCKER-USER = %q", f.docker)
	}
	if s, _ := r.Gateway(ctx, "ens3"); s != (GatewayState{Table: true, DockerChain: true, DockerRules: true}) {
		t.Fatalf("state = %+v", s)
	}

	// Docker 重建了链：规则不见了，状态不完整，再次 GatewayUp 补上。
	f.docker = []string{"-j RETURN"}
	if s, _ := r.Gateway(ctx, "ens3"); s.Complete() {
		t.Fatalf("state = %+v", s)
	}
	// 重复的规则一并删除，其他表和规则不动。
	f.docker = append(want[:2:2], want...)
	if err := r.GatewayDown(ctx, "ens3"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.docker, []string{"-j RETURN"}) || !slices.Equal(f.tables, []string{Table}) {
		t.Fatalf("after down: %q %v", f.docker, f.tables)
	}
}
