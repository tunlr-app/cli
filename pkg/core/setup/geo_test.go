package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// TestSetGeo 验证按订阅修改 geo 数据设置：写进订阅的 geo mixin，或现在决定那个键的订阅 mixin；default 删除，
// 删空时删除文件；订阅不存在、地址不对时报错。
func TestSetGeo(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	ctx := context.Background()
	useSub(t, i, "home", "proxies: []\nrules:\n  - MATCH,DIRECT\ngeo-update-interval: 12\n")
	w, err := i.SetGeo(ctx, "home", GeoChange{Update: "2d", URLs: map[string]string{"geosite": "https://cdn.example/g.dat"}})
	if err != nil || !w.Active || strings.Join(w.Keys, " ") != "geo-auto-update geo-update-interval geox-url.geosite" {
		t.Fatalf("set: %+v, %v", w, err)
	}
	path := filepath.Join(i.Layout.MixinDir(), "home", GeoMixinFile)
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "mihomo:\n  geo-auto-update: true\n  geo-update-interval: 48\n  geox-url:\n    geosite: https://cdn.example/g.dat\n") {
		t.Fatalf("mixin: %v\n%s", err, data)
	}
	sources, err := i.GeoSources(ctx, "home")
	if err != nil || GeoPeriod(sources) != "2d" {
		t.Fatalf("period = %s, %v", GeoPeriod(sources), err)
	}
	for _, s := range sources {
		want := "home/" + GeoMixinFile
		if s.Path == "geox-url.mmdb" {
			want = pipeline.ManagedDefaults
		}
		if s.Path == "geox-url.mmdb" || s.Path == "geox-url.geosite" || s.Path == "geo-update-interval" {
			if s.From != want {
				t.Errorf("%s from %s, want %s", s.Path, s.From, want)
			}
		}
	}

	// 排在后面的订阅 mixin 写了的键改在那个文件里；default 从这个订阅的所有 mixin 中删除，共用的不动。
	late := filepath.Join(i.Layout.MixinDir(), "home", "50-late.yaml")
	writeFileIn(t, late, "# mine\nmihomo:\n  geo-update-interval: 6\n")
	shared := filepath.Join(i.Layout.MixinDir(), "10-shared.yaml")
	writeFileIn(t, shared, "mihomo:\n  geo-update-interval: 3\n")
	// geo-auto-update 在 20-geo.yaml 中已经是 true，只改 50-late.yaml。
	w, err = i.SetGeo(ctx, "home", GeoChange{Update: "1d"})
	if err != nil || len(w.Changes) != 1 || w.Changes[0].Name != "home/50-late.yaml" {
		t.Fatalf("later mixin: %+v, %v", w, err)
	}
	if got := string(mustRead(t, late)); !strings.Contains(got, "# mine") || !strings.Contains(got, "geo-update-interval: 24") {
		t.Errorf("late:\n%s", got)
	}
	if sources, _ := i.GeoSources(ctx, "home"); GeoPeriod(sources) != "1d" {
		t.Errorf("period = %s", GeoPeriod(sources))
	}
	if _, err := i.SetGeo(ctx, "home", GeoChange{Update: GeoDefault, URLs: map[string]string{"geosite": GeoDefault}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("emptied mixin not removed: %v", err)
	}
	if _, err := os.Stat(late); !os.IsNotExist(err) {
		t.Errorf("emptied late mixin not removed: %v", err)
	}
	if string(mustRead(t, shared)) != "mihomo:\n  geo-update-interval: 3\n" {
		t.Error("shared mixin changed")
	}
	if _, err := i.SetGeo(ctx, "nope", GeoChange{Update: "1d"}); err == nil {
		t.Error("unknown subscription accepted")
	}
	if _, err := i.SetGeo(ctx, "home", GeoChange{URLs: map[string]string{"geoip": "ftp://x/"}}); err == nil {
		t.Error("ftp URL accepted")
	}
}

// useSub 让名为 name 的订阅成为启用的订阅，内容为 content。
func useSub(t *testing.T, i *Installer, name, content string) {
	t.Helper()
	l := &subscription.List{Active: name, Subscriptions: []subscription.Def{{Name: name, URL: "https://example.net/" + name}}}
	if err := subscription.SaveList(i.Layout.SubscriptionList(), l); err != nil {
		t.Fatal(err)
	}
	writeFileIn(t, i.Layout.SubscriptionFile(name), content)
}

func writeFileIn(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
