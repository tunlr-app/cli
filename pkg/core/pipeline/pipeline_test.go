package pipeline

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/templates"
)

// fakeValidator 在配置含 MATCH,NOPE 时报告检查未通过，并记录校验次数。
type fakeValidator struct{ calls int }

func (v *fakeValidator) Validate(_ context.Context, cfg []byte) error {
	v.calls++
	if bytes.Contains(cfg, []byte("MATCH,NOPE")) {
		return core.ErrCheckFailed
	}
	return nil
}

// fakeReloader 记录每次加载时磁盘上的配置，fail 置位时加载失败。
type fakeReloader struct {
	layout paths.Layout
	fail   bool
	loaded []string
}

func (r *fakeReloader) Reload(context.Context) error {
	data, _ := os.ReadFile(r.layout.CoreConfig())
	r.loaded = append(r.loaded, string(data))
	if r.fail {
		r.fail = false // 只让新配置加载失败，恢复后的旧配置正常加载
		return errors.New("kernel rejected the config")
	}
	return nil
}

const subscriptionV1 = `proxies:
  - {name: a, type: ss, server: a.example.net, port: 1, cipher: aes-128-gcm, password: p}
rules:
  - MATCH,a
`

func newPipeline(t *testing.T) (*Pipeline, *fakeValidator, *fakeReloader) {
	l := paths.Layout{Root: t.TempDir()}
	v := &fakeValidator{}
	r := &fakeReloader{layout: l}
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	return &Pipeline{Layout: l, Version: "test", Validator: v, Reloader: r, Now: func() time.Time { return now }}, v, r
}

// useSubscription 让名为 name 的订阅成为启用的订阅，内容为 content。
func useSubscription(t *testing.T, l paths.Layout, name, content string) {
	t.Helper()
	st, _, err := state.LoadOrNew(l.StateFile(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if st.Find(name) == nil {
		st.Subscriptions = append(st.Subscriptions, state.Subscription{Name: name, URL: "https://example.net/sub"})
	}
	st.Active = name
	if err := state.Save(l.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	writeFile(t, l.SubscriptionFile(name), content)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustPlan(t *testing.T, p *Pipeline) *Plan {
	t.Helper()
	plan, err := p.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func mustApply(t *testing.T, p *Pipeline, plan *Plan) *config.Revision {
	t.Helper()
	rev, err := p.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

func TestFirstApply(t *testing.T) {
	p, v, r := newPipeline(t)

	plan := mustPlan(t, p)
	if plan.Current != nil || !plan.Changed() || plan.Diff != "" || plan.Subscription != "" || plan.Check != nil {
		t.Fatalf("first plan = %+v", plan)
	}
	if v.calls != 1 {
		t.Errorf("validator calls = %d", v.calls)
	}
	cfg := string(plan.Config)
	if !strings.Contains(cfg, "\nexternal-controller-unix: api.sock\n") {
		t.Errorf("enforced socket missing:\n%s", cfg)
	}
	if !strings.Contains(cfg, "\nsecret: "+plan.newSecret+"\n") || len(plan.newSecret) != 32 {
		t.Errorf("secret %q not in config", plan.newSecret)
	}
	// 只预览不保存：再预览一次会生成另一个 secret。
	if again := mustPlan(t, p); again.newSecret == plan.newSecret {
		t.Error("plan saved the secret")
	}

	rev := mustApply(t, p, plan)
	if rev == nil || rev.Number != 1 || rev.Reason != config.ReasonApply || rev.ToolVersion != "test" {
		t.Fatalf("revision = %+v", rev)
	}
	if got, _ := os.ReadFile(p.Layout.CoreConfig()); !bytes.Equal(got, plan.Config) {
		t.Error("config not written")
	}
	if fi, err := os.Stat(p.Layout.CoreDir()); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o750 {
		t.Errorf("core dir permissions = %v, want 0750 (it holds the API socket)", fi.Mode().Perm())
	}
	if len(r.loaded) != 1 {
		t.Errorf("reloads = %d", len(r.loaded))
	}
	st, err := state.Load(p.Layout.StateFile())
	if err != nil || st.APISecret != plan.newSecret {
		t.Fatalf("secret not saved: %v %v", st, err)
	}

	// secret 已保存，再次预览没有变化，也不重复校验。
	v.calls = 0
	plan = mustPlan(t, p)
	if plan.Changed() || plan.Check != nil || v.calls != 0 {
		t.Fatalf("second plan changed=%v check=%v calls=%d", plan.Changed(), plan.Check, v.calls)
	}
	if rev := mustApply(t, p, plan); rev != nil {
		t.Errorf("no-op apply recorded %+v", rev)
	}
}

func TestApplySubscriptionAndMixin(t *testing.T) {
	p, _, _ := newPipeline(t)
	mustApply(t, p, mustPlan(t, p))

	useSubscription(t, p.Layout, "main", "port: 1234\n"+subscriptionV1)
	writeFile(t, p.Layout.MixinFile(), "log-level: debug\nexternal-controller-unix: /tmp/x.sock\n")
	plan := mustPlan(t, p)
	if plan.Subscription != "main" || len(plan.Dropped) != 1 || plan.Dropped[0] != "port" {
		t.Fatalf("plan = %+v", plan)
	}
	for _, want := range []string{"-log-level: info", "+log-level: debug", "+  - MATCH,a"} {
		if !strings.Contains(plan.Diff, want+"\n") {
			t.Errorf("diff lacks %q:\n%s", want, plan.Diff)
		}
	}
	if strings.Contains(string(plan.Config), "/tmp/x.sock") {
		t.Error("mixin overrode an enforced key")
	}
	if rev := mustApply(t, p, plan); rev.Number != 2 || rev.Subscription != "main" {
		t.Errorf("revision = %+v", rev)
	}
}

func TestPlanRequiresSubscriptionContent(t *testing.T) {
	p, _, _ := newPipeline(t)
	useSubscription(t, p.Layout, "main", subscriptionV1)
	os.Remove(p.Layout.SubscriptionFile("main"))
	if _, err := p.Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "tunlr sub update main") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckFailedIsNotApplied(t *testing.T) {
	p, _, _ := newPipeline(t)
	useSubscription(t, p.Layout, "main", "rules:\n  - MATCH,NOPE\nproxies: []\n")
	plan := mustPlan(t, p)
	if !errors.Is(plan.Check, core.ErrCheckFailed) {
		t.Fatalf("check = %v", plan.Check)
	}
	if _, err := p.Apply(context.Background(), plan); !errors.Is(err, core.ErrCheckFailed) {
		t.Fatalf("apply err = %v", err)
	}
	if _, err := os.Stat(p.Layout.CoreConfig()); !errors.Is(err, os.ErrNotExist) {
		t.Error("failed check wrote the config")
	}
}

func TestApplyStale(t *testing.T) {
	p, _, _ := newPipeline(t)
	mustApply(t, p, mustPlan(t, p))
	useSubscription(t, p.Layout, "main", subscriptionV1)
	plan := mustPlan(t, p)
	writeFile(t, p.Layout.CoreConfig(), "edited: true\n")
	if _, err := p.Apply(context.Background(), plan); !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v", err)
	}
}

// TestApplyArchivesUnmanagedChanges 验证手工改过的生效配置在覆盖前存入历史。
func TestApplyArchivesUnmanagedChanges(t *testing.T) {
	p, _, _ := newPipeline(t)
	mustApply(t, p, mustPlan(t, p))
	writeFile(t, p.Layout.CoreConfig(), "edited: true\n")

	rev := mustApply(t, p, mustPlan(t, p))
	if rev.Number != 3 {
		t.Fatalf("revision = %+v", rev)
	}
	data, found, err := p.history().Read(2)
	if err != nil || found.Reason != config.ReasonFound || string(data) != "edited: true\n" {
		t.Fatalf("archived = %q %+v %v", data, found, err)
	}
}

func TestReloadFailureRollsBack(t *testing.T) {
	p, _, r := newPipeline(t)
	mustApply(t, p, mustPlan(t, p))
	before, _ := os.ReadFile(p.Layout.CoreConfig())

	useSubscription(t, p.Layout, "main", subscriptionV1)
	plan := mustPlan(t, p)
	r.fail = true
	_, err := p.Apply(context.Background(), plan)
	if !errors.Is(err, core.ErrRolledBack) || !strings.Contains(err.Error(), "kernel rejected the config") {
		t.Fatalf("err = %v", err)
	}
	if after, _ := os.ReadFile(p.Layout.CoreConfig()); !bytes.Equal(after, before) {
		t.Error("previous config not restored")
	}
	// 依次加载了：第一次发布、失败的新配置、恢复的旧配置。
	if len(r.loaded) != 3 || r.loaded[2] != string(before) {
		t.Errorf("reloads = %d", len(r.loaded))
	}
	revs, _, _ := p.Revisions()
	if len(revs) != 1 {
		t.Errorf("failed apply recorded in history: %+v", revs)
	}
}

func TestReloadFailureOnFirstApplyRemovesConfig(t *testing.T) {
	p, _, r := newPipeline(t)
	r.fail = true
	if _, err := p.Apply(context.Background(), mustPlan(t, p)); !errors.Is(err, core.ErrRolledBack) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(p.Layout.CoreConfig()); !errors.Is(err, os.ErrNotExist) {
		t.Error("config left behind")
	}
}

func TestRollback(t *testing.T) {
	p, _, _ := newPipeline(t)
	ctx := context.Background()
	if _, err := p.PlanRollback(ctx, 0); !errors.Is(err, ErrNothingToRollBack) {
		t.Fatalf("empty history: err = %v", err)
	}
	mustApply(t, p, mustPlan(t, p))
	if _, err := p.PlanRollback(ctx, 0); !errors.Is(err, ErrNothingToRollBack) {
		t.Fatalf("single revision: err = %v", err)
	}
	first, _ := os.ReadFile(p.Layout.CoreConfig())
	useSubscription(t, p.Layout, "main", subscriptionV1)
	mustApply(t, p, mustPlan(t, p))
	second, _ := os.ReadFile(p.Layout.CoreConfig())

	plan, err := p.PlanRollback(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != KindRollback || plan.RollbackTo != 1 || plan.Subscription != "" || !strings.Contains(plan.Diff, "-  - MATCH,a") {
		t.Fatalf("plan = %+v", plan)
	}
	rev := mustApply(t, p, plan)
	if rev.Number != 3 || rev.Reason != config.ReasonRollback || rev.RollbackOf != 1 {
		t.Fatalf("revision = %+v", rev)
	}
	if got, _ := os.ReadFile(p.Layout.CoreConfig()); !bytes.Equal(got, first) {
		t.Error("rollback did not restore revision 1")
	}

	// 再次回滚回到第二版。
	plan, err = p.PlanRollback(ctx, 0)
	if err != nil || plan.RollbackTo != 2 {
		t.Fatalf("second rollback: %+v %v", plan, err)
	}
	mustApply(t, p, plan)
	if got, _ := os.ReadFile(p.Layout.CoreConfig()); !bytes.Equal(got, second) {
		t.Error("rollback did not restore revision 2")
	}

	revs, current, err := p.Revisions()
	if err != nil || len(revs) != 4 || current != 4 {
		t.Fatalf("revisions: %d current %d %v", len(revs), current, err)
	}
	if _, err := p.PlanRollback(ctx, 9); !errors.Is(err, core.ErrInvalidArgument) || !errors.Is(err, config.ErrNoRevision) {
		t.Errorf("unknown revision: err = %v", err)
	}
}

func TestMirrorDefaults(t *testing.T) {
	p, _, _ := newPipeline(t)
	writeFile(t, p.Layout.SettingsFile(), "github-mirror: https://gh-proxy.com\n")
	cfg := string(mustPlan(t, p).Config)
	for _, want := range []string{
		"external-ui-url: https://gh-proxy.com/https://github.com/Zephyruso/zashboard/releases/latest/download/dist.zip\n",
		"  mmdb: https://gh-proxy.com/https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb\n",
		"  geosite: https://gh-proxy.com/https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geosite.dat\n",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q", want)
		}
	}
}

// TestTemplateUIURL 确认镜像改写的面板地址与模板中的一致。
func TestTemplateUIURL(t *testing.T) {
	if !bytes.Contains(templates.Config, []byte("\nexternal-ui-url: "+zashboardURL+"\n")) {
		t.Errorf("template external-ui-url differs from %s", zashboardURL)
	}
}
