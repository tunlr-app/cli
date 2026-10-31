package config

import (
	"strings"
	"testing"
)

// TestDNSSources 验证 dns、sniffer 的来源：订阅写了的整个采用，否则是兜底值（最先提供它的那一层）；之后改过
// 其中的键、改动仍然生效的 mixin 文件按顺序列出，tunlr 的受管 mixin 不列；被删除的报告没有设置。
func TestDNSSources(t *testing.T) {
	tpl := []byte("mode: rule\n")
	fallbacks := Managed{Name: "tunlr:defaults", Default: true,
		Data: []byte("tunlr:\n  defaults:\n    dns:\n      enable: true\n      nameserver: [1.1.1.1]\n    sniffer:\n      enable: true\n")}
	listen := Managed{Name: "tunlr:dns", Default: true, Data: []byte("mihomo:\n  dns:\n    listen: 127.0.0.1:1053\n")}
	sources := func(l Layers) string {
		t.Helper()
		l.Template = tpl
		res, err := Compose(l)
		if err != nil {
			t.Fatal(err)
		}
		var s []string
		for _, src := range res.DNSSources {
			v := src.Key + "=" + src.From
			if src.Fallback {
				v += "*"
			}
			for _, f := range src.ChangedBy {
				v += "+" + f
			}
			if !src.Set {
				v += "!"
			}
			s = append(s, v)
		}
		return strings.Join(s, " ")
	}
	mixin := func(name, data string) Mixin { return Mixin{Name: name, Data: []byte(data)} }
	nameserver := mixin("10-dns.yaml", "mihomo:\n  dns:\n    nameserver: [9.9.9.9]\n")

	for _, c := range []struct {
		name string
		l    Layers
		want string
	}{
		{"fallbacks", Layers{Managed: []Managed{fallbacks, listen}}, "dns=tunlr:defaults* sniffer=tunlr:defaults*"},
		{"changed by a mixin", Layers{Managed: []Managed{fallbacks, listen}, Mixins: []Mixin{nameserver}},
			"dns=tunlr:defaults*+10-dns.yaml sniffer=tunlr:defaults*"},
		{"subscription", Layers{Managed: []Managed{fallbacks, listen}, Subscription: []byte("dns:\n  enable: true\n  nameserver: [8.8.8.8]\n"),
			Mixins: []Mixin{nameserver}}, "dns=subscription+10-dns.yaml sniffer=tunlr:defaults*"},
		{"removed", Layers{Managed: []Managed{fallbacks}, Mixins: []Mixin{mixin("10-a.yaml", "mihomo:\n  sniffer: null\n")}},
			"dns=tunlr:defaults* sniffer=tunlr:defaults*+10-a.yaml!"},
		// 后面的文件覆盖了前面的改动时只列后面的。
		{"overridden", Layers{Managed: []Managed{fallbacks}, Mixins: []Mixin{nameserver, mixin("20-b.yaml", "mihomo:\n  dns:\n    nameserver: [9.9.9.10]\n")}},
			"dns=tunlr:defaults*+20-b.yaml sniffer=tunlr:defaults*"},
		// mixin 的 tunlr.defaults 也算改动，订阅写了 dns 时它被替换，不算。
		{"mixin fallbacks", Layers{Managed: []Managed{fallbacks}, Mixins: []Mixin{mixin("05-f.yaml", "tunlr:\n  defaults:\n    dns:\n      ipv6: false\n")}},
			"dns=tunlr:defaults*+05-f.yaml sniffer=tunlr:defaults*"},
		{"mixin fallbacks replaced", Layers{Managed: []Managed{fallbacks}, Subscription: []byte("dns:\n  enable: true\n"),
			Mixins: []Mixin{mixin("05-f.yaml", "tunlr:\n  defaults:\n    dns:\n      ipv6: false\n")}}, "dns=subscription sniffer=tunlr:defaults*"},
		// 只有 mixin 的 tunlr.defaults 提供时它就是基础；哪里都没有时为空，mixin 加上的列为改动。
		{"only a mixin", Layers{Mixins: []Mixin{mixin("05-f.yaml", "tunlr:\n  defaults:\n    sniffer:\n      enable: true\n"), nameserver}},
			"dns=+10-dns.yaml sniffer=05-f.yaml*"},
		{"nothing", Layers{}, "dns=! sniffer=!"},
	} {
		if got := sources(c.l); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}
