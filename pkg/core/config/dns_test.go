package config

import (
	"slices"
	"testing"
)

func TestDNSWarnings(t *testing.T) {
	has := func(name string) bool { return name == "eth0" }
	for _, tt := range []struct {
		name, cfg string
		want      []string
	}{
		{"fine", "dns:\n  enable: true\n  nameserver: [dhcp://eth0, dhcp://system, 1.1.1.1]\n", nil},
		{"off", "dns:\n  enable: false\n", nil},
		{"empty", "dns:\n  enable: true\n  nameserver: []\n", []string{"dns.nameserver is empty: mihomo cannot resolve names"}},
		{"missing interface", "dns:\n  enable: true\n  nameserver: [1.1.1.1]\n  default-nameserver: [dhcp://en0]\n  nameserver-policy:\n    \"+.lan\": dhcp://en0#x\n",
			[]string{"DNS server dhcp://en0: this server has no network interface en0 (use a mixin to change the DNS servers)"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Inspect([]byte(tt.cfg))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, w := range DNSWarnings(f, has) {
				got = append(got, w.Message)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("warnings = %q, want %q", got, tt.want)
			}
		})
	}
}
