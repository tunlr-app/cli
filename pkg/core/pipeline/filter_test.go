package pipeline

import (
	"slices"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/state"
)

func TestParsePorts(t *testing.T) {
	for in, want := range map[string][]string{
		"443":                  {"443"},
		"443, 80/8000-9000 80": {"443", "80", "8000-9000"},
		"53-53,1-65535":        {"53", "1-65535"},
	} {
		if got, err := ParsePorts(in); err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", " , ", "0", "65536", "80-", "-80", "9000-8000", "http", "1.5"} {
		if got, err := ParsePorts(in); err == nil {
			t.Errorf("%q accepted: %v", in, got)
		}
	}
}

func TestFilterRules(t *testing.T) {
	if rules := filterRules(switches{}); rules != nil {
		t.Fatalf("no filters: %v", rules)
	}
	// 直连的设备在前，不受其他过滤项影响；QUIC 在端口之前。
	sw := switches{direct: []string{"192.168.1.9"}, filter: state.Filter{QUIC: true, Ports: true, PortList: []string{"80", "8000-9000"}}}
	want := []string{"SRC-IP-CIDR,192.168.1.9/32,DIRECT", "AND,((NETWORK,UDP),(DST-PORT,443)),REJECT", "NOT,((DST-PORT,80/8000-9000)),DIRECT"}
	if got := filterRules(sw); !slices.Equal(got, want) {
		t.Fatalf("rules = %q", got)
	}
	// 端口列表关闭时保留，不生成规则；没有自定义时用默认的列表。
	sw.filter = state.Filter{PortList: []string{"80"}}
	if got := filterRules(sw); len(got) != 1 {
		t.Fatalf("ports off: %q", got)
	}
	if got := filterRules(switches{filter: state.Filter{Ports: true}}); len(got) != 1 || got[0] != "NOT,((DST-PORT,22/80/143/194/443/465/587/853/993/995/5222/8080/8443)),DIRECT" {
		t.Fatalf("default ports: %q", got)
	}
}
