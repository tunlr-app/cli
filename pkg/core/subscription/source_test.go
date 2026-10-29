package subscription

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/state"
)

func TestParseSource(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.yaml")
	if err := os.WriteFile(file, []byte(sampleConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		in   string
		want Source
	}{
		{"https://x.example/sub?token=t", Source{URL: "https://x.example/sub?token=t"}},
		{"-", Source{Path: state.PathStdin}},
		{file, Source{Path: file}},
	} {
		if got, err := ParseSource(tt.in); err != nil || got != tt.want {
			t.Errorf("ParseSource(%q) = %+v, %v", tt.in, got, err)
		}
	}
	for _, in := range []string{"", "ftp://x.example/", filepath.Join(dir, "missing.yaml"), dir} {
		if _, err := ParseSource(in); !errors.Is(err, core.ErrInvalidArgument) {
			t.Errorf("ParseSource(%q) = %v, want an invalid argument", in, err)
		}
	}
}

// TestFileSource 覆盖本地文件订阅：读取、默认不自动更新、update 时重读、文件删掉后保留上次的内容、
// 不能带 User-Agent。
func TestFileSource(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "airport.yaml")
	if err := os.WriteFile(file, []byte(sampleConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(ctx, "home", file, AddOptions{UserAgent: "x"}); !errors.Is(err, core.ErrInvalidArgument) {
		t.Fatalf("user agent with a file: %v", err)
	}
	res, err := m.Add(ctx, "home", file, AddOptions{})
	if err != nil || res.Summary.Proxies != 1 {
		t.Fatalf("Add = %+v, %v", res, err)
	}
	entries, _ := m.List()
	s := entries[0].Subscription
	if s.URL != "" || s.Path != file || s.UpdatePeriod() != "off" || Describe(&s) != file || !NextUpdate(&s).IsZero() {
		t.Errorf("entry = %+v (period %s)", s, s.UpdatePeriod())
	}

	two := strings.Replace(sampleConfig, "proxies:\n", "proxies:\n  - {name: b, type: ss, server: b.example.net, port: 2, cipher: aes-128-gcm, password: p}\n", 1)
	if err := os.WriteFile(file, []byte(two), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := m.Update(ctx, nil, UpdateOptions{}); err != nil || res[0].Summary.Proxies != 2 {
		t.Fatalf("Update = %+v, %v", res, err)
	}
	os.Remove(file)
	res2, err := m.Update(ctx, []string{"home"}, UpdateOptions{})
	if err == nil || !strings.Contains(res2[0].Err.Error(), "read "+file) {
		t.Fatalf("missing file: %+v, %v", res2, err)
	}
	if data, _ := os.ReadFile(m.Layout.SubscriptionFile("home")); string(data) != two {
		t.Error("the last good copy was not kept")
	}
}

// TestStdinSource 覆盖从标准输入导入：不能自动更新，全部更新时跳过，只有 --source - 才再读一次。
func TestStdinSource(t *testing.T) {
	p := newProvider(t)
	m := newManager(t)
	ctx := context.Background()
	if _, err := m.Add(ctx, "home", "-", AddOptions{Update: "1d", Stdin: strings.NewReader(sampleConfig)}); !errors.Is(err, core.ErrInvalidArgument) {
		t.Fatalf("stdin with a period: %v", err)
	}
	if _, err := m.Add(ctx, "home", "-", AddOptions{Stdin: strings.NewReader(sampleConfig)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(ctx, "web", p.URL+"/ok", AddOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(ctx, "home", "6h"); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("Set on stdin: %v", err)
	}
	res, err := m.Update(ctx, nil, UpdateOptions{})
	if err != nil || len(res) != 1 || res[0].Name != "web" {
		t.Errorf("update all = %+v, %v", res, err)
	}
	if _, err := m.Update(ctx, []string{"home"}, UpdateOptions{}); !errors.Is(err, core.ErrInvalidArgument) || !strings.Contains(err.Error(), "--source -") {
		t.Errorf("update stdin = %v", err)
	}
	dash := "-"
	if _, err := m.Update(ctx, []string{"home"}, UpdateOptions{Source: &dash, Stdin: strings.NewReader(sampleConfig)}); err != nil {
		t.Errorf("update --source - = %v", err)
	}

	// 换成地址：读取成功才保存；失败时来源不变。
	bad := p.URL + "/missing"
	if _, err := m.Update(ctx, []string{"home"}, UpdateOptions{Source: &bad}); err == nil {
		t.Error("a broken source was accepted")
	}
	entries, _ := m.List()
	if entries[0].Path != state.PathStdin || entries[0].URL != "" || entries[0].LastError == "" {
		t.Errorf("after a failed --source: %+v", entries[0].Subscription)
	}
	good := p.URL + "/ok"
	if _, err := m.Update(ctx, []string{"home"}, UpdateOptions{Source: &good}); err != nil {
		t.Fatal(err)
	}
	entries, _ = m.List()
	if entries[0].Path != "" || entries[0].URL != good || entries[0].UpdatePeriod() != state.DefaultUpdate {
		t.Errorf("after --source URL: %+v", entries[0].Subscription)
	}
	if _, err := m.Update(ctx, nil, UpdateOptions{Source: &good}); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("--source without a name: %v", err)
	}
}

// TestAddFilter 检查 --include/--exclude 写入订阅 mixin，删除订阅时一并删除。
func TestAddFilter(t *testing.T) {
	p := newProvider(t)
	m := newManager(t)
	ctx := context.Background()
	if _, err := m.Add(ctx, "home", p.URL+"/ok", AddOptions{Filter: "("}); !errors.Is(err, core.ErrInvalidArgument) {
		t.Fatalf("bad regexp: %v", err)
	}
	if _, err := m.Add(ctx, "spare", p.URL+"/ok", AddOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(ctx, "home", p.URL+"/ok", AddOptions{Filter: "a`b", Exclude: "c"}); err != nil {
		t.Fatal(err)
	}
	mixins, err := config.LoadMixinsFor(m.Layout.MixinDir(), "home")
	if err != nil || len(mixins) != 1 || mixins[0].Name != "home/10-filter.yaml" ||
		!strings.Contains(string(mixins[0].Data), "filter: a`b") || !strings.Contains(string(mixins[0].Data), "exclude-filter: c") {
		t.Fatalf("mixins = %+v, %v", mixins, err)
	}
	if n, err := m.MixinFiles("home"); err != nil || n != 1 {
		t.Errorf("MixinFiles = %d, %v", n, err)
	}
	if _, err := m.Use(ctx, "spare"); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, "home"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.Layout.SubscriptionMixinDir("home")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the subscription's mixins were not removed: %v", err)
	}
}
