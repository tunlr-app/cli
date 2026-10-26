package config

import (
	"strings"
	"testing"
)

func TestInspectSubscription(t *testing.T) {
	sum, err := InspectSubscription([]byte(`
proxies:
  - {name: a, type: ss, server: a.example.net, port: 1, cipher: aes-128-gcm, password: p}
  - {name: b, type: ss, server: b.example.net, port: 2, cipher: aes-128-gcm, password: p}
proxy-providers:
  extra: {type: http, url: "https://example.net/p.yaml", path: ./p.yaml}
proxy-groups:
  - {name: g, type: select, proxies: [a, b]}
rules:
  - DOMAIN-SUFFIX,cn,DIRECT
  - MATCH,g
`))
	if err != nil {
		t.Fatal(err)
	}
	want := SubscriptionSummary{Proxies: 2, ProxyProviders: 1, ProxyGroups: 1, Rules: 2}
	if sum != want {
		t.Errorf("summary = %+v, want %+v", sum, want)
	}
}

func TestInspectSubscriptionErrors(t *testing.T) {
	tests := []struct{ name, data, want string }{
		{"empty", "", "subscription: empty"},
		{"node list", "c3M6Ly9ZV1Z6TFRJMU5pMW5ZMjA2QGV4YW1wbGUubmV0OjQ0Mw==\n", "try a different --user-agent"},
		{"no proxies", "rules:\n  - MATCH,DIRECT\n", "subscription: no proxies or proxy-providers"},
		{"html", "<html><body>login</body></html>\n", "try a different --user-agent"},
		{"broken yaml", "proxies: [\n", "subscription: yaml:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := InspectSubscription([]byte(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
