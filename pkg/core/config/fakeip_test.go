package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestFakeIPFilterOps 验证 tunlr.fake-ip-filter：在最终生效的列表上追加（已有的不重复）、删除；改过的文件记入
// DNSSources，之后用 mihomo: 整份替换时记为覆盖；只认 append、delete。
func TestFakeIPFilterOps(t *testing.T) {
	tpl := []byte("mode: rule\n")
	fallbacks := Managed{Name: "tunlr:defaults", Default: true,
		Data: []byte("tunlr:\n  defaults:\n    dns:\n      enable: true\n      fake-ip-filter: [+.lan, time.*.com]\n")}
	compose := func(sub string, mixins ...Mixin) (*Result, error) {
		return Compose(Layers{Template: tpl, Subscription: []byte(sub), Managed: []Managed{fallbacks}, Mixins: append([]Mixin{useSubDNS}, mixins...)})
	}
	ours := Mixin{Name: "90-dns.yaml", Data: []byte("tunlr:\n  fake-ip-filter:\n    append: [nas.example.com, +.lan]\n    delete: [time.*.com, /^stun/]\n")}

	// 兜底值的列表。
	res, err := compose("mode: rule\n", ours)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(FakeIPFilter(res.Config), " "); got != "+.lan nas.example.com" {
		t.Errorf("fallback list: %s\n%s", got, res.Config)
	}
	if !slices.ContainsFunc(res.Warnings, func(w Warning) bool { return strings.Contains(w.Message, "nothing matches /^stun/") }) {
		t.Errorf("warnings: %v", res.Warnings)
	}
	for _, s := range res.DNSSources {
		if s.Key == "dns" && !slices.Equal(s.ChangedBy, []string{ours.Name}) {
			t.Errorf("dns source: %+v", s)
		}
	}

	// 订阅自己的列表；订阅的 dns 没有这个列表时新建。
	res, err = compose("dns:\n  enable: true\n  fake-ip-filter: [stun.example.com, +.local]\n", ours)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(FakeIPFilter(res.Config), " "); got != "+.local nas.example.com +.lan" {
		t.Errorf("subscription list: %s", got)
	}
	res, err = compose("dns:\n  enable: true\n", ours)
	if err != nil || strings.Join(FakeIPFilter(res.Config), " ") != "nas.example.com +.lan" {
		t.Errorf("no list: %v\n%s", err, res.Config)
	}

	// 之后的文件整份替换：条目丢失，记为覆盖。
	later := Mixin{Name: "95-dns.yaml", Data: []byte("mihomo:\n  dns:\n    fake-ip-filter: [+.home.arpa]\n")}
	res, err = compose("mode: rule\n", ours, later)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Overrides, func(o Override) bool {
		return o.Path == "dns.fake-ip-filter" && o.File == later.Name && o.Previous == ours.Name
	}) {
		t.Errorf("overrides: %+v", res.Overrides)
	}

	for _, c := range []struct{ body, want string }{
		{"prepend: [a]", "tunlr.fake-ip-filter.prepend: unknown operation (known: append, delete)"},
		{"append: a", "tunlr.fake-ip-filter.append: value must be a list"},
		{"append: [{name: a}]", "tunlr.fake-ip-filter.append: each item must be a domain pattern"},
	} {
		var me *MixinError
		data := []byte("tunlr:\n  fake-ip-filter:\n    " + c.body + "\n")
		_, err := compose("mode: rule\n", Mixin{Name: "10-a.yaml", Data: data})
		if !errors.As(err, &me) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.body, err)
		}
		if _, _, err := MixinFakeIPFilter("10-a.yaml", data); !errors.As(err, &me) {
			t.Errorf("MixinFakeIPFilter %s: %v", c.body, err)
		}
	}

	added, deleted, err := MixinFakeIPFilter(ours.Name, ours.Data)
	if err != nil || strings.Join(added, " ") != "nas.example.com +.lan" || strings.Join(deleted, " ") != "time.*.com /^stun/" {
		t.Errorf("MixinFakeIPFilter: %v %v %v", added, deleted, err)
	}
	if added, deleted, err := MixinFakeIPFilter("x.yaml", []byte("mihomo:\n  mode: rule\n")); added != nil || deleted != nil || err != nil {
		t.Errorf("none: %v %v %v", added, deleted, err)
	}
}
