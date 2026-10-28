package netfilter

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRenderDevices 把生成的脚本与 testdata/devices.nft 逐字比对；用 -update 重新生成。
func TestRenderDevices(t *testing.T) {
	got, err := RenderDevices()
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "devices.nft")
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

func TestDevicesSync(t *testing.T) {
	f, r := newFake()
	ctx := context.Background()

	// 表不存在：整张表连同集合的内容一起加载。
	if err := r.DevicesSync(ctx, []string{"ens3"}, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.tables, []string{DevicesTable}) || !strings.Contains(string(f.stdin), "table inet tunlr_devices {") ||
		!strings.HasSuffix(string(f.stdin), "flush set inet tunlr_devices lan\nflush set inet tunlr_devices ports\n"+
			"add element inet tunlr_devices lan { \"ens3\" }\n") {
		t.Fatalf("tables %v, script:\n%s", f.tables, f.stdin)
	}

	// 表已存在：只替换集合 lan、ports，不重建表（已记下的设备保留）。
	if err := r.DevicesSync(ctx, nil, []int{7890}); err != nil {
		t.Fatal(err)
	}
	want := "flush set inet tunlr_devices lan\nflush set inet tunlr_devices ports\nadd element inet tunlr_devices ports { 7890 }\n"
	if string(f.stdin) != want {
		t.Fatalf("script:\n%s", f.stdin)
	}

	for _, bad := range []struct {
		ifaces []string
		ports  []int
	}{{[]string{`a" }`}, nil}, {nil, []int{0}}, {nil, []int{65536}}} {
		if err := r.DevicesSync(ctx, bad.ifaces, bad.ports); err == nil {
			t.Errorf("accepted %v %v", bad.ifaces, bad.ports)
		}
	}

	// 都为空：删除整张表；再次执行什么都不做。
	for range 2 {
		if err := r.DevicesSync(ctx, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.tables) != 0 {
		t.Fatalf("tables = %v", f.tables)
	}
}

func TestParseSeen(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	out := `{"nftables": [{"metainfo": {"version": "1.0.6", "json_schema_version": 1}}, {"set": {"family": "inet",
	"name": "gateway", "table": "tunlr_devices", "type": ["ether_addr", "ipv4_addr"], "flags": ["timeout"], "timeout": 86400,
	"elem": [{"elem": {"val": {"concat": ["AA:BB:CC:DD:EE:01", "192.168.1.5"]}, "timeout": 86400, "expires": 86340}},
	{"elem": {"val": {"concat": ["aa:bb:cc:dd:ee:02", "192.168.1.6"]}, "expires": 3600}},
	{"elem": {"val": {"concat": ["bad", "192.168.1.7"]}, "timeout": 86400, "expires": 1}}]}}]}`
	seen, err := parseSeen([]byte(out), UseGateway, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []Seen{
		{MAC: "aa:bb:cc:dd:ee:01", IP: "192.168.1.5", Use: UseGateway, LastSeen: now.Add(-time.Minute)},
		{MAC: "aa:bb:cc:dd:ee:02", IP: "192.168.1.6", Use: UseGateway, LastSeen: now.Add(-23 * time.Hour)},
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("seen = %+v", seen)
	}
	// 空集合没有 elem。
	if seen, err := parseSeen([]byte(`{"nftables": [{"set": {"name": "proxy", "timeout": 86400}}]}`), UseProxy, now); err != nil || len(seen) != 0 {
		t.Fatalf("empty: %v %v", seen, err)
	}
}

func TestParseNeighbors(t *testing.T) {
	out := `[{"dst":"192.168.1.5","dev":"ens3","lladdr":"AA:BB:CC:DD:EE:01","state":["REACHABLE"]},
	{"dst":"192.168.1.6","dev":"ens3","lladdr":"aa:bb:cc:dd:ee:02","state":["STALE"]},
	{"dst":"192.168.1.7","dev":"ens3","state":["FAILED"]}]`
	got, err := parseNeighbors([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := []Neighbor{
		{IP: "192.168.1.5", MAC: "aa:bb:cc:dd:ee:01", Interface: "ens3", Reachable: true},
		{IP: "192.168.1.6", MAC: "aa:bb:cc:dd:ee:02", Interface: "ens3"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("neighbors = %+v", got)
	}
	if got, err := parseNeighbors([]byte("\n")); err != nil || got != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
}
