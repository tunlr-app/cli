package cli

import (
	"slices"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/setup"
)

func TestSwitchCommands(t *testing.T) {
	tests := []struct {
		s    setup.SwitchSuggestion
		want []string
	}{
		{setup.SwitchSuggestion{}, nil},
		{setup.SwitchSuggestion{Tun: true, PanelLAN: true}, []string{"tunlr tun on", "tunlr panel lan on"}},
		{setup.SwitchSuggestion{Gateway: true, Interface: "ens3", ProxyLAN: true}, []string{"tunlr tun gateway on --interface ens3", "tunlr proxy lan on"}},
		{setup.SwitchSuggestion{Gateway: true, Was: "eth9"}, []string{"tunlr tun gateway on"}},
		// 放行的入口：这台机器上也有的网卡，以及容器。
		{setup.SwitchSuggestion{ProxyLAN: true, PanelLAN: true, LANInterfaces: []string{"ens3", "wg0"}, LANContainers: true},
			[]string{"tunlr proxy lan on --interface ens3,wg0 --containers", "tunlr panel lan on --interface ens3,wg0 --containers"}},
		{setup.SwitchSuggestion{PanelLAN: true, LANWas: []string{"eth9"}}, []string{"tunlr panel lan on"}},
		{setup.SwitchSuggestion{Tun: true, IPv6Bypass: true}, []string{"tunlr tun on", "tunlr ipv6 bypass"}},
	}
	for _, tt := range tests {
		if got := switchCommands(tt.s); !slices.Equal(got, tt.want) {
			t.Errorf("%+v: got %q, want %q", tt.s, got, tt.want)
		}
	}
}

func TestLANThrough(t *testing.T) {
	for _, tt := range []struct {
		s    setup.SwitchSuggestion
		want string
	}{
		{setup.SwitchSuggestion{}, ""},
		{setup.SwitchSuggestion{LANInterfaces: []string{"ens3"}, LANContainers: true}, " (through ens3, containers)"},
		{setup.SwitchSuggestion{LANWas: []string{"eth9"}}, " (through interfaces without a public address; eth9 not found here)"},
	} {
		if got := lanThrough(tt.s); got != tt.want {
			t.Errorf("%+v: got %q, want %q", tt.s, got, tt.want)
		}
	}
}
