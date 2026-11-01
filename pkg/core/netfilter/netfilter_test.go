package netfilter

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/tunlr.nft")

// TestRender 把生成的脚本与 testdata/tunlr.nft、tunlr-ipv6-dns.nft（拒绝经 IPv6 的 DNS 查询）逐字比对；
// 用 -update 重新生成。脚本本身由 nft 在 network namespace 中的集成测试验证。
func TestRender(t *testing.T) {
	for name, o := range map[string]TableOptions{"tunlr.nft": {}, "tunlr-ipv6-dns.nft": {RejectIPv6DNS: true}} {
		got, err := Render(o)
		if err != nil {
			t.Fatal(err)
		}
		golden := filepath.Join("testdata", name)
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
			t.Fatalf("%s: script mismatch (run with -update to accept)\n--- got ---\n%s", name, got)
		}
	}
}

// fakeSystem 模拟 nft、ip 和 iptables：记录执行的命令，维护表、ip rule 和 DOCKER-USER 规则的列表。
type fakeSystem struct {
	calls  []string
	tables []string
	rules  map[string][]string // 地址族 → ip rule show 的各行
	stdin  []byte
	// docker 为 nil 时没有 DOCKER-USER 链；否则是链中规则的参数（不含链名）。
	docker []string
}

func (f *fakeSystem) run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case call == "nft -f -":
		f.stdin = stdin
		// 脚本的最后一个 “table inet X {” 是加载的表。
		for line := range strings.Lines(string(stdin)) {
			if t, ok := strings.CutPrefix(line, "table inet "); ok && strings.HasSuffix(t, " {\n") {
				if t = strings.TrimSuffix(t, " {\n"); !slices.Contains(f.tables, t) {
					f.tables = append(f.tables, t)
				}
			}
		}
	case name == "iptables":
		if f.docker == nil || args[1] != DockerChain {
			return nil, errors.New("iptables: No chain/target/match by that name.")
		}
		rule := strings.Join(args[2:], " ")
		switch args[0] {
		case "-S":
		case "-C":
			if !slices.Contains(f.docker, rule) {
				return nil, errors.New("iptables: Bad rule (does a matching rule exist in that chain?).")
			}
		case "-I":
			f.docker = slices.Insert(f.docker, 0, rule)
		case "-D":
			i := slices.Index(f.docker, rule)
			if i < 0 {
				return nil, errors.New("iptables: Bad rule")
			}
			f.docker = slices.Delete(f.docker, i, i+1)
		}
	case call == "nft list tables inet":
		var b strings.Builder
		for _, t := range f.tables {
			fmt.Fprintf(&b, "table inet %s\n", t)
		}
		return []byte(b.String()), nil
	case strings.HasPrefix(call, "nft delete table inet "):
		f.tables = slices.DeleteFunc(f.tables, func(t string) bool { return t == args[3] })
	case name == "ip" && args[2] == "show":
		return []byte(strings.Join(f.rules[args[0]], "\n") + "\n"), nil
	case name == "ip" && args[2] == "add":
		f.rules[args[0]] = append(f.rules[args[0]], "8999:\tfrom all fwmark 0x10000000/0x10000000 lookup main")
	case name == "ip" && args[2] == "del":
		prefix := args[len(args)-1] + ":"
		if i := slices.IndexFunc(f.rules[args[0]], func(r string) bool { return strings.HasPrefix(r, prefix) }); i >= 0 {
			f.rules[args[0]] = slices.Delete(f.rules[args[0]], i, i+1)
			return nil, nil
		}
		return nil, errors.New("RTNETLINK answers: No such file or directory")
	case name == "systemctl":
		return []byte("inactive\n"), errors.New("exit status 3")
	}
	return nil, nil
}

func newFake() (*fakeSystem, *Rules) {
	f := &fakeSystem{rules: map[string][]string{
		"-4": {"0:\tfrom all lookup local", "32766:\tfrom all lookup main", "32767:\tfrom all lookup default"},
		"-6": {"0:\tfrom all lookup local", "32766:\tfrom all lookup main"},
	}}
	return f, &Rules{Run: f.run, IPv6: func() bool { return true }}
}

func countRules(f *fakeSystem, family, prefix string) int {
	n := 0
	for _, r := range f.rules[family] {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func TestUpIsIdempotent(t *testing.T) {
	f, r := newFake()
	ctx := context.Background()
	for range 2 {
		if err := r.Up(ctx, TableOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(string(f.stdin), "table inet tunlr {") {
		t.Fatalf("nft script:\n%s", f.stdin)
	}
	for _, family := range []string{"-4", "-6"} {
		if n := countRules(f, family, "8999:"); n != 1 {
			t.Errorf("ip %s: %d rules at 8999, want 1: %v", family, n, f.rules[family])
		}
	}
}

func TestDown(t *testing.T) {
	f, r := newFake()
	ctx := context.Background()
	if err := r.Up(ctx, TableOptions{}); err != nil {
		t.Fatal(err)
	}
	// 内核正常退出：只清理工具自己的表和规则。
	left, err := r.Down(ctx)
	if err != nil || left {
		t.Fatalf("Down = %v, %v", left, err)
	}
	if len(f.tables) != 0 || countRules(f, "-4", "8999:") != 0 || countRules(f, "-6", "8999:") != 0 {
		t.Fatalf("left behind: %v %v", f.tables, f.rules)
	}

	// 内核被 SIGKILL：它的表和 9000～9010 的规则一并清理，其他优先级的规则不动。
	f.tables = []string{KernelTable, "other"}
	f.rules["-4"] = append(f.rules["-4"], "9000:\tfrom all to 198.18.0.0/30 lookup 2022",
		"9002:\tnot from all iif lo lookup 2022", "9010:\tfrom all nop", "9100:\tfrom all lookup 100")
	left, err = r.Down(ctx)
	if err != nil || !left {
		t.Fatalf("Down = %v, %v", left, err)
	}
	if !slices.Equal(f.tables, []string{"other"}) {
		t.Errorf("tables = %v", f.tables)
	}
	if countRules(f, "-4", "90") != 0 || countRules(f, "-4", "9100:") != 1 {
		t.Errorf("rules = %v", f.rules["-4"])
	}
}

func TestPreflightForeignRules(t *testing.T) {
	_, r := newFake()
	r.Run = func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		if name == "ip" && args[0] == "-4" {
			return []byte("0:\tfrom all lookup local\n8999:\tfrom all fwmark 0x10000000/0x10000000 lookup main\n9005:\tfrom all lookup 77\n"), nil
		}
		if name == "nft" {
			// UFW 的入站链默认丢弃。
			return []byte(`{"nftables": [{"chain": {"family": "ip", "table": "filter", "name": "INPUT", "hook": "input", "prio": 0, "policy": "drop"}},
{"chain": {"family": "ip", "table": "filter", "name": "ufw-before-input"}}]}`), nil
		}
		return []byte("0:\tfrom all lookup local\n"), nil
	}
	problems, warnings := r.Preflight(context.Background())
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		// 开发机（如 Windows）上没有 TUN 和 nft，只检查到这一步。
		if len(problems) == 0 {
			t.Fatal("no problem reported without /dev/net/tun")
		}
		return
	}
	if len(problems) != 1 || !strings.Contains(problems[0], `"from all lookup 77"`) {
		t.Errorf("problems = %q", problems)
	}
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "ip filter INPUT (UFW) drops incoming connections by default") {
		t.Errorf("warnings = %q", warnings)
	}
}

func TestUIDRuleMissing(t *testing.T) {
	f, r := newFake()
	ctx := context.Background()
	// 内核没有建立 auto-route 的规则（TUN 关着）：不算缺。
	if missing, err := r.UIDRuleMissing(ctx, 999); err != nil || len(missing) != 0 {
		t.Fatalf("no kernel rules: %v %v", missing, err)
	}
	f.rules["-4"] = append(f.rules["-4"], "9000:\tfrom all uidrange 999-999 goto 9010", "9003:\tnot from all iif lo lookup 2022", "9010:\tfrom all nop")
	f.rules["-6"] = append(f.rules["-6"], "9001:\tnot from all iif lo lookup 2022", "9010:\tfrom all nop")
	if missing, err := r.UIDRuleMissing(ctx, 999); err != nil || !slices.Equal(missing, []string{"-6"}) {
		t.Errorf("missing = %v %v", missing, err)
	}
	// UID 99 不能匹配到 999-999。
	if missing, _ := r.UIDRuleMissing(ctx, 99); !slices.Equal(missing, []string{"-4", "-6"}) {
		t.Errorf("other uid: %v", missing)
	}
}
