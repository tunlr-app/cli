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
)

// installedAt 把状态文件的安装时间改为 at，清掉更新记录，并加上订阅 home（启用，最近一次抓取在
// fetched）和 spare（不自动更新）。
func installedAt(t *testing.T, i *Installer, at, fetched time.Time) {
	t.Helper()
	// 订阅地址指向一个只返回 404 的本地服务：抓取失败，且不访问网络。
	airport := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(airport.Close)
	st := tunSwitch(t, i)
	st.CreatedAt, st.Updates = at, nil
	st.Subscriptions = []state.Subscription{
		{Name: "home", URL: airport.URL + "/home", AddedAt: at, FetchedAt: fetched},
		{Name: "spare", URL: airport.URL + "/spare", Update: settings.PeriodOff, AddedAt: at, FetchedAt: fetched},
	}
	st.Active = "home"
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
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
	if geo.Task != TaskGeo || geo.Period != "7d" || geo.Updater != settings.GeoUpdaterTunlr || geo.Last != nil || geo.Due(testNow) ||
		!geo.Next.Equal(testNow.Add(-36*time.Hour+7*24*time.Hour)) {
		t.Errorf("geo = %+v", geo)
	}

	// 抓取失败后最多 6 小时再试。
	st := tunSwitch(t, i)
	st.Subscriptions[0].LastError, st.Subscriptions[0].LastErrorAt = "HTTP 500", testNow.Add(-time.Hour)
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	scheds, _ = i.Schedules()
	if s := scheds[0]; s.Last == nil || s.Last.Status != StepFailed || s.Last.Message != "HTTP 500" || !s.Next.Equal(testNow.Add(5*time.Hour)) {
		t.Errorf("after a failure: %+v", s)
	}

	// 由内核自己更新 geo 数据时没有到期时间，最近一次更新是最新的 geo 文件的修改时间。
	if err := i.SetGeoUpdate(context.Background(), "", settings.GeoUpdaterMihomo); err != nil {
		t.Fatal(err)
	}
	if scheds, _ = i.Schedules(); scheds[2].Updater != settings.GeoUpdaterMihomo || !scheds[2].Next.IsZero() || scheds[2].Last != nil {
		t.Errorf("mihomo updater: %+v", scheds[2])
	}
	geoFile := filepath.Join(i.Layout.CoreDir(), "geosite.dat")
	if err := os.WriteFile(geoFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := testNow.Add(-3 * 24 * time.Hour)
	if err := os.Chtimes(geoFile, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if scheds, _ = i.Schedules(); scheds[2].Last == nil || !scheds[2].Last.At.Equal(mtime) {
		t.Errorf("mihomo updater with a geo file: %+v", scheds[2])
	}

	// 关闭后没有到期时间，也就不会到期；非法的值不保存。
	if err := i.SetGeoUpdate(context.Background(), settings.PeriodOff, ""); err != nil {
		t.Fatal(err)
	}
	if scheds, _ = i.Schedules(); !scheds[2].Next.IsZero() || scheds[2].Due(testNow) || scheds[2].Updater != settings.GeoUpdaterTunlr {
		t.Errorf("off: %+v", scheds[2])
	}
	if err := i.SetGeoUpdate(context.Background(), "2w", ""); err == nil {
		t.Error("invalid period accepted")
	}
	if err := i.SetGeoUpdate(context.Background(), "", "clash"); err == nil {
		t.Error("invalid updater accepted")
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
	if got := stepNames(res); got != "sub home,kernel" || res.Steps[0].Status != StepFailed {
		t.Errorf("steps = %+v", res.Steps)
	}
	if s := tunSwitch(t, i).Find("home"); s.LastError == "" || !s.LastErrorAt.Equal(testNow) {
		t.Errorf("failure not recorded: %+v", s)
	}

	// 失败后 6 小时内不再试，再次唤醒时没有到期的任务，什么都不做。
	if res, err = i.Update(ctx, UpdateOptions{}); err != nil || len(res.Tasks) != 0 || len(res.Steps) != 0 {
		t.Errorf("second run: %+v, %v", res, err)
	}

	// 手动执行不看周期：订阅任务抓取全部订阅（包括不自动更新的），geo 任务记入状态文件。
	res, _ = i.Update(ctx, UpdateOptions{Tasks: []string{TaskSubscription, TaskGeo}})
	if got := stepNames(res); got != "sub home,sub spare,geo,kernel" {
		t.Errorf("manual: %+v", res.Steps)
	}
	if run, ok := tunSwitch(t, i).Updates[TaskGeo]; !ok || !run.At.Equal(testNow) {
		t.Errorf("geo run not recorded: %+v", tunSwitch(t, i).Updates)
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
	if err := i.SetGeoUpdate(ctx, settings.PeriodOff, ""); err != nil || len(h.calls) != 0 {
		t.Fatalf("not installed: %v, %q", err, h.calls)
	}
	if err := os.MkdirAll(i.Layout.UnitDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(i.Layout.UnitDir(), service.CoreUnit), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// 已安装时修改设置随即同步 timer：geo 关闭但 home 仍每天更新，tunlr-update.timer 保持启用。
	if err := i.SetGeoUpdate(ctx, settings.PeriodOff, ""); err != nil {
		t.Fatal(err)
	}
	if !h.called("systemctl enable --now "+service.UpdateTimer) || !h.called("systemctl disable --now "+service.CheckTimer) {
		t.Errorf("calls = %q", h.calls)
	}
	// 全部订阅都不自动更新、geo 也关闭时停用。
	st := tunSwitch(t, i)
	st.Subscriptions[0].Update = settings.PeriodOff
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	h.calls = nil
	if err := i.SyncTimers(ctx); err != nil || !h.called("systemctl disable --now "+service.UpdateTimer) {
		t.Errorf("all off: %v, %q", err, h.calls)
	}
	// 由内核自己更新 geo 数据不需要 timer。
	h.calls = nil
	if err := i.SetGeoUpdate(ctx, "", settings.GeoUpdaterMihomo); err != nil || !h.called("systemctl disable --now "+service.UpdateTimer) {
		t.Errorf("mihomo updater: %v, %q", err, h.calls)
	}
	h.calls = nil
	if err := i.SetGeoUpdate(ctx, "", ""); err != nil || !h.called("systemctl enable --now "+service.UpdateTimer) {
		t.Errorf("defaults: %v, %q", err, h.calls)
	}

	// 透明代理开启时启用检查。
	st = tunSwitch(t, i)
	st.SetTun(state.TunOn, time.Time{})
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	h.calls = nil
	if err := i.SyncTimers(ctx); err != nil || !h.called("systemctl enable --now "+service.CheckTimer) {
		t.Errorf("TUN on: %v, %q", err, h.calls)
	}
}
