package cli

import (
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/setup"
)

// TestIPv6Options 验证选择 IPv6 的设置时的选项：顺序为 off、on、bypass、按配置；没有写定时光标停在“按配置”并写出
// 现在的设置，写定或 bypass 时停在它上面并标出；bypass 时“按配置”说明会结束 bypass。
func TestIPv6Options(t *testing.T) {
	for _, c := range []struct {
		name    string
		s       setup.IPv6Status
		initial int
		marked  string
		decide  string
	}{
		{"fallback", setup.IPv6Status{Mode: setup.IPv6Off, Answers: setup.DNSIPv6{Fallback: true, From: "tunlr:defaults"}}, 3, "", "(now: off)"},
		{"pinned on", setup.IPv6Status{Mode: setup.IPv6On, Answers: setup.DNSIPv6{On: true, Set: true, From: "90-dns.yaml", Files: []string{"90-dns.yaml"}}}, 1, "On:", ""},
		{"bypass", setup.IPv6Status{Mode: setup.IPv6Bypass}, 2, "Bypass:", "(ends bypass)"},
	} {
		opts, initial := ipv6Options(&c.s)
		if len(opts) != 4 || !strings.HasPrefix(opts[0], "Off:") || !strings.HasPrefix(opts[1], "On:") || !strings.HasPrefix(opts[2], "Bypass:") {
			t.Fatalf("%s: options %q", c.name, opts)
		}
		if initial != c.initial {
			t.Errorf("%s: initial %d, want %d", c.name, initial, c.initial)
		}
		for i, o := range opts[:3] {
			if want := c.marked != "" && strings.HasPrefix(o, c.marked); strings.HasSuffix(o, " (now)") != want {
				t.Errorf("%s: option %d %q", c.name, i, o)
			}
		}
		if c.decide == "" && strings.Contains(opts[3], "(") || c.decide != "" && !strings.HasSuffix(opts[3], " "+c.decide) {
			t.Errorf("%s: %q", c.name, opts[3])
		}
	}
}
