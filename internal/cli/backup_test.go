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
	}
	for _, tt := range tests {
		if got := switchCommands(tt.s); !slices.Equal(got, tt.want) {
			t.Errorf("%+v: got %q, want %q", tt.s, got, tt.want)
		}
	}
}
