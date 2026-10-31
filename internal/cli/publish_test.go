package cli

import (
	"testing"

	"github.com/tunlr-app/cli/pkg/core/config"
)

// TestDescribeDNSSources 验证 DNS 和嗅探来源的说明：说法相同的键合在一起，兜底值只有 tunlr:defaults 时不点名，
// 列出改过它的 mixin，删除了的说明被谁删除。
func TestDescribeDNSSources(t *testing.T) {
	for _, c := range []struct {
		sources []config.SectionSource
		want    string
	}{
		{[]config.SectionSource{{Key: "dns", From: "tunlr:defaults", Fallback: true, Set: true}, {Key: "sniffer", From: "tunlr:defaults", Fallback: true, Set: true}},
			"dns, sniffer from the fallbacks"},
		{[]config.SectionSource{{Key: "dns", From: config.FromSubscription, ChangedBy: []string{"10-dns.yaml", "home/20-x.yaml"}, Set: true},
			{Key: "sniffer", From: "tunlr:defaults", Fallback: true, ChangedBy: []string{"10-dns.yaml"}, Set: false}},
			"dns from the subscription, changed by 10-dns.yaml, home/20-x.yaml; sniffer removed by 10-dns.yaml"},
		{[]config.SectionSource{{Key: "dns", From: "05-f.yaml", Fallback: true, Set: true}, {Key: "sniffer", ChangedBy: []string{"10-s.yaml"}, Set: true}},
			"dns from the fallbacks in 05-f.yaml; sniffer from 10-s.yaml"},
		{[]config.SectionSource{{Key: "dns", From: config.FromSubscription, Set: true}, {Key: "sniffer"}},
			"dns from the subscription; sniffer not set"},
	} {
		if got := describeDNSSources(c.sources); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}
