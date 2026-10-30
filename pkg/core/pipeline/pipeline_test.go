package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
	"github.com/tunlr-app/cli/templates"
)

// fakeValidator 在配置含 MATCH,NOPE 时报告检查未通过，并记录校验次数。download 不为空时，校验时调用它，
// 模拟 mihomo -t 下载 geo 文件。
type fakeValidator struct {
	calls    int
	download func(cfg []byte)
}

func (v *fakeValidator) Validate(_ context.Context, cfg []byte, _ *config.GeoNeeds) error {
	v.calls++
	if v.download != nil {
		v.download(cfg)
	}
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
	list, err := subscription.LoadList(l.SubscriptionList())
	if err != nil {
		t.Fatal(err)
	}
	if list.Find(name) == nil {
		list.Subscriptions = append(list.Subscriptions, subscription.Def{Name: name, URL: "https://example.net/sub"})
	}
	list.Active = name
	if err := subscription.SaveList(l.SubscriptionList(), list); err != nil {
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

func mixinNames(mixins []config.Mixin) []string {
	var names []string
	for _, m := range mixins {
		names = append(names, m.Name)
	}
	return names
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
	writeFile(t, filepath.Join(dir, "20-b.yaml"), "mihomo:\n  log-level: debug\n")
	writeFile(t, filepath.Join(dir, "10-a.yaml"), "mihomo:\n  log-level: warning\n")
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
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings = %+v", plan.Warnings)
	}
	for _, want := range []string{"-log-level: info", "+log-level: debug", "+  - MATCH,a"} {
		if !strings.Contains(plan.Diff, want+"\n") {
			t.Errorf("diff lacks %q:\n%s", want, plan.Diff)
		}
	}
	if rev := mustApply(t, p, plan); rev.Number != 2 || rev.Subscription != "main" {
		t.Errorf("revision = %+v", rev)
	}
}

// TestPlanWithMixin 验证编辑中的 mixin 代替磁盘上的同名文件，新文件按文件名顺序加入。
func TestPlanWithMixin(t *testing.T) {
	p, _, _ := newPipeline(t)
	dir := p.Layout.MixinDir()
	writeFile(t, filepath.Join(dir, "10-a.yaml"), "mihomo:\n  log-level: warning\n")
	writeFile(t, filepath.Join(dir, "30-c.yaml"), "mihomo:\n  mode: global\n")

	plan, err := p.PlanWithMixin(context.Background(), config.Mixin{Name: "10-a.yaml", Data: []byte("mihomo:\n  log-level: debug\n")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan.Config), "\nlog-level: debug\n") || strings.Join(plan.Mixins, ",") != "10-a.yaml,30-c.yaml" {
		t.Errorf("replace: mixins %v\n%s", plan.Mixins, plan.Config)
	}

	plan, err = p.PlanWithMixin(context.Background(), config.Mixin{Name: "20-b.yaml", Data: []byte("mihomo:\n  mode: direct\n")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan.Config), "\nmode: global\n") || strings.Join(plan.Mixins, ",") != "10-a.yaml,20-b.yaml,30-c.yaml" {
		t.Errorf("insert: mixins %v\n%s", plan.Mixins, plan.Config)
	}
}

// TestSubscriptionMixins 验证订阅 mixin 只在它的订阅启用时应用、排在共用的之后；编辑没有启用的订阅的
// mixin 时按它被启用合成，计划不能发布；订阅不存在时报错。
func TestSubscriptionMixins(t *testing.T) {
	p, _, _ := newPipeline(t)
	ctx := context.Background()
	useSubscription(t, p.Layout, "spare", subscriptionV1)
	useSubscription(t, p.Layout, "main", subscriptionV1)
	dir := p.Layout.MixinDir()
	writeFile(t, filepath.Join(dir, "10-a.yaml"), "mihomo:\n  log-level: warning\n")
	writeFile(t, filepath.Join(dir, "main", "10-x.yaml"), "mihomo:\n  log-level: debug\n")
	writeFile(t, filepath.Join(dir, "spare", "10-x.yaml"), "mihomo:\n  log-level: error\n")

	plan := mustPlan(t, p)
	if strings.Join(plan.Mixins, ",") != "10-a.yaml,main/10-x.yaml" || !strings.Contains(string(plan.Config), "\nlog-level: debug\n") {
		t.Errorf("mixins %v\n%s", plan.Mixins, plan.Config)
	}
	if len(plan.Overrides) != 1 || plan.Overrides[0] != (config.Override{Path: "log-level", File: "main/10-x.yaml", Previous: "10-a.yaml"}) {
		t.Errorf("overrides = %+v", plan.Overrides)
	}

	// 编辑共用的和启用订阅的：照常是可以发布的计划；新文件插在所在那一段的正确位置。
	plan, err := p.PlanWithMixin(ctx, config.Mixin{Name: "main/05-y.yaml", Data: []byte("mihomo:\n  mode: global\n")})
	if err != nil || plan.Inactive || strings.Join(plan.Mixins, ",") != "10-a.yaml,main/05-y.yaml,main/10-x.yaml" {
		t.Errorf("active: %v %v %v", plan.Mixins, plan.Inactive, err)
	}
	plan, err = p.PlanWithMixin(ctx, config.Mixin{Name: "20-b.yaml", Data: []byte("mihomo:\n  mode: global\n")})
	if err != nil || strings.Join(plan.Mixins, ",") != "10-a.yaml,20-b.yaml,main/10-x.yaml" {
		t.Errorf("shared: %v %v", plan.Mixins, err)
	}

	plan, err = p.PlanWithMixin(ctx, config.Mixin{Name: "spare/10-x.yaml", Data: []byte("mihomo:\n  log-level: silent\n")})
	if err != nil || !plan.Inactive || plan.Subscription != "spare" || strings.Join(plan.Mixins, ",") != "10-a.yaml,spare/10-x.yaml" ||
		!strings.Contains(string(plan.Config), "\nlog-level: silent\n") {
		t.Fatalf("inactive: %+v, %v", plan, err)
	}
	if _, err := p.Apply(ctx, plan); err == nil || !strings.Contains(err.Error(), "not active") {
		t.Errorf("Apply(inactive) = %v", err)
	}
	if _, err := p.PlanWithMixin(ctx, config.Mixin{Name: "gone/10-x.yaml", Data: []byte("{}\n")}); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("unknown subscription: %v", err)
	}

	// 恢复备份时只用启用订阅的 mixin。
	all, err := config.LoadAllMixins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := mixinsFor(all, "spare"); len(got) != 2 || got[1].Name != "spare/10-x.yaml" {
		t.Errorf("mixinsFor = %v", mixinNames(got))
	}
	if got := mixinsFor(all, ""); len(got) != 1 {
		t.Errorf("mixinsFor(none) = %v", mixinNames(got))
	}
}

// TestMixinErrorIsCheckFailure 验证有错的 mixin 不能发布，按“检查未通过”报告并指出文件。
func TestMixinErrorIsCheckFailure(t *testing.T) {
	p, _, _ := newPipeline(t)
	writeFile(t, filepath.Join(p.Layout.MixinDir(), "50-rules.yaml"), "mihomo:\n  rules: x\ntunlr:\n  rules:\n    prepend: [MATCH,DIRECT]\n")
	_, err := p.Plan(context.Background())
	var me *config.MixinError
	if !errors.As(err, &me) || !errors.Is(err, core.ErrCheckFailed) || err.Error() != "mixin 50-rules.yaml:5: tunlr.rules.prepend: rules is not a list" {
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

	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-api.yaml"), "mihomo:\n  external-ui-name: x\n  external-doh-server: /dns-query\n")
	plan := mustPlan(t, p)
	if !slices.Equal(plan.Restart, []string{"external-doh-server", "external-ui-name"}) {
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

	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-api.yaml"), "mihomo:\n  external-ui-name: x\n")
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
	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-api.yaml"), "mihomo:\n  external-ui-name: x\n")
	res, err := p.Apply(context.Background(), mustPlan(t, p))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reloaded || res.Restarted || !slices.Equal(res.RestartNeeded, []string{"external-ui-name"}) || len(r.loaded) != 2 {
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

func TestTunSwitch(t *testing.T) {
	p, _, _ := newPipeline(t)
	ctx := context.Background()
	mustApply(t, p, mustPlan(t, p))
	if got, _ := os.ReadFile(p.Layout.CoreConfig()); !strings.Contains(string(got), "tun:\n  enable: false\n") {
		t.Fatalf("tun is not off by default:\n%s", got)
	}

	// PlanTun 按开关打开时的样子合成，不看状态文件。
	plan, err := p.PlanTun(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Diff, "-  enable: false\n+  enable: true\n") || len(plan.Restart) > 0 {
		t.Fatalf("diff:\n%s\nrestart: %v", plan.Diff, plan.Restart)
	}
	for _, want := range []string{"  device: tunlr0\n", "  auto-redirect: true\n", "  strict-route: false\n", "  auto-detect-interface: true\n", "  iproute2-rule-index: 9000\n"} {
		if !strings.Contains(string(plan.Config), want) {
			t.Errorf("config lacks %q", want)
		}
	}
	if same := mustPlan(t, p); same.Changed() {
		t.Fatal("Plan follows PlanTun instead of the saved switch")
	}

	st, _ := state.Load(p.Layout.StateFile())
	st.SetTun(state.TunOn, time.Time{})
	if err := state.Save(p.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	if got := mustPlan(t, p); !bytes.Equal(got.Config, plan.Config) {
		t.Fatal("Plan does not follow the saved switch")
	}
	mustApply(t, p, plan)

	// 回滚到开关关闭时的修订：独占的键按现在的开关写入，内容与生效的配置相同。
	rb, err := p.PlanRollback(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rb.Changed() {
		t.Fatalf("rollback changes the tun switch:\n%s", rb.Diff)
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

func TestPlanResetSecret(t *testing.T) {
	p, _, _ := newPipeline(t)
	ctx := context.Background()
	mustApply(t, p, mustPlan(t, p))
	st, _ := state.Load(p.Layout.StateFile())
	old := st.APISecret

	plan, err := p.PlanResetSecret(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if plan.newSecret == old || len(plan.newSecret) != 32 || !slices.Contains(plan.Restart, "secret") {
		t.Fatalf("new %q (old %q), restart %v", plan.newSecret, old, plan.Restart)
	}
	mustApply(t, p, plan)
	if st, _ = state.Load(p.Layout.StateFile()); st.APISecret != plan.newSecret {
		t.Fatalf("saved %q", st.APISecret)
	}
	if cfg, _ := os.ReadFile(p.Layout.CoreConfig()); !strings.Contains(string(cfg), "\nsecret: "+plan.newSecret+"\n") {
		t.Error("new secret not in the configuration")
	}

	// 两份换新的计划：先发布的那份生效，另一份已经过时。
	a, _ := p.PlanResetSecret(ctx)
	b, _ := p.PlanResetSecret(ctx)
	mustApply(t, p, a)
	if _, err := p.Apply(ctx, b); !errors.Is(err, ErrStale) {
		t.Errorf("stale reset: %v", err)
	}

}

// TestManagedTunExcludeUID 验证知道内核用户时 tunlr:tun 按它的 UID 排除内核自己的流量，不知道时不写。
func TestManagedTunExcludeUID(t *testing.T) {
	tun := func(uid int) string {
		for _, m := range managed("", switches{tun: true, coreUID: uid}, &settings.Settings{}) {
			if m.Name == ManagedTun {
				return string(m.Data)
			}
		}
		t.Fatal("no tunlr:tun")
		return ""
	}
	if got := tun(999); !strings.Contains(got, "    iproute2-rule-index: 9000\n    exclude-uid:\n      - 999\n  dns:\n") {
		t.Errorf("known user:\n%s", got)
	}
	if got := tun(-1); strings.Contains(got, "exclude-uid") {
		t.Errorf("unknown user:\n%s", got)
	}
}

// TestManagedConflicts 验证 mixin 动了受管 mixin 独占的键时拒绝合成，提示改用的命令；独占的键在回滚时
// 也按当前状态写入。
func TestManagedConflicts(t *testing.T) {
	p, _, _ := newPipeline(t)
	ctx := context.Background()
	mustApply(t, p, mustPlan(t, p))

	for _, tt := range []struct{ mixin, want string }{
		{"mihomo:\n  secret: mine\n", "mixin 10-a.yaml:2: secret is managed by tunlr:panel (change it with: tunlr panel lan on/off, tunlr panel reset-secret)"},
		{"mihomo:\n  x: 1\n  external-controller: 0.0.0.0:9091\n", "mixin 10-a.yaml:3: external-controller is managed by tunlr:panel"},
		{"mihomo:\n  tun:\n    stack: system\n    enable: true\n", "mixin 10-a.yaml:4: tun.enable is managed by tunlr:tun (change it with: tunlr tun on/off)"},
		{"mihomo:\n  tun:\n    auto-detect-interface: false\n", "mixin 10-a.yaml:3: tun.auto-detect-interface is managed by tunlr:tun"},
		// 测试用的布局不知道内核用户，exclude-uid 不写，仍然独占。
		{"mihomo:\n  tun:\n    exclude-uid: [1000]\n", "mixin 10-a.yaml:3: tun.exclude-uid is managed by tunlr:tun"},
		{"mihomo:\n  external-controller-unix: x.sock\n", "mixin 10-a.yaml:2: external-controller-unix is managed by tunlr:core (tunlr needs it to work)"},
	} {
		writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-a.yaml"), tt.mixin)
		var me *config.MixinError
		if _, err := p.Plan(ctx); !errors.As(err, &me) || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("%q: err = %v", tt.mixin, err)
		}
		if err := p.CheckMixins(ctx); !errors.As(err, &me) {
			t.Errorf("%q: CheckMixins = %v", tt.mixin, err)
		}
	}
	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-a.yaml"), "mihomo:\n  tun:\n    stack: system\n")
	if err := p.CheckMixins(ctx); err != nil {
		t.Errorf("allowed key: %v", err)
	}
	mustApply(t, p, mustPlan(t, p))

	// 回滚到的修订是换 secret 之前的；独占的键按当前状态写入，secret 保持现在的。
	mustApply(t, p, mustPlanReset(t, p))
	st, _ := state.Load(p.Layout.StateFile())
	plan, err := p.PlanRollback(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan.Config), "\nsecret: "+st.APISecret+"\n") {
		t.Errorf("rollback did not keep the current secret:\n%s", plan.Config)
	}

	ms, err := p.Managed()
	if err != nil || len(ms) != 7 || ms[4].Name != ManagedDefaults || ms[5].Name != ManagedDNS || ms[6].Name != ManagedMirror || !ms[6].Default || ms[1].Keys()[0] != "tun.enable" {
		t.Errorf("Managed = %+v, %v", ms, err)
	}
}

func mustPlanReset(t *testing.T, p *Pipeline) *Plan {
	t.Helper()
	plan, err := p.PlanResetSecret(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// 重新合成的配置会开放无需认证的端口时拒绝发布；回滚不检查，旧配置总能恢复。
func TestExposureRejected(t *testing.T) {
	p, _, _ := newPipeline(t)
	ctx := context.Background()
	mustApply(t, p, mustPlan(t, p))
	useSubscription(t, p.Layout, "main", subscriptionV1)
	mustApply(t, p, mustPlan(t, p))

	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-lan.yaml"), "mihomo:\n  listeners:\n    - {name: open, type: mixed, port: 7999}\n")
	var ee *config.ExposureError
	if _, err := p.Plan(ctx); !errors.As(err, &ee) || !errors.Is(err, core.ErrCheckFailed) {
		t.Fatalf("plan: %v", err)
	}
	if _, err := p.PlanRollback(ctx, 0); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-lan.yaml"), "mihomo:\n  listeners:\n    - {name: open, type: mixed, port: 7999}\n  authentication: [\"u:p\"]\n")
	mustPlan(t, p)
}

// TestProxyLAN 验证代理端口的局域网访问开关写入 allow-lan 和只含私有地址段的 lan-allowed-ips，通过端口
// 安全检查；关闭时只有 allow-lan 变为 false（来源列表留在模板的位置上）。mixin 不能写这两个键。
func TestProxyLAN(t *testing.T) {
	p, _, _ := newPipeline(t)
	ctx := context.Background()
	plan, err := p.PlanProxyLAN(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(plan.Config)
	if !strings.Contains(cfg, "\nallow-lan: true\n\n# ") || !strings.Contains(cfg, "\nlan-allowed-ips:\n  - 127.0.0.0/8\n  - 10.0.0.0/8\n") {
		t.Errorf("on:\n%s", cfg)
	}
	if allow, private, err := config.LANAccess(plan.Config); !allow || !private || err != nil {
		t.Errorf("LANAccess = %v %v %v", allow, private, err)
	}
	if off := string(mustPlan(t, p).Config); !strings.Contains(off, "\nallow-lan: false\n\n# ") || !strings.Contains(off, "\nlan-allowed-ips:\n  - 127.0.0.0/8\n  - 10.0.0.0/8\n") {
		t.Errorf("off:\n%s", off)
	}
	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-lan.yaml"), "mihomo:\n  lan-allowed-ips: [0.0.0.0/0]\n")
	var me *config.MixinError
	if _, err := p.PlanProxyLAN(ctx, true); !errors.As(err, &me) || !strings.Contains(err.Error(), "tunlr proxy lan on/off") {
		t.Errorf("mixin: %v", err)
	}
}

func TestMirrorDefaults(t *testing.T) {
	p, _, _ := newPipeline(t)
	writeFile(t, p.Layout.SettingsFile(), "github-mirror: https://gh-proxy.com\n")
	plan := mustPlan(t, p)
	if fmt.Sprint(plan.Rewrites) != "[{geox-url.geoip tunlr:mirror} {geox-url.geosite tunlr:mirror} {geox-url.mmdb tunlr:mirror} {geox-url.asn tunlr:mirror}]" {
		t.Errorf("rewrites = %v", plan.Rewrites)
	}
	cfg := string(plan.Config)
	for _, want := range []string{
		"external-ui-url: https://gh-proxy.com/https://github.com/Zephyruso/zashboard/releases/latest/download/dist.zip\n",
		"  mmdb: https://gh-proxy.com/https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb\n",
		"  geosite: https://gh-proxy.com/https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geosite.dat\n",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q", want)
		}
	}

	// tunlr:mirror 应用在用户的 mixin 之前，用户可以覆盖，覆盖记录在计划中。
	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-a.yaml"), "mihomo:\n  external-ui-url: https://example.com/ui.zip\n")
	plan = mustPlan(t, p)
	if cfg := string(plan.Config); !strings.Contains(cfg, "\nexternal-ui-url: https://example.com/ui.zip\n") {
		t.Errorf("mixin did not override the default:\n%s", cfg)
	}
	if fmt.Sprint(plan.Overrides) != "[{external-ui-url 10-a.yaml tunlr:mirror}]" {
		t.Errorf("overrides = %v", plan.Overrides)
	}
}

// TestTemplateURLs 确认模板中面板的地址、tunlr:defaults 中 geo 数据的地址与内核的默认地址一致。
func TestTemplateURLs(t *testing.T) {
	if !bytes.Contains(templates.Config, []byte("\nexternal-ui-url: "+mihomo.ZashboardURL+"\n")) {
		t.Errorf("template external-ui-url differs from %s", mihomo.ZashboardURL)
	}
	for _, f := range []mihomo.GeoFile{mihomo.GeoIPDat, mihomo.GeoSite, mihomo.GeoMMDB, mihomo.GeoASN} {
		if !bytes.Contains(templates.Defaults, []byte(": "+f.URL()+"\n")) {
			t.Errorf("defaults geox-url lacks %s", f.URL())
		}
	}
	// 模板中写的 lan-allowed-ips 只为固定它在配置中的位置，值必须与 tunlr:proxy 写入的相同。
	if want := "\nlan-allowed-ips:\n  - " + strings.Join(config.LANAllowedIPs, "\n  - ") + "\n"; !bytes.Contains(templates.Config, []byte(want)) {
		t.Errorf("template lan-allowed-ips differs from %v", config.LANAllowedIPs)
	}
}

// TestMirrorRules 验证配置了 GitHub 镜像时，镜像主机的直连规则插在订阅规则前面，用户可以在 mixin 中去掉。
func TestMirrorRules(t *testing.T) {
	p, _, _ := newPipeline(t)
	useSubscription(t, p.Layout, "main", subscriptionV1)
	writeFile(t, p.Layout.SettingsFile(), "github-mirror: https://gh-proxy.com/\n")
	plan := mustPlan(t, p)
	if m := plan.Managed[len(plan.Managed)-1]; m.Name != ManagedMirror || !strings.Contains(string(m.Data), "tunlr:\n  rules:\n    prepend:\n      - DOMAIN,gh-proxy.com,DIRECT\n") {
		t.Errorf("mirror = %s", m.Data)
	}
	if !strings.Contains(string(plan.Config), "\nrules:\n  - DOMAIN,gh-proxy.com,DIRECT\n  - MATCH,a\n") {
		t.Errorf("config:\n%s", plan.Config)
	}

	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-a.yaml"), "tunlr:\n  rules:\n    delete:\n      - DOMAIN,gh-proxy.com,DIRECT\n")
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

// TestSubscriptionDNS 验证采用订阅的 dns：listen 由 tunlr:dns 提供、用户的 mixin 可以覆盖，开启 TUN 时
// 强制 enable，指向本机没有的网卡的 DNS 服务器给出警告。
func TestSubscriptionDNS(t *testing.T) {
	p, _, _ := newPipeline(t)
	ctx := context.Background()
	useSubscription(t, p.Layout, "main", subscriptionV1+"dns:\n  enable: false\n  listen: 0.0.0.0:53\n  nameserver: [dhcp://tunlr-test-none0]\n")
	plan := mustPlan(t, p)
	if cfg := string(plan.Config); !strings.Contains(cfg, "\ndns:\n  enable: false\n  nameserver: ['dhcp://tunlr-test-none0']\n  listen: 127.0.0.1:1053\n") {
		t.Errorf("config:\n%s", cfg)
	}
	if !slices.Contains(plan.Dropped, "dns.listen") {
		t.Errorf("dropped = %v", plan.Dropped)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings with DNS off = %+v", plan.Warnings)
	}
	plan, err := p.PlanTun(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan.Config), "\ndns:\n  enable: true\n  nameserver: ['dhcp://tunlr-test-none0']\n  listen: 127.0.0.1:1053\n") {
		t.Errorf("TUN did not turn DNS on:\n%s", plan.Config)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0].Message, "no network interface tunlr-test-none0") {
		t.Errorf("warnings = %+v", plan.Warnings)
	}

	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-dns.yaml"), "mihomo:\n  dns:\n    listen: 127.0.0.1:5353\n")
	plan = mustPlan(t, p)
	if !strings.Contains(string(plan.Config), "\n  listen: 127.0.0.1:5353\n") || fmt.Sprint(plan.Overrides) != "[{dns.listen 10-dns.yaml tunlr:dns}]" {
		t.Errorf("mixin override: %v\n%s", plan.Overrides, plan.Config)
	}
}

// TestGeoSourceChange 验证 geox-url 改了（如切换到自带 geo 来源的订阅）时，mihomo -t 按新来源下载到暂存目录的
// geo 文件发布时才换进工作目录；内核加载失败时连同配置一起换回。
func TestGeoSourceChange(t *testing.T) {
	const srv = "http://geo.example"
	p, v, r := newPipeline(t)
	// 校验时把新来源的文件“下载”到暂存目录，内容是地址中的路径。
	source := regexp.MustCompile(`geosite: '?` + srv + `(/[^'\s]+)`)
	v.download = func(cfg []byte) {
		if m := source.FindSubmatch(cfg); m != nil {
			writeFile(t, filepath.Join(p.Layout.GeoStageDir(), mihomo.GeoSite.Name), "from "+string(m[1]))
		}
	}
	useSubscription(t, p.Layout, "main", "proxies:\n  - {name: a, type: ss, server: a.example.net, port: 1, cipher: aes-128-gcm, password: p}\nrules:\n  - GEOSITE,cn,a\n  - MATCH,a\n")
	mustApply(t, p, mustPlan(t, p))
	geosite := filepath.Join(p.Layout.CoreDir(), "GeoSite.dat")
	writeFile(t, geosite, "old")
	read := func() string {
		data, _ := os.ReadFile(geosite)
		return string(data)
	}

	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-geo.yaml"), "mihomo:\n  geox-url:\n    geosite: "+srv+"/a.dat\n")
	plan := mustPlan(t, p)
	if fmt.Sprint(plan.GeoReplaced) != "[GeoSite.dat]" || read() != "old" {
		t.Fatalf("plan: replaced %v, file %q", plan.GeoReplaced, read())
	}
	res, err := p.Apply(context.Background(), plan)
	if err != nil || read() != "from /a.dat" || !slices.Contains(res.RestartNeeded, "GeoSite.dat") {
		t.Fatalf("apply: file %q, result %+v, %v", read(), res, err)
	}
	if _, err := os.Stat(p.Layout.GeoStageDir()); !os.IsNotExist(err) {
		t.Errorf("staging left behind: %v", err)
	}
	// 发布后没有变化，不再暂存。
	if plan := mustPlan(t, p); plan.Changed() || plan.GeoReplaced != nil {
		t.Errorf("unchanged: %+v", plan.GeoReplaced)
	}

	writeFile(t, filepath.Join(p.Layout.MixinDir(), "10-geo.yaml"), "mihomo:\n  geox-url:\n    geosite: "+srv+"/b.dat\n")
	plan = mustPlan(t, p)
	r.fail = true
	if _, err := p.Apply(context.Background(), plan); !errors.Is(err, core.ErrRolledBack) {
		t.Fatalf("err = %v", err)
	}
	if read() != "from /a.dat" {
		t.Errorf("geo data not put back: %q", read())
	}
}

// TestDefaultsManaged 验证兜底值 tunlr:defaults：订阅没有写时使用，订阅写了部分 geox-url 时其余的文件用它的地址；
// 它是提供默认值的受管 mixin，不写入 mixin.d。
func TestDefaultsManaged(t *testing.T) {
	p, _, _ := newPipeline(t)
	useSubscription(t, p.Layout, "main", subscriptionV1+"geox-url:\n  geosite: https://geo.example/site.dat\n")
	plan := mustPlan(t, p)
	cfg := string(plan.Config)
	for _, want := range []string{"\n  geosite: https://geo.example/site.dat\n", "\n  mmdb: https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb\n",
		"\ngeo-update-interval: 168\n", "\n  enhanced-mode: fake-ip\n"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q:\n%s", want, cfg)
		}
	}
	for _, s := range plan.GeoSources {
		want := config.KeySource{Path: s.Path, Value: s.Value, From: ManagedDefaults, Fallback: true}
		if s.Path == "geox-url.geosite" {
			want = config.KeySource{Path: s.Path, Value: s.Value, From: config.FromSubscription}
		}
		if s != want {
			t.Errorf("source %+v, want %+v", s, want)
		}
	}
	if i := slices.IndexFunc(plan.Managed, func(m config.Managed) bool { return m.Name == ManagedDefaults }); i < 0 || !plan.Managed[i].Default {
		t.Errorf("managed = %v", plan.Managed)
	}
	if len(plan.Mixins) != 0 {
		t.Errorf("mixins = %v", plan.Mixins)
	}
	if _, err := os.Stat(p.Layout.MixinDir()); !os.IsNotExist(err) {
		t.Errorf("mixin.d: %v", err)
	}
}
