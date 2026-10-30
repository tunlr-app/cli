package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// installedAt 把状态文件的安装时间改为 at，并加上订阅 home（启用，最近一次抓取在
// fetched）和 spare（不自动更新）。
func installedAt(t *testing.T, i *Installer, at, fetched time.Time) {
	t.Helper()
	// 订阅地址指向一个只返回 404 的本地服务：抓取失败，且不访问网络。
	airport := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(airport.Close)
	st := tunSwitch(t, i)
	st.CreatedAt = at
	st.Fetches = map[string]state.Fetch{"home": {AddedAt: at, FetchedAt: fetched}, "spare": {AddedAt: at, FetchedAt: fetched}}
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	l := &subscription.List{Active: "home", Subscriptions: []subscription.Def{
		{Name: "home", URL: airport.URL + "/home"},
		{Name: "spare", URL: airport.URL + "/spare", Update: settings.PeriodOff},
	}}
	if err := subscription.SaveList(i.Layout.SubscriptionList(), l); err != nil {
		t.Fatal(err)
	}
}

// editList 修改订阅列表。
func editList(t *testing.T, i *Installer, edit func(l *subscription.List)) {
	t.Helper()
	l, err := subscription.LoadList(i.Layout.SubscriptionList())
	if err != nil {
		t.Fatal(err)
	}
	edit(l)
	if err := subscription.SaveList(i.Layout.SubscriptionList(), l); err != nil {
		t.Fatal(err)
	}
}

func TestSchedules(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	installedAt(t, i, testNow.Add(-36*time.Hour), testNow.Add(-25*time.Hour))
	scheds, err := i.Schedules()
	if err != nil {
		t.Fatal(err)
	}
	if len(scheds) != 3 {
		t.Fatalf("schedules = %+v", scheds)
	}
	// 订阅按自己的周期从上次抓取算起：home（每天）已到期，spare 不自动更新；geo（每周）从安装算起，还没有到期。
	home, spare, geo := scheds[0], scheds[1], scheds[2]
	if home.Task != TaskSubscription || home.Name != "home" || !home.Active || home.Period != "1d" || !home.Due(testNow) ||
		home.Last == nil || home.Last.Status != StepOK || !home.Last.At.Equal(testNow.Add(-25*time.Hour)) {
		t.Errorf("home = %+v", home)
	}
	if spare.Name != "spare" || spare.Active || spare.Period != settings.PeriodOff || !spare.Next.IsZero() {
		t.Errorf("spare = %+v", spare)
	}
	if geo.Task != TaskGeo || geo.Period != "7d" || geo.Last != nil || !geo.Updated.IsZero() || !geo.Next.IsZero() || geo.Due(testNow) {
		t.Errorf("geo = %+v", geo)
	}
	// 抓取失败后最多 6 小时再试。
	st := tunSwitch(t, i)
	f := st.Fetches["home"]
	f.LastError, f.LastErrorAt = "HTTP 500", testNow.Add(-time.Hour)
	st.Fetches["home"] = f
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	scheds, _ = i.Schedules()
	if s := scheds[0]; s.Last == nil || s.Last.Status != StepFailed || s.Last.Message != "HTTP 500" || !s.Next.Equal(testNow.Add(5*time.Hour)) {
		t.Errorf("after a failure: %+v", s)
	}

	// geo 数据由内核更新：下次更新是最新的 geo 文件之后再过一个周期，tunlr 不执行，从不到期。
	mtime := testNow.Add(-8 * 24 * time.Hour)
	writeGeoFile(t, i, "geosite.dat", mtime)
	geo = geoSchedule(t, i)
	if !geo.Updated.Equal(mtime) || !geo.Next.Equal(mtime.Add(7*24*time.Hour)) || geo.Due(testNow) {
		t.Errorf("with a geo file: %+v", geo)
	}
	// 周期取自发布的配置：关闭自动更新时为 off，没有下次更新。
	writeCoreConfig(t, i, "geo-auto-update: false\n")
	if geo := geoSchedule(t, i); geo.Period != settings.PeriodOff || !geo.Next.IsZero() {
		t.Errorf("off: %+v", geo)
	}
	writeCoreConfig(t, i, "geo-auto-update: true\ngeo-update-interval: 6\n")
	if geo := geoSchedule(t, i); geo.Period != "6h" || !geo.Next.Equal(mtime.Add(6*time.Hour)) {
		t.Errorf("6 hours: %+v", geo)
	}
}

func geoSchedule(t *testing.T, i *Installer) Schedule {
	t.Helper()
	scheds, err := i.Schedules()
	if err != nil {
		t.Fatal(err)
	}
	return scheds[len(scheds)-1]
}

func writeGeoFile(t *testing.T, i *Installer, name string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(i.Layout.CoreDir(), name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// writeCoreConfig 写入发布的配置（只用来读 geo 的设置）。
func writeCoreConfig(t *testing.T, i *Installer, cfg string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(i.Layout.CoreConfig()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(i.Layout.CoreConfig(), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}
func TestUpdateRunsDueTasks(t *testing.T) {
	ctx := context.Background()
	i, _, _ := newTunInstaller(t)
	installedAt(t, i, testNow.Add(-36*time.Hour), testNow.Add(-25*time.Hour))

	// 只有 home 到期：抓取（测试中的地址不可达，失败并保留原内容），启用的订阅没有更新成功就不发布。
	res, err := i.Update(ctx, UpdateOptions{})
	if err == nil {
		t.Fatal("a failed fetch did not fail the run")
	}
	if len(res.Tasks) != 1 || res.Tasks[0] != TaskSubscription {
		t.Fatalf("tasks = %v", res.Tasks)
	}
	if got := stepNames(res); got != "sub home" || res.Steps[0].Status != StepFailed {
		t.Errorf("steps = %+v", res.Steps)
	}
	if s := tunSwitch(t, i).Fetches["home"]; s.LastError == "" || !s.LastErrorAt.Equal(testNow) {
		t.Errorf("failure not recorded: %+v", s)
	}

	// 失败后 6 小时内不再试，再次唤醒时没有到期的任务，什么都不做。
	if res, err = i.Update(ctx, UpdateOptions{}); err != nil || len(res.Tasks) != 0 || len(res.Steps) != 0 {
		t.Errorf("second run: %+v, %v", res, err)
	}

	// geo 数据从不到期；内核没有运行时手动执行 geo 任务跳过（测试中没有运行的内核）。
	writeGeoFile(t, i, "GeoSite.dat", testNow.Add(-30*24*time.Hour))
	if res, err = i.Update(ctx, UpdateOptions{}); err != nil || len(res.Tasks) != 0 || len(res.Steps) != 0 {
		t.Errorf("geo due: %+v, %v", res, err)
	}

	// 手动执行不看周期：订阅任务抓取全部订阅（包括不自动更新的）。
	res, _ = i.Update(ctx, UpdateOptions{Tasks: []string{TaskSubscription, TaskGeo}})
	if got := stepNames(res); got != "sub home,sub spare,geo" {
		t.Errorf("manual: %+v", res.Steps)
	}
	if s := res.Steps[2]; s.Status != StepSkipped {
		t.Errorf("geo without a kernel: %+v", s)
	}
	if _, err := i.Update(ctx, UpdateOptions{Tasks: []string{"self"}}); err == nil {
		t.Error("unknown task accepted")
	}
}

func stepNames(res *UpdateResult) string {
	var names []string
	for _, s := range res.Steps {
		names = append(names, s.Name)
	}
	return strings.Join(names, ",")
}

func TestSyncTimers(t *testing.T) {
	ctx := context.Background()
	i, h, _ := newTunInstaller(t)
	installedAt(t, i, testNow.Add(-36*time.Hour), testNow.Add(-25*time.Hour))
	// 还没有安装 unit 时不碰 timer。
	if err := i.SyncTimers(ctx); err != nil || len(h.calls) != 0 {
		t.Fatalf("not installed: %v, %q", err, h.calls)
	}
	if err := os.MkdirAll(i.Layout.UnitDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(i.Layout.UnitDir(), service.CoreUnit), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := i.SyncTimers(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.called("systemctl enable --now "+service.UpdateTimer) || !h.called("systemctl enable --now "+service.AftercarePath) ||
		!h.called("systemctl disable --now "+service.CheckTimer) {
		t.Errorf("calls = %q", h.calls)
	}
	// 全部订阅都不自动更新时 tunlr-update.timer 仍然启用：随时可能添加订阅，没有到期的更新时它什么都不做。
	editList(t, i, func(l *subscription.List) { l.Subscriptions[0].Update = settings.PeriodOff })

	writeCoreConfig(t, i, "geo-auto-update: false\n")
	h.calls = nil
	if err := i.SyncTimers(ctx); err != nil || !h.called("systemctl enable --now "+service.UpdateTimer) {
		t.Errorf("all off: %v, %q", err, h.calls)
	}
	// 透明代理开启时启用检查。
	st := tunSwitch(t, i)
	st.SetTun(state.TunOn, time.Time{})
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	h.calls = nil
	if err := i.SyncTimers(ctx); err != nil || !h.called("systemctl enable --now "+service.CheckTimer) {
		t.Errorf("TUN on: %v, %q", err, h.calls)
	}
}

func TestDescribeGeoUpdate(t *testing.T) {
	for _, tt := range []struct {
		before, after map[string]string
		want          string
	}{
		{map[string]string{"GeoSite.dat": "1", "geoip.metadb": "1"}, map[string]string{"GeoSite.dat": "2", "geoip.metadb": "1"},
			"mihomo updated GeoSite.dat; geoip.metadb was already up to date"},
		{map[string]string{"GeoSite.dat": "1", "geoip.metadb": "1"}, map[string]string{"GeoSite.dat": "1", "geoip.metadb": "1"},
			"the geo data was already up to date (geoip.metadb, GeoSite.dat)"},
		{map[string]string{}, map[string]string{"ASN.mmdb": "1"}, "mihomo updated ASN.mmdb"},
		{map[string]string{}, map[string]string{}, "mihomo has no geo data files yet; it downloads them when the configuration uses them"},
	} {
		if got := describeGeoUpdate(tt.before, tt.after); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}
