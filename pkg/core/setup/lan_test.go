package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// fakeNIC 在 Root 下的 /sys/class/net 中造一块网卡：没有 lowers 时是硬件网卡（有 device），
// 否则是叠在 lowers 之上的网卡（网桥、bond 等，有 lower_<网卡>），lowers 为 nil 表示纯虚拟网卡。
func fakeNIC(t *testing.T, l paths.Layout, name string, hardware bool, lowers ...string) {
	t.Helper()
	dir := filepath.Join(l.NetDevices(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if hardware {
		os.MkdirAll(filepath.Join(dir, "device"), 0o755)
	}
	for _, lower := range lowers {
		os.MkdirAll(filepath.Join(dir, "lower_"+lower), 0o755)
	}
}

func TestPhysicalInterface(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	l := i.Layout
	fakeNIC(t, l, "enp1s0", true)
	fakeNIC(t, l, "bond0", false, "enp1s0")
	fakeNIC(t, l, "bond0.10", false, "bond0")
	fakeNIC(t, l, "vmbr0", false, "bond0.10", "tap100i0")
	fakeNIC(t, l, "tap100i0", false)
	fakeNIC(t, l, "veth9", false)
	fakeNIC(t, l, "docker0", false, "veth9")
	fakeNIC(t, l, "wg-lan", false)
	fakeNIC(t, l, "loop1", false, "loop2")
	fakeNIC(t, l, "loop2", false, "loop1")
	for name, want := range map[string]bool{
		"enp1s0": true, "bond0": true, "bond0.10": true, "vmbr0": true,
		"tap100i0": false, "veth9": false, "docker0": false, "wg-lan": false, "loop1": false, "missing": false, "../x": false,
	} {
		if got := i.physicalInterface(name); got != want {
			t.Errorf("physicalInterface(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLANInterfaces(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	i.InterfaceNames = func() ([]string, error) {
		return []string{"lo", "docker0", "eth1", "ens3", "tunlr0", "veth1", "wlan0", "eth2", "br0", "wg0"}, nil
	}
	for _, name := range []string{"eth1", "wlan0", "eth2", "eth9"} {
		fakeNIC(t, i.Layout, name, true)
	}
	fakeNIC(t, i.Layout, "br0", false, "eth9")
	fakeNIC(t, i.Layout, "docker0", false, "veth1")
	fakeNIC(t, i.Layout, "wg0", false)
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
	// ens3 是默认路由所在的网卡（见 fakeHost），sysfs 中没有它也是候选，排在最前；br0 接着硬件网卡；
	// wlan0 没有启用，eth2 没有 IPv4 地址；docker0、veth1、wg0 不接着真实网络。
	if len(got) != 3 || got[0].Name != "ens3" || !got[0].Default || got[1].Name != "br0" || got[2].Name != "eth1" ||
		len(got[2].PublicAddresses) != 1 || len(got[0].PublicAddresses) != 0 {
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
