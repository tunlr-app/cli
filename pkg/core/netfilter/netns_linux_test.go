package netfilter

import (
	"context"
	"fmt"
	"os"
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
	if _, err := inNS(ctx, nil, "iptables", "-N", DockerChain); err != nil {
		t.Fatal(err)
	}
	if _, err := inNS(ctx, nil, "iptables", "-A", DockerChain, "-j", "RETURN"); err != nil {
		t.Fatal(err)
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
