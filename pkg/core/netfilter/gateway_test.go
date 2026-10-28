package netfilter

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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
