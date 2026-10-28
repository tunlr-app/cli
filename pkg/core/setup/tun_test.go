package setup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/lock"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/state"
)

type passValidator struct{}

func (passValidator) Validate(context.Context, []byte, *config.GeoNeeds) error { return nil }

// fakeReloader 记录每次热重载时磁盘上的配置。
type fakeReloader struct {
	layout paths.Layout
	loaded []string
}

func (r *fakeReloader) Reload(context.Context) error {
	data, _ := os.ReadFile(r.layout.CoreConfig())
	r.loaded = append(r.loaded, string(data))
	return nil
}

// fakeHost 记录 systemctl、systemd-run、nft、ip、iptables 的调用。tables 是 nft list tables 报告的表。
type fakeHost struct {
	calls  []string
	tables []string
	active bool
	// docker 为 nil 时没有 DOCKER-USER 链；否则是链中的规则。
	docker []string
	// failGateway 让加载网关的 nft 表失败。
	failGateway bool
	// devicesSync 是最近一次更新记录设备的表的脚本；seen 是该表中各集合的 nft -j 输出，neigh 是
	// ip -j neigh show 的输出。
	devicesSync string
	seen        map[string]string
	neigh       string
}

func (h *fakeHost) systemd(_ context.Context, name string, args ...string) ([]byte, error) {
	h.calls = append(h.calls, name+" "+strings.Join(args, " "))
	if len(args) > 0 && args[0] == "show" {
		if h.active {
			return []byte("LoadState=loaded\nActiveState=active\nSubState=running\n"), nil
		}
		return []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\n"), nil
	}
	return nil, nil
}

func (h *fakeHost) netfilter(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	h.calls = append(h.calls, cmd)
	switch {
	case cmd == "nft list tables inet":
		var b strings.Builder
		for _, t := range h.tables {
			b.WriteString("table inet " + t + "\n")
		}
		return []byte(b.String()), nil
	case cmd == "nft -f -":
		table := netfilter.Table
		if strings.Contains(string(stdin), "table inet "+netfilter.GatewayTable+" {") {
			if h.failGateway {
				return nil, errors.New("nft: Error: Could not process rule")
			}
			table = netfilter.GatewayTable
		}
		if strings.Contains(string(stdin), "table inet "+netfilter.PanelTable+" {") {
			table = netfilter.PanelTable
		}
		if strings.Contains(string(stdin), "set inet "+netfilter.DevicesTable+" lan") {
			// 记录设备的表：只更新集合时表须已存在。
			table = netfilter.DevicesTable
			h.devicesSync = string(stdin)
			if !strings.Contains(string(stdin), "table inet "+netfilter.DevicesTable+" {") && !slices.Contains(h.tables, table) {
				return nil, errors.New("nft: Error: No such file or directory")
			}
		}
		if !slices.Contains(h.tables, table) {
			h.tables = append(h.tables, table)
		}
	case strings.HasPrefix(cmd, "nft delete table inet "):
		h.tables = slices.DeleteFunc(h.tables, func(t string) bool { return t == args[3] })
	case strings.HasPrefix(cmd, "nft -j list set inet "+netfilter.DevicesTable+" "):
		return []byte(h.seen[args[len(args)-1]]), nil
	case cmd == "ip -j -4 neigh show":
		return []byte(h.neigh), nil
	case cmd == "ip -4 route show default":
		return []byte("default via 192.0.2.1 dev ens3 proto dhcp src 192.0.2.10 metric 100\n"), nil
	case name == "iptables":
		if h.docker == nil {
			return nil, errors.New("iptables: No chain/target/match by that name.")
		}
		rule := strings.Join(args[2:], " ")
		switch args[0] {
		case "-C":
			if !slices.Contains(h.docker, rule) {
				return nil, errors.New("iptables: Bad rule")
			}
		case "-I":
			h.docker = slices.Insert(h.docker, 0, rule)
		case "-D":
			h.docker = slices.DeleteFunc(h.docker, func(r string) bool { return r == rule })
		}
	}
	return nil, nil
}

func (h *fakeHost) called(prefix string) bool {
	return slices.ContainsFunc(h.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

var testNow = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

func newTunInstaller(t *testing.T) (*Installer, *fakeHost, *fakeReloader) {
	t.Helper()
	l := paths.Layout{Root: t.TempDir()}
	h := &fakeHost{}
	r := &fakeReloader{layout: l}
	gh := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(gh.Close)
	now := testNow
	i := &Installer{
		Layout: l, Version: "test",
		Systemd:   &service.Systemd{Layout: l, Run: h.systemd},
		Download:  &download.Client{APIBase: gh.URL},
		Now:       func() time.Time { return now },
		Validator: passValidator{}, Reloader: r,
		Rules: &netfilter.Rules{Run: h.netfilter, IPv6: func() bool { return false }},
		CheckNetwork: func(context.Context, TunOptions) []TunCheck {
			return []TunCheck{{Name: "dns", Status: StepOK}}
		},
	}
	// 先发布一份 TUN 关闭的配置。
	p := i.pipeline(nil)
	plan, err := p.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	r.loaded = nil
	return i, h, r
}

func tunEnabled(t *testing.T, i *Installer) bool {
	t.Helper()
	data, err := os.ReadFile(i.Layout.CoreConfig())
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case strings.Contains(string(data), "tun:\n  enable: true\n"):
		return true
	case strings.Contains(string(data), "tun:\n  enable: false\n"):
		return false
	}
	t.Fatalf("no tun.enable in:\n%s", data)
	return false
}

func tunSwitch(t *testing.T, i *Installer) *state.State {
	t.Helper()
	st, err := state.Load(i.Layout.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func turnOn(t *testing.T, i *Installer) (*TunResult, error) {
	t.Helper()
	plan, err := i.pipeline(nil).PlanTun(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	return i.TunOn(context.Background(), plan, TunOptions{})
}

func TestTunOn(t *testing.T) {
	i, h, r := newTunInstaller(t)
	res, err := turnOn(t, i)
	if err != nil {
		t.Fatal(err)
	}
	if res.Revision == nil || len(res.Checks) != 1 {
		t.Errorf("result = %+v", res)
	}
	if st := tunSwitch(t, i); st.Tun != state.TunOn || !st.TunDeadline.IsZero() {
		t.Errorf("switch = %q, deadline %v", st.Tun, st.TunDeadline)
	}
	if !tunEnabled(t, i) || len(r.loaded) != 1 {
		t.Errorf("config not published with TUN on (reloads: %d)", len(r.loaded))
	}
	// 回滚定时器先布置、最后撤掉；规则在发布前加载。
	arm := slices.IndexFunc(h.calls, func(c string) bool { return strings.HasPrefix(c, "systemd-run ") })
	up := slices.Index(h.calls, "nft -f -")
	if arm < 0 || up < arm {
		t.Errorf("calls = %q", h.calls)
	}
	if last := h.calls[len(h.calls)-1]; !strings.Contains(last, "reset-failed "+service.GuardUnit) {
		t.Errorf("guard not disarmed at the end: %q", h.calls)
	}
	if !strings.Contains(h.calls[arm], "--on-active=180") || !strings.HasSuffix(h.calls[arm], " _tun-guard") {
		t.Errorf("guard = %q", h.calls[arm])
	}
}

func TestTunOnChecksFail(t *testing.T) {
	i, _, r := newTunInstaller(t)
	i.CheckNetwork = func(context.Context, TunOptions) []TunCheck {
		return []TunCheck{{Name: "dns", Status: StepOK}, {Name: "ssh", Status: StepFailed, Message: "not acknowledged"}}
	}
	res, err := turnOn(t, i)
	if !errors.Is(err, core.ErrRolledBack) || !strings.Contains(err.Error(), "ssh: not acknowledged") {
		t.Fatalf("err = %v", err)
	}
	if res == nil || len(res.Checks) != 2 {
		t.Errorf("result = %+v", res)
	}
	if st := tunSwitch(t, i); st.Tun != "" {
		t.Errorf("switch = %q after failed checks", st.Tun)
	}
	if tunEnabled(t, i) || len(r.loaded) != 2 {
		t.Errorf("TUN not turned off again (reloads: %d)", len(r.loaded))
	}
}

func TestTunOff(t *testing.T) {
	i, h, _ := newTunInstaller(t)
	if _, err := turnOn(t, i); err != nil {
		t.Fatal(err)
	}
	h.calls = nil
	plan, err := i.pipeline(nil).PlanTun(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.TunOff(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if st := tunSwitch(t, i); st.Tun != "" || tunEnabled(t, i) {
		t.Errorf("switch = %q, enabled %v", st.Tun, tunEnabled(t, i))
	}
	if !h.called("systemctl stop " + service.GuardUnit + ".timer") {
		t.Errorf("guard not disarmed: %q", h.calls)
	}
}

// setPending 模拟 tun on 发布后被中断：配置已开启，开关仍为 pending。
func setPending(t *testing.T, i *Installer, deadline time.Time) {
	t.Helper()
	i.CheckNetwork = func(context.Context, TunOptions) []TunCheck { return nil }
	if _, err := turnOn(t, i); err != nil {
		t.Fatal(err)
	}
	st := tunSwitch(t, i)
	st.SetTun(state.TunPending, deadline)
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
}

func TestTunGuard(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	if reverted, err := i.TunGuard(context.Background()); err != nil || reverted {
		t.Fatalf("off: reverted %v, err %v", reverted, err)
	}
	setPending(t, i, testNow.Add(-time.Second))
	if reverted, err := i.TunGuard(context.Background()); err != nil || !reverted {
		t.Fatalf("pending: reverted %v, err %v", reverted, err)
	}
	if st := tunSwitch(t, i); st.Tun != "" || tunEnabled(t, i) {
		t.Errorf("switch = %q, enabled %v", st.Tun, tunEnabled(t, i))
	}
}

func TestPreStart(t *testing.T) {
	ctx := context.Background()
	t.Run("pending within the deadline", func(t *testing.T) {
		i, h, _ := newTunInstaller(t)
		setPending(t, i, testNow.Add(time.Minute))
		h.calls = nil
		if err := i.PreStart(ctx); err != nil {
			t.Fatal(err)
		}
		if st := tunSwitch(t, i); st.Tun != state.TunPending || !tunEnabled(t, i) {
			t.Errorf("switch = %q, enabled %v", st.Tun, tunEnabled(t, i))
		}
		if !h.called("nft -f -") {
			t.Errorf("rules not loaded: %q", h.calls)
		}
	})
	t.Run("pending past the deadline", func(t *testing.T) {
		i, h, _ := newTunInstaller(t)
		setPending(t, i, testNow.Add(-time.Minute))
		h.calls = nil
		if err := i.PreStart(ctx); err != nil {
			t.Fatal(err)
		}
		if st := tunSwitch(t, i); st.Tun != "" || tunEnabled(t, i) {
			t.Errorf("switch = %q, enabled %v", st.Tun, tunEnabled(t, i))
		}
		if !h.called("nft -f -") || h.called("systemctl stop "+service.CoreUnit) {
			t.Errorf("calls = %q", h.calls)
		}
	})
	t.Run("lock held", func(t *testing.T) {
		i, _, _ := newTunInstaller(t)
		setPending(t, i, testNow.Add(-time.Minute))
		l, err := lock.TryAcquire(i.Layout.LockFile())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Release()
		if err := i.PreStart(ctx); err != nil {
			t.Fatal(err)
		}
		if st := tunSwitch(t, i); st.Tun != state.TunPending {
			t.Errorf("switch = %q although another operation holds the lock", st.Tun)
		}
	})
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	i, h, _ := newTunInstaller(t)
	h.active = true

	// 透明代理关着时什么都不查，停用自己。
	res, err := i.Check(ctx)
	if err != nil || len(res.Actions) != 1 || !h.called("systemctl disable --now "+service.CheckTimer) || h.called("nft") {
		t.Fatalf("TUN off: %+v, %v, %q", res, err, h.calls)
	}

	st := tunSwitch(t, i)
	st.SetTun(state.TunOn, time.Time{})
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	h.calls = nil
	res, err = i.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Actions) != 1 || !h.called("nft -f -") {
		t.Errorf("missing table not reloaded: %+v, %q", res, h.calls)
	}
	h.calls = nil
	if res, err = i.Check(ctx); err != nil || len(res.Actions) != 0 || h.called("nft -f -") {
		t.Errorf("second check: %+v, %v, %q", res, err, h.calls)
	}

	// 回滚定时器没有生效：过期一分钟以上的 pending 被关闭。
	setPending(t, i, testNow.Add(-2*time.Minute))
	if res, err = i.Check(ctx); err != nil || len(res.Actions) != 2 || !h.called("systemctl disable --now "+service.CheckTimer) {
		t.Fatalf("expired pending: %+v, %v", res, err)
	}
	if st := tunSwitch(t, i); st.Tun != "" || tunEnabled(t, i) {
		t.Errorf("switch = %q, enabled %v", st.Tun, tunEnabled(t, i))
	}
}
