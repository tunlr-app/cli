package netfilter

import "testing"

func TestPrivateAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"192.168.1.5": true, "10.1.2.3": true, "172.31.255.1": true, "172.32.0.1": false,
		"100.100.1.1": true, "169.254.3.4": true, "::ffff:192.168.1.5": true,
		"fd7a:115c:a1e0::1": true, "fe80::1": true, "2001:db8::1": false,
		"8.8.8.8": false, "127.0.0.1": false, "::1": false, "lan": false,
	} {
		if got := PrivateAddress(addr); got != want {
			t.Errorf("PrivateAddress(%q) = %v, want %v", addr, got, want)
		}
	}
}
