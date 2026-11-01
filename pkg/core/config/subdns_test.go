package config

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// useSubDNS 是采用订阅的 DNS 的 mixin，测试订阅的 dns 怎样合成时用。
var useSubDNS = Mixin{Name: "01-sub-dns.yaml", Data: []byte("tunlr:\n  sub-dns: true\n")}

// TestSubDNS 验证订阅的 dns、sniffer 默认不采用（用兜底值，报告在 UnusedDNS 中），tunlr.sub-dns: true 时采用；
// 后面的文件压过前面的；订阅没写的键不报告；值只能是 true、false。
func TestSubDNS(t *testing.T) {
	tpl := []byte("mode: rule\n")
	fallbacks := Managed{Name: "tunlr:defaults", Default: true,
		Data: []byte("tunlr:\n  defaults:\n    dns:\n      enable: true\n      nameserver: [1.1.1.1]\n    sniffer:\n      enable: true\n")}
	sub := []byte("dns:\n  enable: true\n  nameserver: [8.8.8.8]\nsniffer:\n  enable: false\nhosts:\n  a.example: 10.0.0.1\n")
	mixin := func(name, v string) Mixin {
		return Mixin{Name: name, Data: []byte("tunlr:\n  sub-dns: " + v + "\n")}
	}
	compose := func(mixins ...Mixin) *Result {
		t.Helper()
		res, err := Compose(Layers{Template: tpl, Subscription: sub, Managed: []Managed{fallbacks}, Mixins: mixins})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// 默认：订阅的 dns、sniffer 不采用，hosts 照样采用。
	res := compose()
	if bytes.Contains(res.Config, []byte("8.8.8.8")) || !bytes.Contains(res.Config, []byte("1.1.1.1")) ||
		!bytes.Contains(res.Config, []byte("sniffer:\n  enable: true\n")) || !bytes.Contains(res.Config, []byte("a.example")) {
		t.Errorf("default:\n%s", res.Config)
	}
	if res.SubDNS != "" || strings.Join(res.UnusedDNS, " ") != "dns sniffer" {
		t.Errorf("default: sub-dns %q, unused %v", res.SubDNS, res.UnusedDNS)
	}
	var from []string
	for _, s := range res.DNSSources {
		from = append(from, s.Key+"="+s.From)
	}
	if got := strings.Join(from, " "); got != "dns=tunlr:defaults sniffer=tunlr:defaults" {
		t.Errorf("sources: %s", got)
	}

	res = compose(mixin("home/20-dns.yaml", "true"))
	if !bytes.Contains(res.Config, []byte("8.8.8.8")) || !bytes.Contains(res.Config, []byte("sniffer:\n  enable: false\n")) ||
		res.SubDNS != "home/20-dns.yaml" || len(res.UnusedDNS) != 0 {
		t.Errorf("sub-dns: %q %v\n%s", res.SubDNS, res.UnusedDNS, res.Config)
	}

	// 后面的文件压过前面的：订阅自己的 false 压过共用的 true，null 也是。
	if res := compose(mixin("10-a.yaml", "true"), mixin("home/20-dns.yaml", "false")); res.SubDNS != "" || len(res.UnusedDNS) != 2 {
		t.Errorf("false after true: %q %v", res.SubDNS, res.UnusedDNS)
	}
	if res := compose(mixin("10-a.yaml", "true"), mixin("20-b.yaml", "null")); res.SubDNS != "" {
		t.Errorf("null after true: %q", res.SubDNS)
	}

	// 订阅没写的键不报告。
	res, err := Compose(Layers{Template: tpl, Subscription: []byte("mode: rule\n"), Managed: []Managed{fallbacks}})
	if err != nil || len(res.UnusedDNS) != 0 {
		t.Errorf("not in the subscription: %v, %v", res.UnusedDNS, err)
	}

	var me *MixinError
	if _, err := Compose(Layers{Template: tpl, Subscription: sub, Mixins: []Mixin{mixin("10-a.yaml", "[dns]")}}); !errors.As(err, &me) ||
		!strings.Contains(err.Error(), "tunlr.sub-dns must be true or false") {
		t.Errorf("not a boolean: %v", err)
	}
	if _, _, err := MixinSubDNS("10-a.yaml", []byte("tunlr:\n  sub-dns: yes please\n")); !errors.As(err, &me) {
		t.Errorf("MixinSubDNS: %v", err)
	}
	for data, want := range map[string][2]bool{"tunlr:\n  sub-dns: true\n": {true, true}, "tunlr:\n  sub-dns: false\n": {true, false},
		"mihomo:\n  mode: rule\n": {false, false}} {
		if set, on, err := MixinSubDNS("x.yaml", []byte(data)); err != nil || [2]bool{set, on} != want {
			t.Errorf("MixinSubDNS(%q) = %v %v, %v", data, set, on, err)
		}
	}
}

// TestSetTunlrKeys 验证修改 mixin 的 tunlr: 中的键，其他内容保留。
func TestSetTunlrKeys(t *testing.T) {
	out, err := SetTunlrKeys([]byte("mihomo:\n  mode: rule\n"), "head", map[string]any{"sub-dns": true})
	if err != nil || string(out) != "mihomo:\n  mode: rule\ntunlr:\n  sub-dns: true\n" {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err = SetTunlrKeys(out, "", map[string]any{"sub-dns": nil})
	if err != nil || string(out) != "mihomo:\n  mode: rule\n" {
		t.Errorf("removed: %v\n%s", err, out)
	}
}

// TestKeySource 验证按键查来源：兜底值、订阅、mixin（含写了上级映射的），以及 MixinSets。
func TestKeySource(t *testing.T) {
	fallbacks := Managed{Name: "tunlr:defaults", Default: true,
		Data: []byte("tunlr:\n  defaults:\n    dns:\n      enhanced-mode: fake-ip\n      nameserver: [1.1.1.1]\n")}
	res, err := Compose(Layers{Template: []byte("mode: rule\n"), Managed: []Managed{fallbacks}, Mixins: []Mixin{
		{Name: "10-a.yaml", Data: []byte("mihomo:\n  dns:\n    nameserver: [9.9.9.9]\n")},
		{Name: "20-b.yaml", Data: []byte("mihomo:\n  sniffer: null\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]KeySource{
		"dns.enhanced-mode": {Path: "dns.enhanced-mode", Value: "fake-ip", From: "tunlr:defaults", Fallback: true},
		"dns.nameserver":    {Path: "dns.nameserver", From: "10-a.yaml"},
		"sniffer.enable":    {Path: "sniffer.enable", From: "20-b.yaml"},
		"dns.listen":        {Path: "dns.listen", From: FromTemplate, Fallback: true},
	} {
		if got := res.KeySource(path); got != want {
			t.Errorf("%s: %+v, want %+v", path, got, want)
		}
	}
	if !MixinSets([]byte("mihomo:\n  dns:\n    enhanced-mode: null\n"), "dns.enhanced-mode") ||
		MixinSets([]byte("tunlr:\n  defaults:\n    dns:\n      enhanced-mode: fake-ip\n"), "dns.enhanced-mode") {
		t.Error("MixinSets")
	}
	china, other := DNSServers([]byte("dns:\n  nameserver: tls://9.9.9.9\n  nameserver-policy:\n    \"geosite:cn\": [223.5.5.5#DIRECT]\n"))
	if !slices.Equal(china, []string{"223.5.5.5#DIRECT"}) || !slices.Equal(other, []string{"tls://9.9.9.9"}) {
		t.Errorf("DNSServers: %v %v", china, other)
	}

	// redir-host 需要而订阅的 DNS 常常没有的设置；DNS 的开关。
	airport := []byte("dns:\n  enable: true\n  nameserver: [223.5.5.5]\n")
	if got := DNSLacks(airport); !slices.Equal(got, []string{"respect-rules", "nameserver-policy"}) || !DNSEnabled(airport) {
		t.Errorf("DNSLacks = %v, enabled %v", got, DNSEnabled(airport))
	}
	full := []byte("dns:\n  respect-rules: true\n  nameserver-policy:\n    geosite:cn: [223.5.5.5]\n")
	if got := DNSLacks(full); len(got) != 0 || DNSEnabled(full) {
		t.Errorf("DNSLacks = %v, enabled %v", got, DNSEnabled(full))
	}
}
