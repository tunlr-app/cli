package cli

import (
	"bytes"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/setup"
)

// TestPrintProxyAddresses 验证代理地址的输出：只列放行的网卡上的地址，一个地址时写成一行，多个时每个地址
// 一行并注明网卡。
func TestPrintProxyAddresses(t *testing.T) {
	var out bytes.Buffer
	a := &app{stdout: &out}
	a.printProxyAddresses(&setup.ProxyLANPlan{Port: 7890, Interfaces: []setup.LANInterface{
		{Name: "ens3", Addresses: []string{"192.168.1.20"}, Default: true, Allowed: true},
		{Name: "wg0", Addresses: []string{"10.8.0.2"}, VPN: true},
	}})
	if want := "On devices, set the HTTP or SOCKS5 proxy to 192.168.1.20:7890\n" +
		"(server 192.168.1.20, port 7890; Wi-Fi settings → proxy → manual on phones).\n"; out.String() != want {
		t.Errorf("one address:\n%s", out.String())
	}

	out.Reset()
	a.printProxyAddresses(&setup.ProxyLANPlan{Port: 7890, Interfaces: []setup.LANInterface{
		{Name: "eth1", Addresses: []string{"192.168.2.1"}, Gateway: true, Allowed: true},
		{Name: "ens3", Addresses: []string{"10.0.0.5"}, Default: true, Allowed: true},
		{Name: "tailscale0", Addresses: []string{"100.100.1.2"}, SSH: true, Allowed: true},
	}})
	if want := "On devices, set the HTTP or SOCKS5 proxy to this server's address on their network:\n" +
		"  192.168.2.1:7890  eth1, gateway LAN\n" +
		"  10.0.0.5:7890     ens3, default route\n" +
		"  100.100.1.2:7890  tailscale0, this SSH session\n" +
		"(port 7890; Wi-Fi settings → proxy → manual on phones).\n"; out.String() != want {
		t.Errorf("several addresses:\n%s", out.String())
	}
}
