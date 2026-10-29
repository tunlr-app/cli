package setup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core/doctor"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// doctorHost 是一台满足支持条件的假机器（root、Debian 12、arm64），不运行任何真实命令。
func doctorHost() *doctor.Host {
	files := map[string]string{
		"/proc/sys/kernel/osrelease": "6.1.0-26-arm64\n",
		"/etc/os-release":            "ID=debian\nVERSION_ID=\"12\"\n",
	}
	return &doctor.Host{
		GOOS: "linux", GOARCH: "arm64",
		Euid: func() int { return 0 },
		ReadFile: func(name string) ([]byte, error) {
			if s, ok := files[name]; ok {
				return []byte(s), nil
			}
			return nil, fs.ErrNotExist
		},
		Stat: func(name string) (fs.FileInfo, error) {
			if name == "/run/systemd/system" || name == "/dev/net/tun" {
				return os.Stat(os.TempDir())
			}
			return nil, fs.ErrNotExist
		},
		LookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "systemd-detect-virt":
				return []byte("none\n"), nil
			case "ss":
				return []byte("tcp LISTEN 0 128 0.0.0.0:7890 0.0.0.0:* users:((\"nginx\",pid=5,fd=3))\n"), nil
			}
			return nil, errors.New(name + ": not found")
		},
	}
}

func newDoctorInstaller(t *testing.T) (*Installer, *fakeHost) {
	t.Helper()
	l := paths.Layout{Root: t.TempDir()}
	h := &fakeHost{}
	return &Installer{
		Layout: l, Version: "test",
		Systemd:   &service.Systemd{Layout: l, Run: h.systemd},
		Now:       func() time.Time { return testNow },
		Validator: passValidator{},
		Rules:     &netfilter.Rules{Run: h.netfilter, IPv6: func() bool { return false }},
		Host:      doctorHost(),
	}, h
}

func checkMap(r *doctor.Report) map[string]doctor.Check {
	m := map[string]doctor.Check{}
	for _, c := range r.Checks {
		m[c.ID] = c
	}
	return m
}

func TestDoctorNotInstalled(t *testing.T) {
	i, _ := newDoctorInstaller(t)
	r, err := i.Doctor(context.Background(), DoctorOptions{Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	got := checkMap(r)
	if c := got["install.installed"]; c.Status != doctor.Skip || !strings.Contains(c.Message, "tunlr init") {
		t.Errorf("installed = %+v", c)
	}
	if c := got["network.all"]; c.Status != doctor.Skip {
		t.Errorf("network = %+v", c)
	}
	if got["system.os"].Status != doctor.OK || got["tun.device"].Status != doctor.OK {
		t.Errorf("system %+v, tun %+v", got["system.os"], got["tun.device"])
	}
	if err := r.Err(); err != nil {
		t.Errorf("Err = %v", err)
	}
	// 分组按 doctor.Groups 的顺序。
	last := -1
	for _, c := range r.Checks {
		idx := strings.Index(strings.Join(doctor.Groups, ","), c.Group)
		if idx < last {
			t.Errorf("%s out of order", c.ID)
		}
		last = idx
	}

	// quick 不检查网络。
	r, err = i.Doctor(context.Background(), DoctorOptions{Quick: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Checks {
		if c.Group == doctor.GroupNetwork {
			t.Errorf("quick has %s", c.ID)
		}
	}
	if err := i.CheckSupport(context.Background()); err != nil {
		t.Errorf("CheckSupport: %v", err)
	}
}

func TestDoctorInstalled(t *testing.T) {
	i, _ := newDoctorInstaller(t)
	units, err := service.RenderUnits(i.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(i.Layout.UnitDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range units {
		if name == service.UpdateTimer {
			data = []byte("# old\n")
		}
		if err := os.WriteFile(filepath.Join(i.Layout.UnitDir(), name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 一份发布过的配置，mixed-port 被 nginx 占用（见 doctorHost 的 ss）。
	if err := os.MkdirAll(filepath.Dir(i.Layout.CoreConfig()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(i.Layout.CoreConfig(), []byte("mixed-port: 7890\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := i.Doctor(context.Background(), DoctorOptions{Quick: true})
	if err != nil {
		t.Fatal(err)
	}
	got := checkMap(r)
	for id, want := range map[string]string{
		"install.units":         doctor.Warn,
		"install.service":       doctor.Warn, // inactive
		"install.kernel":        doctor.Fail, // 没有安装内核
		"install.ports":         doctor.Fail,
		"install.subscriptions": doctor.Warn,
		"install.files":         doctor.OK,
	} {
		if c := got[id]; c.Status != want {
			t.Errorf("%s = %s, want %s: %s", id, c.Status, want, c.Message)
		}
	}
	if c := got["install.units"]; !strings.Contains(c.Message, service.UpdateTimer) {
		t.Errorf("units: %q", c.Message)
	}
	if c := got["install.ports"]; !strings.Contains(c.Message, "tcp 7890 (mixed-port) is used by nginx") {
		t.Errorf("ports: %q", c.Message)
	}
	if r.Err() == nil {
		t.Error("no failure reported")
	}
	if _, ok := got["install.gateway"]; ok || got["install.exposure"].Status != doctor.OK {
		t.Errorf("gateway %+v, exposure %+v", got["install.gateway"], got["install.exposure"])
	}

	// 发布之外改成了开放的配置；网关开着但转发和规则都不在。
	if err := os.WriteFile(i.Layout.CoreConfig(), []byte("mixed-port: 7890\nallow-lan: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	st.SetTun(state.TunOn, time.Time{})
	st.Gateway = &state.Gateway{Interface: "ens3", ForwardBefore: "0"}
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	i.Interfaces = func(name string) (*netfilter.Interface, error) {
		return &netfilter.Interface{Name: name, Up: true}, nil
	}
	r, err = i.Doctor(context.Background(), DoctorOptions{Quick: true})
	if err != nil {
		t.Fatal(err)
	}
	got = checkMap(r)
	if c := got["install.exposure"]; c.Status != doctor.Fail || !strings.HasPrefix(c.Message, "allow-lan is on") {
		t.Errorf("exposure = %+v", c)
	}
	if c := got["install.gateway"]; c.Status != doctor.Fail || !strings.Contains(c.Message, "IPv4 forwarding is off") ||
		!strings.Contains(c.Message, netfilter.GatewayTable) {
		t.Errorf("gateway = %+v", c)
	}

	// 非 root：安装的细节都跳过。
	i.Host.Euid = func() int { return 1000 }
	r, err = i.Doctor(context.Background(), DoctorOptions{Quick: true})
	if err != nil {
		t.Fatal(err)
	}
	if c := checkMap(r)["install.installed"]; c.Status != doctor.Skip || !strings.Contains(c.Message, doctor.NeedsRoot) {
		t.Errorf("non-root: %+v", c)
	}
}

func TestDoctorSubscriptions(t *testing.T) {
	i, _ := newDoctorInstaller(t)
	day := 24 * time.Hour
	entry := func(name string, f state.Fetch) subscription.Entry {
		return subscription.Entry{Subscription: subscription.Subscription{Def: subscription.Def{Name: name}, Fetch: f}, Active: name == "main"}
	}
	subs := []subscription.Entry{
		entry("main", state.Fetch{FetchedAt: testNow.Add(-day), UserInfo: &state.UserInfo{Total: 100, Download: 100, Expire: testNow.Add(30 * day)}}),
		entry("spare", state.Fetch{FetchedAt: testNow.Add(-2 * day), LastError: "HTTP 403", LastErrorAt: testNow.Add(-day),
			UserInfo: &state.UserInfo{Expire: testNow.Add(-day)}}),
		entry("soon", state.Fetch{FetchedAt: testNow, UserInfo: &state.UserInfo{Total: 100, Upload: 50, Download: 45, Expire: testNow.Add(3 * day)}}),
		entry("fine", state.Fetch{FetchedAt: testNow, UserInfo: &state.UserInfo{Total: 100, Download: 10}}),
	}
	c := &doctor.Checker{Group: doctor.GroupInstall}
	i.checkSubscriptions(c, subs, nil)
	got := map[string]doctor.Check{}
	for _, ch := range c.List {
		got[ch.ID] = ch
	}
	for id, want := range map[string]string{
		"install.sub-main":  doctor.Fail, // 启用的订阅流量用完
		"install.sub-spare": doctor.Warn, // 未启用的订阅到期只警告
		"install.sub-soon":  doctor.Warn,
		"install.sub-fine":  doctor.OK,
	} {
		if got[id].Status != want {
			t.Errorf("%s = %s, want %s: %s", id, got[id].Status, want, got[id].Message)
		}
	}
	if m := got["install.sub-spare"].Message; !strings.Contains(m, "HTTP 403") || !strings.Contains(m, "expired") {
		t.Errorf("spare: %q", m)
	}
	if m := got["install.sub-soon"].Message; !strings.Contains(m, "95% of the traffic") || !strings.Contains(m, "expires on") {
		t.Errorf("soon: %q", m)
	}

	// 订阅列表读不出来时判 fail，提示改文件。
	c = &doctor.Checker{Group: doctor.GroupInstall}
	i.checkSubscriptions(c, nil, errors.New("subscription list: bad"))
	if len(c.List) != 1 || c.List[0].Status != doctor.Fail || !strings.Contains(c.List[0].Hint, "subscriptions.yaml") {
		t.Errorf("broken list: %+v", c.List)
	}
}
