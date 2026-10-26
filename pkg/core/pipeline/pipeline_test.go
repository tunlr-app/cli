package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
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

// fakeReloader 记录每次加载时磁盘上的配置。fail 置位时加载失败；
// stopped 置位时报告内核没有运行；crash 置位时加载失败且内核随之退出。
type fakeReloader struct {
	layout  paths.Layout
	fail    bool
	stopped bool
	crash   bool
	loaded  []string
}

func (r *fakeReloader) Reload(context.Context) error {
	if r.stopped {
		return mihomo.ErrNotRunning
	}
	data, _ := os.ReadFile(r.layout.CoreConfig())
	r.loaded = append(r.loaded, string(data))
	if r.crash {
		r.crash, r.stopped = false, true
		return errors.New("mihomo stopped responding after loading the configuration")
	}
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
	// 默认不能访问 GitHub：安装面板失败，不影响发布。
	gh := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(gh.Close)
	return &Pipeline{
		Layout: l, Version: "test", Validator: v, Reloader: r,
		Download: &download.Client{APIBase: gh.URL},
		Now:      func() time.Time { return now },
	}, v, r
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

// mustApply 发布 plan，返回记录下的修订；没有变化时返回 nil。
func mustApply(t *testing.T, p *Pipeline, plan *Plan) *config.Revision {
	t.Helper()
	res, err := p.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		return nil
	}
	if !res.Reloaded {
		t.Error("not reloaded")
	}
	return res.Revision
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
	dir := p.Layout.MixinDir()
	writeFile(t, filepath.Join(dir, "20-b.yaml"), "log-level: debug\n")
	writeFile(t, filepath.Join(dir, "10-a.yaml"), "log-level: warning\nexternal-controller-unix: /tmp/x.sock\n")
	writeFile(t, filepath.Join(dir, "notes.txt"), "log-level: error\n")
	plan := mustPlan(t, p)
	if plan.Subscription != "main" || len(plan.Dropped) != 1 || plan.Dropped[0] != "port" {
		t.Fatalf("plan = %+v", plan)
	}
	if strings.Join(plan.Mixins, ",") != "10-a.yaml,20-b.yaml" {
		t.Errorf("mixins = %v", plan.Mixins)
	}
	if len(plan.Overrides) != 1 || plan.Overrides[0] != (config.Override{Path: "log-level", File: "20-b.yaml", Previous: "10-a.yaml"}) {
		t.Errorf("overrides = %+v", plan.Overrides)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].String() != "10-a.yaml:2: external-controller-unix is managed by tunlr; the value in the mixin is ignored" {
		t.Errorf("warnings = %+v", plan.Warnings)
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

// TestPlanWithMixin 验证编辑中的 mixin 代替磁盘上的同名文件，新文件按文件名顺序加入。
func TestPlanWithMixin(t *testing.T) {
	p, _, _ := newPipeline(t)
	dir := p.Layout.MixinDir()
	writeFile(t, filepath.Join(dir, "10-a.yaml"), "log-level: warning\n")
	writeFile(t, filepath.Join(dir, "30-c.yaml"), "mode: global\n")

	plan, err := p.PlanWithMixin(context.Background(), config.Mixin{Name: "10-a.yaml", Data: []byte("log-level: debug\n")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan.Config), "\nlog-level: debug\n") || strings.Join(plan.Mixins, ",") != "10-a.yaml,30-c.yaml" {
		t.Errorf("replace: mixins %v\n%s", plan.Mixins, plan.Config)
	}

	plan, err = p.PlanWithMixin(context.Background(), config.Mixin{Name: "20-b.yaml", Data: []byte("mode: direct\n")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan.Config), "\nmode: global\n") || strings.Join(plan.Mixins, ",") != "10-a.yaml,20-b.yaml,30-c.yaml" {
		t.Errorf("insert: mixins %v\n%s", plan.Mixins, plan.Config)
	}
}

// TestMixinErrorIsCheckFailure 验证有错的 mixin 不能发布，按“检查未通过”报告并指出文件。
func TestMixinErrorIsCheckFailure(t *testing.T) {
	p, _, _ := newPipeline(t)
	writeFile(t, filepath.Join(p.Layout.MixinDir(), "50-rules.yaml"), "rules: x\nprepend-rules: [MATCH,DIRECT]\n")
	_, err := p.Plan(context.Background())
	var me *config.MixinError
	if !errors.As(err, &me) || !errors.Is(err, core.ErrCheckFailed) || err.Error() != "mixin 50-rules.yaml:2: prepend-rules: rules is not a list" {
		t.Fatalf("err = %v", err)
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

// TestKernelNotRunning 验证内核没有运行时照常发布，只是标明未重新加载。
func TestKernelNotRunning(t *testing.T) {
	p, _, r := newPipeline(t)
	r.stopped = true
	res, err := p.Apply(context.Background(), mustPlan(t, p))
	if err != nil {
		t.Fatal(err)
	}
	if res.Reloaded || res.Revision.Number != 1 {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Stat(p.Layout.CoreConfig()); err != nil {
		t.Errorf("config not written: %v", err)
	}
}

// TestKernelCrashOnReload 验证内核在加载新配置时退出的，恢复原配置（内核下次启动时加载它）。
func TestKernelCrashOnReload(t *testing.T) {
	p, _, r := newPipeline(t)
	mustApply(t, p, mustPlan(t, p))
	before, _ := os.ReadFile(p.Layout.CoreConfig())
	useSubscription(t, p.Layout, "main", subscriptionV1)
	r.crash = true
	_, err := p.Apply(context.Background(), mustPlan(t, p))
	if !errors.Is(err, core.ErrRolledBack) || !strings.Contains(err.Error(), "stopped responding") {
		t.Fatalf("err = %v", err)
	}
	if after, _ := os.ReadFile(p.Layout.CoreConfig()); !bytes.Equal(after, before) {
		t.Error("previous config not restored")
	}
}

// fakeRestarter 记录每次重启时磁盘上的配置。fail 置位时第一次重启失败；stopped 置位时报告
// 服务没有运行（ifRunning 时）。
type fakeRestarter struct {
	layout    paths.Layout
	fail      bool
	stopped   bool
	restarted []string
	forced    int
}

func (r *fakeRestarter) RestartLocked(_ context.Context, ifRunning bool) error {
	if ifRunning && r.stopped {
		return mihomo.ErrNotRunning
	}
	if !ifRunning {
		r.forced++
	}
	data, _ := os.ReadFile(r.layout.CoreConfig())
	r.restarted = append(r.restarted, string(data))
	if r.fail {
		r.fail = false
		return mihomo.ErrUnhealthy
	}
	return nil
}

func TestApplyRestartsForControllerChanges(t *testing.T) {
	p, _, r := newPipeline(t)
	rs := &fakeRestarter{layout: p.Layout}
	p.Restarter = rs
	mustApply(t, p, mustPlan(t, p))
	if len(rs.restarted) != 0 {
		t.Fatal("first apply restarted mihomo")
	}

	// 只改订阅：热重载。
	useSubscription(t, p.Layout, "main", subscriptionV1)
	if plan := mustPlan(t, p); len(plan.Restart) != 0 {
		t.Fatalf("restart keys for a subscription change = %v", plan.Restart)
	}
	mustApply(t, p, mustPlan(t, p))
	reloads := len(r.loaded)

	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-api.yaml"), "secret: changed\nexternal-controller: 127.0.0.1:9091\n")
	plan := mustPlan(t, p)
	if !slices.Equal(plan.Restart, []string{"external-controller", "secret"}) {
		t.Fatalf("restart keys = %v", plan.Restart)
	}
	res, err := p.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Restarted || res.Reloaded || len(rs.restarted) != 1 || len(r.loaded) != reloads {
		t.Fatalf("result = %+v, restarts %d, reloads %d", res, len(rs.restarted), len(r.loaded)-reloads)
	}
}

func TestRestartFailureRollsBack(t *testing.T) {
	p, _, _ := newPipeline(t)
	rs := &fakeRestarter{layout: p.Layout}
	p.Restarter = rs
	mustApply(t, p, mustPlan(t, p))
	before, _ := os.ReadFile(p.Layout.CoreConfig())

	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-api.yaml"), "secret: changed\n")
	rs.fail = true
	_, err := p.Apply(context.Background(), mustPlan(t, p))
	if !errors.Is(err, core.ErrRolledBack) || !errors.Is(err, mihomo.ErrUnhealthy) {
		t.Fatalf("err = %v", err)
	}
	if after, _ := os.ReadFile(p.Layout.CoreConfig()); !bytes.Equal(after, before) {
		t.Error("previous config not restored")
	}
	// 恢复原配置后不论服务状态都再次重启。
	if len(rs.restarted) != 2 || rs.restarted[1] != string(before) || rs.forced != 1 {
		t.Errorf("restarts = %d, forced = %d", len(rs.restarted), rs.forced)
	}
}

func TestRestartKeysWithoutService(t *testing.T) {
	p, _, r := newPipeline(t)
	p.Restarter = &fakeRestarter{layout: p.Layout, stopped: true}
	mustApply(t, p, mustPlan(t, p))
	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-api.yaml"), "secret: changed\n")
	res, err := p.Apply(context.Background(), mustPlan(t, p))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reloaded || res.Restarted || !slices.Equal(res.RestartNeeded, []string{"secret"}) || len(r.loaded) != 2 {
		t.Fatalf("result = %+v", res)
	}
}

// TestApplyAfterConfigRemoved 验证生效的配置被删除（如卸载后重新 init）后再发布同样的内容时，
// 沿用最新的修订而不是重复记录。
func TestApplyAfterConfigRemoved(t *testing.T) {
	p, _, _ := newPipeline(t)
	useSubscription(t, p.Layout, "main", subscriptionV1)
	first := mustApply(t, p, mustPlan(t, p))
	if err := os.Remove(p.Layout.CoreConfig()); err != nil {
		t.Fatal(err)
	}
	again := mustApply(t, p, mustPlan(t, p))
	if again.Number != first.Number {
		t.Fatalf("revision = %d, want %d", again.Number, first.Number)
	}
	if revs, _, _ := p.Revisions(); len(revs) != 1 {
		t.Fatalf("revisions = %+v", revs)
	}
}

func TestPlanSummarizesProxies(t *testing.T) {
	p, _, _ := newPipeline(t)
	useSubscription(t, p.Layout, "main", subscriptionV1)
	mustApply(t, p, mustPlan(t, p))
	useSubscription(t, p.Layout, "main", strings.Replace(subscriptionV1, "port: 1,", "port: 2,", 1))
	plan := mustPlan(t, p)
	if plan.Proxies == nil || plan.Proxies.Changed != 1 || plan.Proxies.Fields["port"] != 1 {
		t.Fatalf("proxies = %+v", plan.Proxies)
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

	// 工具生成的调整应用在用户的 mixin 之前，用户可以覆盖，覆盖记录在计划中。
	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-a.yaml"), "secret: mine\nexternal-ui-url: https://example.com/ui.zip\n")
	plan := mustPlan(t, p)
	if cfg := string(plan.Config); !strings.Contains(cfg, "\nsecret: mine\n") || !strings.Contains(cfg, "\nexternal-ui-url: https://example.com/ui.zip\n") {
		t.Errorf("mixin did not override the generated values:\n%s", cfg)
	}
	if fmt.Sprint(plan.Overrides) != "[{secret 10-a.yaml tunlr} {external-ui-url 10-a.yaml tunlr}]" {
		t.Errorf("overrides = %v", plan.Overrides)
	}
}

// TestTemplateURLs 确认镜像改写的面板和 geo 数据地址与模板中的一致。
func TestTemplateURLs(t *testing.T) {
	if !bytes.Contains(templates.Config, []byte("\nexternal-ui-url: "+mihomo.ZashboardURL+"\n")) {
		t.Errorf("template external-ui-url differs from %s", mihomo.ZashboardURL)
	}
	for _, f := range []mihomo.GeoFile{mihomo.GeoIPDat, mihomo.GeoSite, mihomo.GeoMMDB, mihomo.GeoASN} {
		if !bytes.Contains(templates.Config, []byte(": "+f.URL()+"\n")) {
			t.Errorf("template geox-url lacks %s", f.URL())
		}
	}
	if len(mihomo.GeoxURL("https://m.example/")) != 4 {
		t.Error("GeoxURL does not cover the four geo files in the template")
	}
}

// TestMirrorRules 验证配置了 GitHub 镜像时，镜像主机的直连规则插在订阅规则前面，用户可以在 mixin 中去掉。
func TestMirrorRules(t *testing.T) {
	p, _, _ := newPipeline(t)
	useSubscription(t, p.Layout, "main", subscriptionV1)
	writeFile(t, p.Layout.SettingsFile(), "github-mirror: https://gh-proxy.com/\n")
	plan := mustPlan(t, p)
	if strings.Join(plan.Generated.PrependRules, ",") != "DOMAIN,gh-proxy.com,DIRECT" {
		t.Errorf("generated rules = %v", plan.Generated.PrependRules)
	}
	if !strings.Contains(string(plan.Config), "\nrules:\n  - DOMAIN,gh-proxy.com,DIRECT\n  - MATCH,a\n") {
		t.Errorf("config:\n%s", plan.Config)
	}

	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-a.yaml"), "delete-rules:\n  - DOMAIN,gh-proxy.com,DIRECT\n")
	if cfg := string(mustPlan(t, p).Config); strings.Contains(cfg, "gh-proxy.com,DIRECT") {
		t.Errorf("mixin could not remove the rule:\n%s", cfg)
	}

	for mirror, want := range map[string]string{
		"":                       "",
		"http://192.0.2.7:8080/": "IP-CIDR,192.0.2.7/32,DIRECT,no-resolve",
		"https://[2001:db8::1]/": "IP-CIDR6,2001:db8::1/128,DIRECT,no-resolve",
	} {
		if got := strings.Join(mirrorRules(mirror), ","); got != want {
			t.Errorf("mirrorRules(%q) = %q, want %q", mirror, got, want)
		}
	}
}

// TestApplyInstallsUI 验证发布时下载并校验面板，已有面板时不再下载；下载失败不影响发布。
func TestApplyInstallsUI(t *testing.T) {
	p, _, _ := newPipeline(t)
	res, err := p.Apply(context.Background(), mustPlan(t, p))
	if err != nil || res.UIInstalled != "" || res.UIError == nil || !strings.Contains(res.UIError.Error(), "install the panel") {
		t.Fatalf("unreachable GitHub: %+v, %v", res, err)
	}

	archive := zipOf(t, map[string]string{"dist/index.html": "<html>"})
	p.Download = fakeRelease(t, "Zephyruso/zashboard", "dist.zip", archive)
	useSubscription(t, p.Layout, "main", subscriptionV1)
	res, err = p.Apply(context.Background(), mustPlan(t, p))
	want := filepath.Join(p.Layout.CoreDir(), "ui", "zashboard")
	if err != nil || res.UIInstalled != want || res.UIError != nil {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(want, "index.html")); string(got) != "<html>" {
		t.Errorf("index.html = %q", got)
	}

	// 用户在 mixin 中换了面板：交给内核。
	os.RemoveAll(want)
	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-ui.yaml"), "external-ui-url: https://example.net/other.zip\n")
	res, err = p.Apply(context.Background(), mustPlan(t, p))
	if err != nil || res.UIInstalled != "" || res.UIError != nil {
		t.Fatalf("other panel: %+v, %v", res, err)
	}
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeRelease 模拟 repo 最新发布中的一个资产，返回指向它的下载客户端。
func fakeRelease(t *testing.T, repo, name string, data []byte) *download.Client {
	sum := sha256.Sum256(data)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + repo + "/releases/latest":
			json.NewEncoder(w).Encode(map[string]any{"assets": []download.Asset{{
				Name: name, Size: int64(len(data)), URL: srv.URL + "/" + name,
				Digest: "sha256:" + hex.EncodeToString(sum[:]),
			}}})
		case "/" + name:
			w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &download.Client{APIBase: srv.URL}
}
