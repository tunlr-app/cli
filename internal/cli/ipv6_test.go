package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/setup"
)

// TestIPv6TrafficOptions 验证选择流量这一层的选项：顺序为 rules、bypass，光标停在现在的设置上并标出。
func TestIPv6TrafficOptions(t *testing.T) {
	for _, traffic := range ipv6Traffic {
		opts, initial := ipv6TrafficOptions(&setup.IPv6Status{Traffic: traffic})
		if len(opts) != 2 || !strings.HasPrefix(opts[0], "By the rules:") || !strings.HasPrefix(opts[1], "Bypass:") {
			t.Fatalf("options %q", opts)
		}
		if ipv6Traffic[initial] != traffic || !strings.HasSuffix(opts[initial], " (now)") || strings.HasSuffix(opts[1-initial], " (now)") {
			t.Errorf("%s: initial %d, options %q", traffic, initial, opts)
		}
	}
}

// TestIPv6AnswerOptions 验证选择回答这一层的选项：顺序为 off、on、按配置；没有写定时光标停在“按配置”并写出现在的
// 设置，写定时停在它上面并标出；bypass 时标出留着的设置。
func TestIPv6AnswerOptions(t *testing.T) {
	pinnedOn := setup.DNSIPv6{On: true, Set: true, From: "90-dns.yaml", Files: []string{"90-dns.yaml"}}
	for _, c := range []struct {
		name    string
		s       setup.IPv6Status
		initial int
		marked  string
		decide  string
	}{
		{"fallback", setup.IPv6Status{Traffic: setup.IPv6Rules, Answers: setup.DNSIPv6{Fallback: true, From: "tunlr:defaults", Active: true}}, 2, "", "(now: off)"},
		{"pinned on", setup.IPv6Status{Traffic: setup.IPv6Rules, Answers: pinnedOn}, 1, "On:", ""},
		{"bypass keeps on", setup.IPv6Status{Traffic: setup.IPv6Bypass, Answers: pinnedOn}, 1, "On:", ""},
	} {
		opts, initial := ipv6AnswerOptions(&c.s)
		if len(opts) != 3 || !strings.HasPrefix(opts[0], "Off:") || !strings.HasPrefix(opts[1], "On:") {
			t.Fatalf("%s: options %q", c.name, opts)
		}
		if initial != c.initial {
			t.Errorf("%s: initial %d, want %d", c.name, initial, c.initial)
		}
		for i, o := range opts[:2] {
			if want := c.marked != "" && strings.HasPrefix(o, c.marked); strings.HasSuffix(o, " (now)") != want {
				t.Errorf("%s: option %d %q", c.name, i, o)
			}
		}
		if c.decide == "" && strings.Contains(opts[2], "(") || c.decide != "" && !strings.HasSuffix(opts[2], " "+c.decide) {
			t.Errorf("%s: %q", c.name, opts[2])
		}
	}
}

// TestPickIPv6 验证两步选择：选 bypass 时不问回答；选 rules 时再问回答，在第二步按 Esc 回到第一步；在第一步按 Esc
// 取消。
func TestPickIPv6(t *testing.T) {
	s := &setup.IPv6Status{Traffic: setup.IPv6Rules, Answers: setup.DNSIPv6{Fallback: true, Active: true}}
	for _, c := range []struct {
		name  string
		picks []string
		want  setup.IPv6Change
		ok    bool
		asked int
	}{
		{"bypass", []string{"Bypass:"}, setup.IPv6Change{Traffic: setup.IPv6Bypass}, true, 1},
		{"rules, on", []string{"By the rules:", "On:"}, setup.IPv6Change{Traffic: setup.IPv6Rules, Answers: setup.IPv6On}, true, 2},
		{"back, then bypass", []string{"By the rules:", "", "Bypass:"}, setup.IPv6Change{Traffic: setup.IPv6Bypass}, true, 3},
		{"cancelled", []string{""}, setup.IPv6Change{}, false, 1},
	} {
		p := &scriptedPrompt{t: t, picks: c.picks}
		a := &app{stdout: &bytes.Buffer{}, prompt: p}
		ch, ok, err := a.pickIPv6(context.Background(), s)
		if err != nil || ok != c.ok || ch != c.want || len(p.asked) != c.asked {
			t.Errorf("%s: %+v, %v, %v, asked %d", c.name, ch, ok, err, len(p.asked))
		}
	}
}

func TestDescribeIPv6(t *testing.T) {
	for want, s := range map[string]setup.IPv6Status{
		"rules, answers off":            {Traffic: setup.IPv6Rules, Answers: setup.DNSIPv6{Active: true}},
		"bypass; answers on, not used":  {Traffic: setup.IPv6Bypass, Answers: setup.DNSIPv6{On: true}},
		"bypass; answers off, not used": {Traffic: setup.IPv6Bypass},
	} {
		if got := describeIPv6(&s); got != want {
			t.Errorf("describeIPv6(%+v) = %q, want %q", s, got, want)
		}
	}
}
