package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/config"
)

// brokenReloader 让重新加载配置失败，发布因此回滚。
type brokenReloader struct{}

func (brokenReloader) Reload(context.Context) error { return errors.New("reload refused") }

// TestFakeIPFilter 验证你加进 fake-ip-filter 的条目：写成 tunlr.fake-ip-filter.append，加在兜底的列表上，已有的不加；
// 文件中手写的 delete 保留；删空时删除文件；发布失败时文件恢复原状；之后整份替换列表的 mixin 被找出来。
func TestFakeIPFilter(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	ctx := context.Background()
	path := filepath.Join(i.Layout.MixinDir(), DNSMixinFile)

	f, err := i.FakeIPFilter(ctx)
	if err != nil || len(f.Entries) != 0 || len(f.Base) < 10 || f.Base[0] != "+.lan" {
		t.Fatalf("before: %+v, %v", f, err)
	}
	e, plan, err := i.PlanFakeIPFilter(ctx, []string{"nas.example.com", "+.lan"})
	if err != nil || !e.Changed() || strings.Join(e.Entries, " ") != "nas.example.com" || strings.Join(e.Duplicates, " ") != "+.lan" || !plan.Changed() {
		t.Fatalf("plan: %+v, %v", e, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("written before saving: %v", err)
	}
	if rev, err := i.SaveFakeIPFilter(ctx, e, plan); err != nil || rev == nil {
		t.Fatalf("save: %v, %v", rev, err)
	}
	cfg := mustRead(t, i.Layout.CoreConfig())
	if got := config.FakeIPFilter(cfg); got[len(got)-1] != "nas.example.com" {
		t.Errorf("published: %v", got)
	}
	if s, err := i.TunStatus(ctx); err != nil || strings.Join(s.FakeIPFilter, " ") != "nas.example.com" {
		t.Errorf("status: %+v, %v", s, err)
	}

	// 手写的 delete 保留；发布失败时文件恢复原状。
	writeFileIn(t, path, "tunlr:\n  fake-ip-filter:\n    append: [nas.example.com]\n    delete: [+.lan]\n")
	before := mustRead(t, path)
	e, plan, err = i.PlanFakeIPFilter(ctx, []string{"nas.example.com", "printer.example.com"})
	if err != nil || strings.Join(e.Before, " ") != "nas.example.com" {
		t.Fatalf("plan: %+v, %v", e, err)
	}
	i.Reloader = brokenReloader{}
	if _, err := i.SaveFakeIPFilter(ctx, e, plan); err == nil {
		t.Fatal("failed publish reported success")
	}
	if got := mustRead(t, path); string(got) != string(before) {
		t.Errorf("not restored:\n%s", got)
	}
	i.Reloader = &fakeReloader{layout: i.Layout}

	// 删空：delete 还在，文件留下；再去掉 delete 后删空时删除文件。
	e, plan, err = i.PlanFakeIPFilter(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.SaveFakeIPFilter(ctx, e, plan); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, path)); strings.Contains(got, "append") || !strings.Contains(got, "delete: [+.lan]") {
		t.Errorf("delete kept:\n%s", got)
	}
	writeFileIn(t, path, "tunlr:\n  fake-ip-filter:\n    append: [nas.example.com]\n")
	e, plan, err = i.PlanFakeIPFilter(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.SaveFakeIPFilter(ctx, e, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("emptied mixin not removed: %v", err)
	}

	// 之后的 mixin 整份替换列表：加入的条目不生效，找出是哪个文件。
	writeFileIn(t, filepath.Join(i.Layout.MixinDir(), "95-dns.yaml"), "mihomo:\n  dns:\n    fake-ip-filter: [+.home.arpa]\n")
	e, _, err = i.PlanFakeIPFilter(ctx, []string{"nas.example.com"})
	if err != nil || strings.Join(e.Lost, " ") != "nas.example.com" || e.LostTo != "95-dns.yaml" {
		t.Errorf("lost: %+v, %v", e, err)
	}

	if _, err := ParseFakeIPFilter("+.lan\n\n  nas.example.com \n+.lan\n"); err != nil {
		t.Error(err)
	}
	if got, _ := ParseFakeIPFilter("+.lan\n\n  nas.example.com \n+.lan\n"); strings.Join(got, " ") != "+.lan nas.example.com" {
		t.Errorf("parsed: %q", got)
	}
	if _, err := ParseFakeIPFilter("a.example b.example\n"); err == nil {
		t.Error("two names on a line accepted")
	}
}
