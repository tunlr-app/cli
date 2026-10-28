package netfilter

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestRenderPanel 把生成的脚本与 testdata/panel.nft 逐字比对；用 -update 重新生成。
func TestRenderPanel(t *testing.T) {
	got, err := RenderPanel(9090)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "panel.nft")
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
	for _, bad := range []int{0, -1, 65536} {
		if _, err := RenderPanel(bad); err == nil {
			t.Errorf("accepted %d", bad)
		}
	}
}

func TestPanelUpDown(t *testing.T) {
	f, r := newFake()
	ctx := context.Background()
	f.tables = []string{Table}
	for range 2 {
		if err := r.PanelUp(ctx, 9090); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(f.tables, []string{Table, PanelTable}) {
		t.Fatalf("tables = %v", f.tables)
	}
	for range 2 {
		if err := r.PanelDown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(f.tables, []string{Table}) {
		t.Fatalf("after down: %v", f.tables)
	}
}
