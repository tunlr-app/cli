package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// TestDNSLeaks 验证可能的 DNS 泄露：tunlr 的 DNS 没有；mixin 改成明文直连时提示怎样改，改回而没有发布时提示
// 发布；TUN 开着时 IPv6 不经 TUN、网关所在的局域网有 IPv6、设备经网关上网却没有经它问 DNS。
func TestDNSLeaks(t *testing.T) {
	i, h := newGatewayInstaller(t, "1")
	ctx := context.Background()
	leaks := func() map[string]DNSLeak {
		t.Helper()
		st, err := state.Load(i.Layout.StateFile())
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]DNSLeak{}
		for _, l := range i.DNSLeaks(ctx, st) {
			m[l.ID] = l
		}
		return m
	}
	publish := func() {
		t.Helper()
		p := i.pipeline(nil)
		plan, err := p.Plan(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Apply(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	if got := leaks(); len(got) != 0 {
		t.Fatalf("tunlr's DNS: %+v", got)
	}

	mixin := filepath.Join(i.Layout.MixinDir(), "10-dns.yaml")
	writeFileIn(t, mixin, "mihomo:\n  dns:\n    nameserver: ['223.5.5.5#DIRECT', https://1.1.1.1/dns-query]\n")
	publish()
	l, ok := leaks()[LeakServers]
	if !ok || l.Message != "dns.nameserver has 1 plain server asked directly (223.5.5.5#DIRECT)" || !strings.Contains(l.Hint, "tunlr dns servers") {
		t.Errorf("plain server: %+v", l)
	}
	if err := os.Remove(mixin); err != nil {
		t.Fatal(err)
	}
	if l := leaks()[LeakServers]; !strings.HasPrefix(l.Hint, "tunlr config apply") {
		t.Errorf("fixed but not published: %+v", l)
	}
	publish()

	h.ipv6Route = "2001:4860:4860::8888 from :: via fe80::1 dev ens3 proto ra metric 100 pref medium\n"
	if l, ok := leaks()[LeakTunIPv6]; !ok || !strings.Contains(l.Message, "leaves through ens3, not tunlr0") {
		t.Errorf("IPv6 outside TUN: %+v", l)
	}
	h.ipv6Route = "2001:4860:4860::8888 from :: dev tunlr0 table 2022 src fdfe:dcba:9876::1 metric 1024 pref medium\n"
	if got := leaks(); len(got) != 0 {
		t.Errorf("IPv6 through TUN: %+v", got)
	}

	gatewayOn(t, i, "ens3")
	h.uses = map[string][]string{
		"dns_seen":     {"192.0.2.21 udp"},
		"input_seen":   {"192.0.2.21 tcp", "192.0.2.22 tcp"},
		"forward_seen": {"192.0.2.23 udp"},
	}
	l, ok = leaks()[LeakGatewayDevices]
	if !ok || !strings.HasPrefix(l.Message, "2 LAN devices used the gateway in the last 5 minutes but asked it no DNS (192.0.2.22, 192.0.2.23)") ||
		!strings.Contains(l.Hint, "this server (192.0.2.10)") {
		t.Errorf("devices without DNS: %+v", l)
	}
	if _, ok := leaks()[LeakGatewayIPv6]; ok {
		t.Error("IPv6 on a LAN without it")
	}
	i.Interfaces = func(name string) (*netfilter.Interface, error) {
		if name != "ens3" {
			return nil, errors.New("no such network interface")
		}
		return &netfilter.Interface{Name: name, Up: true, IPv4: []string{"192.0.2.10"}, IPv6: []string{"2001:db8::10"}}, nil
	}
	if l, ok := leaks()[LeakGatewayIPv6]; !ok || !strings.Contains(l.Message, "the LAN on ens3 has IPv6") {
		t.Errorf("IPv6 on the LAN: %+v", l)
	}
}
