package netfilter

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestNetns 在一个临时的 network namespace 中加载和清理真实的规则，不影响宿主机。
// 需要 root、nft 和 ip，设置 TUNLR_TEST_NETNS=1 时运行。
func TestNetns(t *testing.T) {
	if os.Getenv("TUNLR_TEST_NETNS") == "" {
		t.Skip("set TUNLR_TEST_NETNS=1 to run (needs root, nft and ip; uses a throwaway network namespace)")
	}
	ctx := context.Background()
	ns := fmt.Sprintf("tunlr-test-%d", os.Getpid())
	if _, err := Exec(ctx, nil, "ip", "netns", "add", ns); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Exec(context.Background(), nil, "ip", "netns", "del", ns) })
	inNS := func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		return Exec(ctx, stdin, "ip", append([]string{"netns", "exec", ns, name}, args...)...)
	}
	r := &Rules{Run: inNS, IPv6: func() bool { return true }}

	for range 2 {
		if err := r.Up(ctx); err != nil {
			t.Fatal(err)
		}
	}
	tables, err := r.Tables(ctx)
	if err != nil || !tables[Table] {
		t.Fatalf("tables = %v, %v", tables, err)
	}
	for _, family := range []string{"-4", "-6"} {
		rules, err := r.listRules(ctx, family)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, rule := range rules {
			if rule.priority == RulePriority && rule.ours() {
				n++
			}
		}
		if n != 1 {
			t.Errorf("ip %s: %d tunlr rules, want 1: %+v", family, n, rules)
		}
	}
	if problems, _ := r.Preflight(ctx); len(problems) > 0 {
		t.Errorf("preflight: %q", problems)
	}

	// 模拟内核被 SIGKILL 后留下的表和规则。
	for _, args := range [][]string{
		{"nft", "add", "table", "inet", KernelTable},
		{"ip", "-4", "rule", "add", "from", "all", "lookup", "2022", "priority", "9000"},
		{"ip", "-4", "rule", "add", "from", "all", "lookup", "100", "priority", "9100"},
	} {
		if _, err := inNS(ctx, nil, args[0], args[1:]...); err != nil {
			t.Fatal(err)
		}
	}
	if problems, _ := r.Preflight(ctx); len(problems) != 1 {
		t.Errorf("preflight with leftovers: %q", problems)
	}
	// 网关：没有 DOCKER-USER 链时只加载表；有链时插入放行规则，删除时只删自己的。
	for range 2 {
		if err := r.GatewayUp(ctx, "lo"); err != nil {
			t.Fatal(err)
		}
	}
	if s, err := r.Gateway(ctx, "lo"); err != nil || s != (GatewayState{Table: true}) {
		t.Fatalf("gateway = %+v, %v", s, err)
	}
	// 记下的新连接：经 lo 重定向到本机端口的 TCP 记在 input 的两个集合中，被其他防火墙丢弃时只在 input_seen 中。
	if uses, err := r.GatewayUses(ctx); err != nil || len(uses) != 0 {
		t.Fatalf("uses = %+v, %v", uses, err)
	}
	nftScript := func(script string) {
		t.Helper()
		if _, err := inNS(ctx, []byte(script), "nft", "-f", "-"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := inNS(ctx, nil, "ip", "link", "set", "lo", "up"); err != nil {
		t.Fatal(err)
	}
	// nft 1.0.6 不接受规则后紧跟 “; }”，每条规则单独一行。
	nftScript("table ip test_nat {\n\tchain out {\n\t\ttype nat hook output priority -100; policy accept;\n\t\ttcp dport 9 redirect to :10\n\t}\n}\n")
	connect := func() {
		// 没有程序监听，连接被拒绝或超时，只要 SYN 经过入站。
		inNS(ctx, nil, "timeout", "2", "bash", "-c", "echo > /dev/tcp/127.0.0.1/9")
	}
	connect()
	if uses, err := r.GatewayUses(ctx); err != nil || !slices.Equal(uses, []GatewayUse{{Device: "127.0.0.1", Path: PathInput, Protocol: "tcp", Passed: true}}) {
		t.Fatalf("uses = %+v, %v", uses, err)
	}
	nftScript("flush set inet " + GatewayTable + " input_seen\nflush set inet " + GatewayTable + " input_passed\n" +
		"table inet test_drop {\n\tchain in {\n\t\ttype filter hook input priority filter; policy accept;\n\t\ttcp dport 10 drop\n\t}\n}\n")
	connect()
	if uses, err := r.GatewayUses(ctx); err != nil || !slices.Equal(uses, []GatewayUse{{Device: "127.0.0.1", Path: PathInput, Protocol: "tcp"}}) {
		t.Fatalf("uses with a drop = %+v, %v", uses, err)
	}
	nftScript("delete table ip test_nat\ndelete table inet test_drop\n")
	if b, err := r.Blockers(ctx); err != nil || len(b) != 0 {
		t.Fatalf("blockers without iptables = %+v, %v", b, err)
	}
	for _, args := range [][]string{{"-N", DockerChain}, {"-A", DockerChain, "-j", "RETURN"}, {"-P", "FORWARD", "DROP"}} {
		if _, err := inNS(ctx, nil, "iptables", args...); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := r.Blockers(ctx); err != nil || !slices.Equal(b, []Blocker{{Family: "ip", Table: "filter", Chain: "FORWARD", Hook: "forward", Owner: OwnerDocker, DockerChain: true}}) {
		t.Fatalf("blockers with Docker's FORWARD = %+v, %v", b, err)
	}
	if err := r.GatewayUp(ctx, "lo"); err != nil {
		t.Fatal(err)
	}
	if s, _ := r.Gateway(ctx, "lo"); !s.Complete() || !s.DockerRules {
		t.Fatalf("gateway with DOCKER-USER = %+v", s)
	}
	if err := r.GatewayDown(ctx, "lo"); err != nil {
		t.Fatal(err)
	}
	if out, _ := inNS(ctx, nil, "iptables", "-S", DockerChain); string(out) != "-N DOCKER-USER\n-A DOCKER-USER -j RETURN\n" {
		t.Errorf("DOCKER-USER after down:\n%s", out)
	}

	// 访问限制：重复加载幂等，通配的容器网桥规则和 fib、th 表达式能被 nft 接受；没有端口时删表。
	access := Access{Ports: []int{7890, 9090}, Interfaces: []string{"lo"}, Containers: true}
	for range 2 {
		if err := r.AccessUp(ctx, access); err != nil {
			t.Fatal(err)
		}
	}
	if tables, _ := r.Tables(ctx); !tables[AccessTable] {
		t.Fatalf("access table not loaded: %v", tables)
	}
	if out, err := inNS(ctx, nil, "nft", "list", "table", "inet", AccessTable); err != nil ||
		!strings.Contains(string(out), `iifname "br-*"`) || !strings.Contains(string(out), "th dport @ports") {
		t.Errorf("access table: %v\n%s", err, out)
	}
	if err := r.AccessUp(ctx, Access{}); err != nil {
		t.Fatal(err)
	}
	if tables, _ := r.Tables(ctx); tables[AccessTable] {
		t.Errorf("access table left after down: %v", tables)
	}

	left, err := r.Down(ctx)
	if err != nil || !left {
		t.Fatalf("Down = %v, %v", left, err)
	}
	if tables, _ := r.Tables(ctx); len(tables) != 0 {
		t.Errorf("tables left: %v", tables)
	}
	rules, _ := r.listRules(ctx, "-4")
	for _, rule := range rules {
		if rule.priority >= RulePriority && rule.priority <= RuleIndex+10 {
			t.Errorf("rule left: %+v", rule)
		}
	}
}
