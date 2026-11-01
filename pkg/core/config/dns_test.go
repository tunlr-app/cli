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

// TestDNSLeaks 验证找出以明文直连询问的服务器：加密的、带节点名的、respect-rules 下的不算（system、dhcp 除外），
// 只含中国、局域网域名的 policy 项和只解析节点的设置不算，DNS 关着算。
func TestDNSLeaks(t *testing.T) {
	for _, tt := range []struct {
		name, cfg string
		want      string
		off       bool
	}{
		{"tunlr defaults", "dns:\n  enable: true\n  respect-rules: true\n  default-nameserver: [223.5.5.5]\n" +
			"  proxy-server-nameserver: [223.5.5.5]\n  direct-nameserver: [223.5.5.5]\n" +
			"  nameserver: [https://1.1.1.1/dns-query]\n  nameserver-policy:\n    geosite:cn: [https://223.5.5.5/dns-query#DIRECT]\n" +
			"    \"+.lan,+.home.arpa\": system\n    \"geosite:private,geolocation-cn\": 192.168.1.1\n    rule-set:cn_domain: 223.5.5.5\n", "", false},
		{"airport", "dns:\n  enable: true\n  nameserver: [223.5.5.5, 'udp://119.29.29.29:53', tls://1.1.1.1, '8.8.8.8#Proxy', '9.9.9.9#ecs=1.2.3.0/24']\n" +
			"  fallback: tcp://8.8.4.4#DIRECT\n",
			"dns.nameserver has 3 plain servers asked directly (223.5.5.5, udp://119.29.29.29:53, 9.9.9.9#ecs=1.2.3.0/24); " +
				"dns.fallback has 1 plain server asked directly (tcp://8.8.4.4#DIRECT)", false},
		{"respect-rules", "dns:\n  enable: true\n  respect-rules: true\n  nameserver: [8.8.8.8, system, dhcp://eth0, '1.1.1.1#DIRECT', rcode://success]\n",
			"dns.nameserver has 3 plain servers asked directly (system, dhcp://eth0, 1.1.1.1#DIRECT)", false},
		{"policy", "dns:\n  enable: true\n  nameserver: [https://1.1.1.1/dns-query]\n  nameserver-policy:\n" +
			"    \"+.google.com\": 8.8.8.8\n    \"geosite:geolocation-!cn\": [1.1.1.1]\n    \"+.lan,+.google.com\": system\n",
			`dns.nameserver-policy["+.google.com"] has 1 plain server asked directly (8.8.8.8); ` +
				`dns.nameserver-policy["+.lan,+.google.com"] has 1 plain server asked directly (system); ` +
				`dns.nameserver-policy["geosite:geolocation-!cn"] has 1 plain server asked directly (1.1.1.1)`, false},
		{"off", "dns:\n  enable: false\n  nameserver: [223.5.5.5]\n", "", true},
		{"missing", "mode: rule\n", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plain, off := DNSLeaks([]byte(tt.cfg))
			if off != tt.off {
				t.Fatalf("off = %v", off)
			}
			if off {
				if len(plain) != 0 || DNSLeakText(plain, off) == "" {
					t.Errorf("plain = %v, text %q", plain, DNSLeakText(plain, off))
				}
				return
			}
			if got := DNSLeakText(plain, off); got != tt.want {
				t.Errorf("text = %q\nwant   %q", got, tt.want)
			}
		})
	}
}
