package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/state"
)

func TestChooseLANAccess(t *testing.T) {
	ctx := context.Background()
	p := &scriptedPrompt{t: t}
	out := &bytes.Buffer{}
	a := &app{layout: paths.Layout{Root: t.TempDir()}, stdout: out, terminal: func() bool { return true }, prompt: p}
	cands := []setup.LANInterface{
		{Name: "ens3", Addresses: []string{"192.168.1.20"}, Default: true, Allowed: true},
		{Name: "eth1", Addresses: []string{"203.0.113.5"}, PublicAddresses: []string{"203.0.113.5"}},
		{Name: "wg0", Addresses: []string{"10.8.0.2"}, VPN: true},
	}
	cur := setup.LANAccess{Interfaces: []string{"ens3", "gone0"}}
	// 选过 gone0，它现在不在：照旧可以保留。
	st, _, err := state.LoadOrNew(a.layout.StateFile(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	st.LANAccess = &state.LANAccess{Interfaces: cur.Interfaces}
	if err := state.Save(a.layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}

	// 预先勾上现在放行的；什么都不选时再问一次。
	p.many = [][]int{{}, {1, 4}}
	got, err := a.chooseLANAccess(ctx, cands, cur, lanChoice{})
	if err != nil || got == nil || !slices.Equal(got.Interfaces, []string{"eth1"}) || !got.Containers {
		t.Fatalf("picked = %+v, %v", got, err)
	}
	if len(p.asked) != 2 || len(p.asked[0]) != 5 || !slices.Equal(p.selected[0], []int{0, 3}) {
		t.Fatalf("asked %q, selected %v", p.asked, p.selected)
	}
	for k, want := range []string{"default route", "public address", "VPN", "down or gone", "Containers"} {
		if !strings.Contains(p.asked[0][k], want) {
			t.Errorf("option %d = %q, want %q", k, p.asked[0][k], want)
		}
	}
	if !strings.Contains(out.String(), "Choose at least one") {
		t.Errorf("output:\n%s", out)
	}

	// 参数给出时不问；also 加在选过的之后。
	yes := true
	if got, err := a.chooseLANAccess(ctx, cands, cur, lanChoice{interfaces: []string{"wg0"}, containers: &yes}); err != nil ||
		!slices.Equal(got.Interfaces, []string{"wg0"}) || !got.Containers {
		t.Errorf("flags = %+v, %v", got, err)
	}
	if got, err := a.chooseLANAccess(ctx, cands, cur, lanChoice{also: []string{"eth1"}}); err != nil ||
		!slices.Equal(got.Interfaces, []string{"ens3", "gone0", "eth1"}) {
		t.Errorf("also = %+v, %v", got, err)
	}

	// 网关的局域网网卡不列，选过时照旧保留；网关开着时什么都不选也可以。
	p.asked, p.selected = nil, nil
	gw := setup.LANAccess{Interfaces: []string{"ens4"}, Gateway: "ens4"}
	p.many = [][]int{{}}
	got, err = a.chooseLANAccess(ctx, append(cands, setup.LANInterface{Name: "ens4", Addresses: []string{"192.168.2.1"}, Gateway: true, Allowed: true}), gw, lanChoice{})
	if err != nil || !slices.Equal(got.Interfaces, []string{"ens4"}) || len(p.asked[0]) != 4 {
		t.Errorf("gateway = %+v, %v, asked %q", got, err, p.asked)
	}

	// 取消时什么都不返回。
	p.many = [][]int{nil}
	if got, err := a.chooseLANAccess(ctx, cands, cur, lanChoice{}); got != nil || err != nil || !a.aborted {
		t.Errorf("cancelled = %+v, %v", got, err)
	}

	// --yes 时不问，沿用选过的。
	a.opts.Yes = true
	if got, err := a.chooseLANAccess(ctx, cands, cur, lanChoice{}); err != nil || !slices.Equal(got.Interfaces, []string{"ens3", "gone0"}) {
		t.Errorf("--yes = %+v, %v", got, err)
	}
}

func TestThroughNote(t *testing.T) {
	a := &app{}
	if got := a.throughNote(&setup.LANAccess{Interfaces: []string{"ens3"}, Gateway: "ens4", Containers: true}); got != " (ens3, ens4, containers)" {
		t.Errorf("note = %q", got)
	}
	if got := a.throughNote(&setup.LANAccess{Interfaces: []string{}}); !strings.Contains(got, "nothing let in") {
		t.Errorf("empty = %q", got)
	}
}

func TestNewlyPublic(t *testing.T) {
	cands := []setup.LANInterface{
		{Name: "ens3", Addresses: []string{"203.0.113.5"}, PublicAddresses: []string{"203.0.113.5"}},
		{Name: "eth1", Addresses: []string{"198.51.100.7"}, PublicAddresses: []string{"198.51.100.7"}},
		{Name: "wg0", Addresses: []string{"10.8.0.2"}},
	}
	access := &state.LANAccess{Interfaces: []string{"ens3", "eth1", "wg0"}}
	cur := setup.LANAccess{Interfaces: []string{"ens3"}}
	names := func(list []setup.LANInterface) []string {
		var res []string
		for _, c := range list {
			res = append(res, c.Name)
		}
		return res
	}
	// 开关关着时放行的公网网卡都要确认；开着时只确认新加的。
	if got := names(newlyPublic(cands, cur, false, access)); !slices.Equal(got, []string{"ens3", "eth1"}) {
		t.Errorf("off: %q", got)
	}
	if got := names(newlyPublic(cands, cur, true, access)); !slices.Equal(got, []string{"eth1"}) {
		t.Errorf("on: %q", got)
	}
}
