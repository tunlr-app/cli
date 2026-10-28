package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/setup"
)

func TestAskUsage(t *testing.T) {
	ens3 := setup.LANInterface{Name: "ens3", Addresses: []string{"192.168.1.20"}, Default: true}
	eth1 := setup.LANInterface{Name: "eth1", Addresses: []string{"203.0.113.5"}, PublicAddresses: []string{"203.0.113.5"}}
	tests := []struct {
		name     string
		cands    []setup.LANInterface
		picks    []string
		confirms []bool
		want     lanUsage
		steps    int
	}{
		{name: "this server only", cands: []setup.LANInterface{ens3}, picks: []string{"Only"}},
		{name: "proxy", cands: []setup.LANInterface{ens3}, picks: []string{"Proxy", "No"},
			want: lanUsage{proxy: true}, steps: 1},
		{name: "proxy and panel", cands: []setup.LANInterface{ens3}, picks: []string{"Proxy", "Yes"},
			want: lanUsage{proxy: true, panel: true}, steps: 2},
		// 只有一个候选时不问网卡。
		{name: "gateway", cands: []setup.LANInterface{ens3}, picks: []string{"Gateway", "Yes"},
			want: lanUsage{proxy: true, gateway: "ens3", panel: true}, steps: 4},
		{name: "gateway choose interface", cands: []setup.LANInterface{ens3, eth1}, picks: []string{"Gateway", "ens3", "No"},
			want: lanUsage{proxy: true, gateway: "ens3"}, steps: 3},
		// 有公网地址的网卡要再确认；回答否时回到用途的问题。
		{name: "public declined", cands: []setup.LANInterface{eth1}, picks: []string{"Proxy", "Only"}, confirms: []bool{false}},
		{name: "public accepted", cands: []setup.LANInterface{eth1}, picks: []string{"Gateway", "No"}, confirms: []bool{true},
			want: lanUsage{proxy: true, gateway: "eth1"}, steps: 3},
		// 没有候选时不能选网关；局域网代理仍然可以（地址留给 proxy lan on 去找）。
		{name: "no interface", picks: []string{"Gateway", "Proxy", "No"}, want: lanUsage{proxy: true}, steps: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &scriptedPrompt{t: t, picks: tt.picks, confirms: tt.confirms}
			a := &app{stdout: &bytes.Buffer{}, prompt: p}
			var u lanUsage
			if err := a.askUsage(context.Background(), tt.cands, &u); err != nil {
				t.Fatal(err)
			}
			if u != tt.want {
				t.Errorf("usage = %+v, want %+v", u, tt.want)
			}
			if len(p.picks) != 0 || len(p.confirms) != 0 {
				t.Errorf("unused answers: %q %v", p.picks, p.confirms)
			}
			if got := u.steps(); len(got) != tt.steps {
				t.Errorf("steps = %q", got)
			}
			// 网卡列表中标出默认路由和公网地址。
			for _, o := range p.asked {
				if strings.HasPrefix(o[0], "ens3") && !slices.ContainsFunc(o, func(s string) bool { return strings.Contains(s, "public address") }) {
					t.Errorf("interface options %q", o)
				}
			}
		})
	}
}
