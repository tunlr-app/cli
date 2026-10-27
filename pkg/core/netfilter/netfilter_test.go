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

// TestRender 把生成的脚本与 testdata/tunlr.nft 逐字比对；用 -update 重新生成。
// 脚本本身由 nft 在 network namespace 中的集成测试验证。
func TestRender(t *testing.T) {
	got, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "tunlr.nft")
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
}

// fakeSystem 模拟 nft 和 ip：记录执行的命令，维护表和 ip rule 的列表。
type fakeSystem struct {
	calls  []string
	tables []string
	rules  map[string][]string // 地址族 → ip rule show 的各行
	stdin  []byte
}

func (f *fakeSystem) run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case call == "nft -f -":
		f.stdin = stdin
		if !slices.Contains(f.tables, Table) {
			f.tables = append(f.tables, Table)
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
		if err := r.Up(ctx); err != nil {
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
	if err := r.Up(ctx); err != nil {
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
		if name == "systemctl" && args[1] == "ufw" {
			return []byte("active\n"), nil
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
	if len(warnings) != 1 || !strings.Contains(warnings[0], "ufw") {
		t.Errorf("warnings = %q", warnings)
	}
}
