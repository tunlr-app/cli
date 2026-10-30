package netfilter

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRenderAccess 把生成的脚本与 testdata/access.nft 逐字比对；用 -update 重新生成。端口和网卡去重并排序。
func TestRenderAccess(t *testing.T) {
	got, err := RenderAccess(Access{Ports: []int{9090, 7890, 7890}, Interfaces: []string{"tailscale0", "eth1", "eth1"}, Containers: true})
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "access.nft")
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

	// 没有网卡、也不放行容器时只放行本机：不写空集合（nft 不接受空的元素列表）。
	only, err := RenderAccess(Access{Ports: []int{7890}})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(only); strings.Contains(s, "set interfaces") || strings.Contains(s, "docker0") || !strings.Contains(s, "iifname \"lo\" return\n\t\tdrop\n") {
		t.Errorf("loopback only:\n%s", s)
	}

	for _, bad := range []Access{{}, {Ports: []int{0}}, {Ports: []int{65536}}, {Ports: []int{7890}, Interfaces: []string{"a b"}}} {
		if _, err := RenderAccess(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestAccessUpDown(t *testing.T) {
	f, r := newFake()
	ctx := context.Background()
	f.tables = []string{Table}
	for range 2 {
		if err := r.AccessUp(ctx, Access{Ports: []int{9090}, Interfaces: []string{"ens3"}}); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(f.tables, []string{Table, AccessTable}) {
		t.Fatalf("tables = %v", f.tables)
	}
	// 没有端口时删除整张表。
	for range 2 {
		if err := r.AccessUp(ctx, Access{Interfaces: []string{"ens3"}}); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(f.tables, []string{Table}) {
		t.Fatalf("after down: %v", f.tables)
	}
}
