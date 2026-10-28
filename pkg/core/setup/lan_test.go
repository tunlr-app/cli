package setup

import (
	"context"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/state"
)

func TestLANInterfaces(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	i.InterfaceNames = func() ([]string, error) {
		return []string{"lo", "docker0", "eth1", "ens3", "tunlr0", "veth1", "wlan0", "eth2"}, nil
	}
	i.Interfaces = func(name string) (*netfilter.Interface, error) {
		switch name {
		case "eth1":
			return &netfilter.Interface{Name: name, Up: true, IPv4: []string{"203.0.113.5"}}, nil
		case "wlan0":
			return &netfilter.Interface{Name: name, Up: false, IPv4: []string{"192.168.2.3"}}, nil
		case "eth2":
			return &netfilter.Interface{Name: name, Up: true}, nil
		}
		return lanInterface(name)
	}
	got, err := i.LANInterfaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// ens3 是默认路由所在的网卡（见 fakeHost），排在最前；wlan0 没有启用，eth2 没有 IPv4 地址。
	if len(got) != 2 || got[0].Name != "ens3" || !got[0].Default || got[1].Name != "eth1" ||
		len(got[1].PublicAddresses) != 1 || len(got[0].PublicAddresses) != 0 {
		t.Errorf("candidates = %+v", got)
	}
}

func TestLANConfigured(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	if ok, err := i.LANConfigured(); ok || err != nil {
		t.Fatalf("configured = %v, %v", ok, err)
	}
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	st.ProxyLAN = &state.ProxyLAN{Since: testNow}
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	if ok, err := i.LANConfigured(); !ok || err != nil {
		t.Errorf("configured = %v, %v", ok, err)
	}
}
