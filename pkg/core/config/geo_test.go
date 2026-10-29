package config

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDetectGeo(t *testing.T) {
	tests := []struct {
		name, cfg string
		want      GeoNeeds
	}{
		{"none", "rules:\n  - DOMAIN-SUFFIX,cn,DIRECT\n  - MATCH,DIRECT\n", GeoNeeds{}},
		{"geox-url read, not a need", "geox-url:\n  geoip: https://x/geoip.dat\n  geosite: https://x/geosite.dat\n  asn: https://x/GeoLite2-ASN.mmdb\ngeo-auto-update: true\n",
			GeoNeeds{URLs: map[string]string{"geoip": "https://x/geoip.dat", "geosite": "https://x/geosite.dat", "asn": "https://x/GeoLite2-ASN.mmdb"}}},
		{"geoip rule", "rules:\n  - GEOIP,CN,DIRECT\n", GeoNeeds{GeoIP: true}},
		{"src-geoip in logic rule", "rules:\n  - AND,((SRC-GEOIP,CN),(NETWORK,UDP)),DIRECT\n", GeoNeeds{GeoIP: true}},
		{"geosite in dns", "dns:\n  nameserver-policy:\n    \"geosite:cn\": 223.5.5.5\n", GeoNeeds{GeoSite: true}},
		{"fallback-filter", "dns:\n  fallback-filter:\n    geoip: true\n", GeoNeeds{GeoIP: true}},
		{"asn", "rules:\n  - IP-ASN,13335,DIRECT\n", GeoNeeds{ASN: true}},
		{"sub-rules", "sub-rules:\n  s:\n    - GEOSITE,google,PROXY\n", GeoNeeds{GeoSite: true}},
		{"geodata mode", "geodata-mode: true\nrules:\n  - GEOIP,CN,DIRECT\n", GeoNeeds{GeoIP: true, GeodataMode: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DetectGeo([]byte(tt.cfg))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestGeoSettings(t *testing.T) {
	cfg := "mode: rule\n" +
		"geodata-mode: true\n" +
		"geo-auto-update: True\n" +
		"geo-update-interval: 168\n" +
		"geox-url:\n  geoip: https://m.example/https://github.com/a/b/releases/download/v1/geoip.dat\n" +
		"rules:\n  - GEOIP,CN,DIRECT\n" +
		"dns:\n  enable: true\n"
	needs, update, err := GeoSettings([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	want := GeoNeeds{GeodataMode: true, URLs: map[string]string{"geoip": "https://m.example/https://github.com/a/b/releases/download/v1/geoip.dat"}}
	if !reflect.DeepEqual(needs, want) {
		t.Errorf("needs = %+v, want %+v", needs, want)
	}
	if update != (GeoUpdate{Auto: true, Interval: 168 * time.Hour}) {
		t.Errorf("update = %+v", update)
	}

	// 没有写时为内核的默认值：不自动更新，间隔 24 小时。
	needs, update, err = GeoSettings([]byte("mode: rule\nrules:\n  - MATCH,DIRECT\n"))
	if err != nil || needs.URLs != nil || update != (GeoUpdate{Interval: 24 * time.Hour}) {
		t.Errorf("defaults: %+v, %+v, %v", needs, update, err)
	}
}

// TestGeoSources 验证 tunlr.defaults 在订阅之前应用：订阅写了的顶层键整个替换它，只有 geox-url 按文件替换
// （订阅没写的文件用前面的地址，前面也没有的用内核的默认地址），mixin 的 mihomo: 覆盖一切；
// 各键的来源和是否为兜底值随之报告。
func TestGeoSources(t *testing.T) {
	tpl := []byte("geo-auto-update: true\ngeo-update-interval: 168\ngeox-url:\n  geosite: https://a/geosite.dat\n  mmdb: https://a/geoip.metadb\n")
	defaults := Mixin{Name: "05-fallback.yaml", Data: []byte("tunlr:\n  defaults:\n    geo-update-interval: 48\n    geox-url:\n      geosite: https://b/geosite.dat\n")}
	sources := func(l Layers) string {
		t.Helper()
		res, err := Compose(l)
		if err != nil {
			t.Fatal(err)
		}
		var s []string
		for _, ks := range res.GeoSources {
			f := ""
			if ks.Fallback {
				f = "*"
			}
			s = append(s, ks.Path+"="+ks.Value+"@"+ks.From+f)
		}
		return strings.Join(s, " ")
	}
	got := sources(Layers{Template: tpl, Mixins: []Mixin{defaults}})
	want := "geo-auto-update=true@template* geo-update-interval=48@05-fallback.yaml* geox-url.geoip=@template* " +
		"geox-url.geosite=https://b/geosite.dat@05-fallback.yaml* geox-url.mmdb=https://a/geoip.metadb@template* geox-url.asn=@template*"
	if got != want {
		t.Errorf("fallback only:\n got %s\nwant %s", got, want)
	}

	got = sources(Layers{Template: tpl,
		Subscription: []byte("geox-url:\n  geosite: https://s/geosite.dat\n"),
		Mixins:       []Mixin{defaults, {Name: "10-a.yaml", Data: []byte("mihomo:\n  geo-auto-update: false\n")}}})
	want = "geo-auto-update=false@10-a.yaml geo-update-interval=48@05-fallback.yaml* geox-url.geoip=@template* " +
		"geox-url.geosite=https://s/geosite.dat@subscription geox-url.mmdb=https://a/geoip.metadb@template* geox-url.asn=@template*"
	if got != want {
		t.Errorf("subscription and mixin:\n got %s\nwant %s", got, want)
	}

	// 订阅没写的文件用兜底的地址。
	withIP := Mixin{Name: "05-fallback.yaml", Data: []byte("tunlr:\n  defaults:\n    geox-url:\n      geoip: https://b/geoip.dat\n      geosite: https://b/geosite.dat\n")}
	got = sources(Layers{Template: tpl, Subscription: []byte("geox-url:\n  geosite: https://s/geosite.dat\n"), Mixins: []Mixin{withIP}})
	if !strings.Contains(got, " geox-url.geoip=https://b/geoip.dat@05-fallback.yaml* geox-url.geosite=https://s/geosite.dat@subscription ") {
		t.Errorf("filled from the fallbacks: %s", got)
	}

	// mihomo: 覆盖兜底值时记为覆盖。
	res, err := Compose(Layers{Template: tpl, Mixins: []Mixin{defaults, {Name: "10-a.yaml", Data: []byte("mihomo:\n  geo-update-interval: 12\n")}}})
	if err != nil || len(res.Overrides) != 1 || res.Overrides[0] != (Override{Path: "geo-update-interval", File: "10-a.yaml", Previous: "05-fallback.yaml"}) {
		t.Errorf("overrides = %+v, %v", res, err)
	}
}

// TestDefaults 验证 tunlr.defaults：订阅采用的顶层键整个替换它，没采用的保留；写了受管 mixin 独占的键时
// 不报错，独占的值优先；值必须是映射。
func TestDefaults(t *testing.T) {
	tpl := []byte("mode: rule\ndns:\n  enable: true\n  nameserver: [1.1.1.1]\n")
	mx := Mixin{Name: "05-fallback.yaml", Data: []byte("tunlr:\n  defaults:\n    dns:\n      nameserver: [9.9.9.9]\n    hosts:\n      a.example: 10.0.0.1\n")}
	res, err := Compose(Layers{Template: tpl, Mixins: []Mixin{mx}})
	if err != nil || !bytes.Contains(res.Config, []byte("  enable: true\n  nameserver: [9.9.9.9]\n")) || !bytes.Contains(res.Config, []byte("a.example: 10.0.0.1")) {
		t.Fatalf("without a subscription: %v\n%s", err, res.Config)
	}
	res, err = Compose(Layers{Template: tpl, Subscription: []byte("dns:\n  nameserver: [8.8.8.8]\n"), Mixins: []Mixin{mx}})
	if err != nil || bytes.Contains(res.Config, []byte("9.9.9.9")) || bytes.Contains(res.Config, []byte("enable: true")) || !bytes.Contains(res.Config, []byte("a.example")) {
		t.Fatalf("with a subscription: %v\n%s", err, res.Config)
	}

	owned := []Managed{{Name: "tunlr:x", Command: "tunlr x", Data: []byte("mihomo:\n  mode: rule\n")}}
	res, err = Compose(Layers{Template: tpl, Managed: owned, Mixins: []Mixin{{Name: "10-a.yaml", Data: []byte("tunlr:\n  defaults:\n    mode: global\n")}}})
	if err != nil || !bytes.Contains(res.Config, []byte("mode: rule\n")) {
		t.Errorf("owned key: %v\n%s", err, res.Config)
	}
	var me *MixinError
	_, err = Compose(Layers{Template: tpl, Mixins: []Mixin{{Name: "10-a.yaml", Data: []byte("tunlr:\n  defaults: [a]\n")}}})
	if !errors.As(err, &me) || !strings.Contains(err.Error(), "tunlr.defaults must be a mapping") {
		t.Errorf("not a mapping: %v", err)
	}
}

// TestSetMixinKeys 验证修改 mixin 的 mihomo: 中的键：保留其他内容，删空的上级随之删除，什么都不剩时返回 nil。
func TestSetMixinKeys(t *testing.T) {
	out, err := SetMixinKeys(nil, "head", map[string]any{"geo-update-interval": 24, "geox-url.geosite": "https://s/g.dat"})
	if err != nil {
		t.Fatal(err)
	}
	want := "# head\nmihomo:\n  geo-update-interval: 24\n  geox-url:\n    geosite: https://s/g.dat\n"
	if string(out) != want {
		t.Fatalf("new:\n%s", out)
	}
	out, err = SetMixinKeys(append(out, "tunlr:\n  rules:\n    prepend: [MATCH,DIRECT]\n"...), "", map[string]any{"geox-url.geosite": nil, "geo-update-interval": nil})
	if err != nil || strings.Contains(string(out), "mihomo") || !strings.Contains(string(out), "tunlr:") {
		t.Fatalf("removed: %v\n%s", err, out)
	}
	if out, err = SetMixinKeys([]byte("mihomo:\n  geo-auto-update: true\n"), "", map[string]any{"geo-auto-update": nil}); err != nil || out != nil {
		t.Errorf("emptied: %q, %v", out, err)
	}
}
