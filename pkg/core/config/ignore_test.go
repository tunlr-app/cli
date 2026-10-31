package config

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestIgnoreFromSubscription 验证 tunlr.ignore-from-subscription：列出的键不从订阅采用，换成兜底值；各文件的
// 列表合在一起，报告的是最先列出的文件；订阅没写的键不报告；只能列出 IgnorableKeys。
func TestIgnoreFromSubscription(t *testing.T) {
	tpl := []byte("mode: rule\n")
	fallbacks := Managed{Name: "tunlr:defaults", Default: true,
		Data: []byte("tunlr:\n  defaults:\n    dns:\n      enable: true\n      nameserver: [1.1.1.1]\n    sniffer:\n      enable: true\n")}
	sub := []byte("dns:\n  enable: true\n  nameserver: [8.8.8.8]\nsniffer:\n  enable: false\nhosts:\n  a.example: 10.0.0.1\n")
	mixin := func(name, keys string) Mixin {
		return Mixin{Name: name, Data: []byte("tunlr:\n  ignore-from-subscription: " + keys + "\n")}
	}
	compose := func(mixins ...Mixin) *Result {
		t.Helper()
		res, err := Compose(Layers{Template: tpl, Subscription: sub, Managed: []Managed{fallbacks}, Mixins: mixins})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	ignored := func(res *Result) string {
		var s []string
		for _, k := range res.Ignored {
			s = append(s, k.Key+"="+k.File)
		}
		return strings.Join(s, " ")
	}

	res := compose(mixin("home/20-dns.yaml", "[dns, sniffer]"))
	if bytes.Contains(res.Config, []byte("8.8.8.8")) || !bytes.Contains(res.Config, []byte("1.1.1.1")) ||
		!bytes.Contains(res.Config, []byte("sniffer:\n  enable: true\n")) || !bytes.Contains(res.Config, []byte("a.example")) {
		t.Errorf("dns, sniffer ignored:\n%s", res.Config)
	}
	if got := ignored(res); got != "dns=home/20-dns.yaml sniffer=home/20-dns.yaml" {
		t.Errorf("ignored: %s", got)
	}
	var from []string
	for _, s := range res.DNSSources {
		from = append(from, s.Key+"="+s.From)
	}
	if got := strings.Join(from, " "); got != "dns=tunlr:defaults sniffer=tunlr:defaults" {
		t.Errorf("sources: %s", got)
	}

	// 合在一起：后面的文件不能取消前面的，null 什么都不列。
	res = compose(mixin("10-a.yaml", "[dns]"), mixin("20-b.yaml", "[dns, hosts]"), mixin("30-c.yaml", "null"))
	if got := ignored(res); got != "dns=10-a.yaml hosts=20-b.yaml" {
		t.Errorf("union: %s", got)
	}
	if bytes.Contains(res.Config, []byte("a.example")) || !bytes.Contains(res.Config, []byte("sniffer:\n  enable: false\n")) {
		t.Errorf("union:\n%s", res.Config)
	}

	// 订阅没写的键不报告。
	res, err := Compose(Layers{Template: tpl, Subscription: []byte("mode: rule\n"), Managed: []Managed{fallbacks}, Mixins: []Mixin{mixin("10-a.yaml", "[hosts]")}})
	if err != nil || len(res.Ignored) != 0 {
		t.Errorf("not in the subscription: %v, %v", res.Ignored, err)
	}

	for _, c := range []struct{ keys, want string }{
		{"[proxies]", "tunlr.ignore-from-subscription: proxies cannot be ignored; only dns, sniffer, hosts can"},
		{"dns", "tunlr.ignore-from-subscription must be a list of keys"},
	} {
		var me *MixinError
		_, err := Compose(Layers{Template: tpl, Subscription: sub, Mixins: []Mixin{mixin("10-a.yaml", c.keys)}})
		if !errors.As(err, &me) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.keys, err)
		}
		if _, err := MixinIgnores("10-a.yaml", fmt.Appendf(nil, "tunlr:\n  ignore-from-subscription: %s\n", c.keys)); !errors.As(err, &me) {
			t.Errorf("MixinIgnores %s: %v", c.keys, err)
		}
	}
	if keys, err := MixinIgnores("10-a.yaml", []byte("mihomo:\n  mode: rule\n")); keys != nil || err != nil {
		t.Errorf("none: %v, %v", keys, err)
	}
}

// TestSetTunlrKeys 验证修改 mixin 的 tunlr: 中的键，其他内容保留。
func TestSetTunlrKeys(t *testing.T) {
	out, err := SetTunlrKeys([]byte("mihomo:\n  mode: rule\n"), "head", map[string]any{"ignore-from-subscription": []string{"dns", "sniffer"}})
	if err != nil || string(out) != "mihomo:\n  mode: rule\ntunlr:\n  ignore-from-subscription:\n    - dns\n    - sniffer\n" {
		t.Fatalf("%v\n%s", err, out)
	}
	if keys, err := MixinIgnores("x.yaml", out); err != nil || strings.Join(keys, ",") != "dns,sniffer" {
		t.Errorf("read back: %v, %v", keys, err)
	}
	out, err = SetTunlrKeys(out, "", map[string]any{"ignore-from-subscription": nil})
	if err != nil || string(out) != "mihomo:\n  mode: rule\n" {
		t.Errorf("removed: %v\n%s", err, out)
	}
}
