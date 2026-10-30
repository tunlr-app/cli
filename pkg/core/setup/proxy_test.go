package setup

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/state"
)

func TestProxyLAN(t *testing.T) {
	ctx := context.Background()
	i, h, _ := newTunInstaller(t)
	i.Interfaces = lanInterface
	i.InterfaceNames = func() ([]string, error) { return []string{"ens3"}, nil }
	installUnit(t, i)

	pre, err := i.ProxyLANPreflight(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if pre.On || pre.Port != 7890 || len(pre.Interfaces) != 1 || pre.Interfaces[0].Name != "ens3" || len(pre.Interfaces[0].PublicAddresses) != 0 ||
		!pre.Interfaces[0].Allowed || !pre.Access.Default {
		t.Errorf("preflight = %+v", pre)
	}

	plan, err := i.pipeline(nil).PlanProxyLAN(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.SetProxyLAN(ctx, true, plan, &state.LANAccess{Interfaces: []string{"ens3"}}); err != nil {
		t.Fatal(err)
	}
	if st := tunSwitch(t, i); st.ProxyLAN == nil || st.LANAccess == nil {
		t.Errorf("switch not saved: %+v", st)
	}
	if cfg, _ := os.ReadFile(i.Layout.CoreConfig()); !strings.Contains(string(cfg), "\nallow-lan: true\n") {
		t.Errorf("config:\n%s", cfg)
	}
	// 表放行 ens3 进来的、到代理端口的连接；_check 随之启用。
	if !strings.Contains(h.access, "elements = { 7890 }") || !strings.Contains(h.access, `elements = { "ens3" }`) ||
		!h.called("systemctl enable --now "+service.CheckTimer) {
		t.Errorf("calls %q, access table:\n%s", h.calls, h.access)
	}
	if s, err := i.Status(ctx); err != nil || !s.ProxyLAN || s.LANAccess == nil || !slices.Equal(s.LANAccess.Interfaces, []string{"ens3"}) {
		t.Errorf("status = %+v, %v", s, err)
	}

	// mixin 改了代理端口：_check 让表跟上生效的配置。
	writeMixin(t, i, "mihomo:\n  mixed-port: 7891\n  socks-port: 7892\n")
	plan, err = i.pipeline(nil).Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.pipeline(nil).Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.access, "elements = { 7891, 7892 }") {
		t.Errorf("access table after the mixin:\n%s", h.access)
	}
	if err := os.Remove(i.Layout.MixinDir() + "/10-api.yaml"); err != nil {
		t.Fatal(err)
	}

	plan, err = i.pipeline(nil).PlanProxyLAN(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.SetProxyLAN(ctx, false, plan, nil); err != nil {
		t.Fatal(err)
	}
	if st := tunSwitch(t, i); st.ProxyLAN != nil {
		t.Error("switch still on")
	}
	if cfg, _ := os.ReadFile(i.Layout.CoreConfig()); !strings.Contains(string(cfg), "\nallow-lan: false\n") {
		t.Errorf("config:\n%s", cfg)
	}
	if slices.Contains(h.tables, netfilter.AccessTable) || !h.called("systemctl disable --now "+service.CheckTimer) {
		t.Errorf("calls %q, tables %v", h.calls, h.tables)
	}
}

func TestResolveLANAccess(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	ens3 := LANInterface{Name: "ens3", Addresses: []string{"192.168.1.20"}, Default: true}
	eth1 := LANInterface{Name: "eth1", Addresses: []string{"203.0.113.5"}, PublicAddresses: []string{"203.0.113.5"}}
	wg0 := LANInterface{Name: "wg0", Addresses: []string{"10.8.0.2"}, VPN: true}
	ts0 := LANInterface{Name: "tailscale0", Addresses: []string{"100.100.1.2"}, VPN: true, SSH: true}
	cands := []LANInterface{ens3, eth1, ts0, wg0}
	yes := true

	// 没有选过：没有公网地址的网卡，VPN 只取 SSH 会话进来的。
	a, err := i.ResolveLANAccess(cands, nil, nil)
	if err != nil || !slices.Equal(a.Interfaces, []string{"ens3", "tailscale0"}) || a.Containers {
		t.Fatalf("default = %+v, %v", a, err)
	}
	// 参数给出的去重；容器单独给出。
	a, err = i.ResolveLANAccess(cands, []string{"eth1", "eth1", "wg0"}, &yes)
	if err != nil || !slices.Equal(a.Interfaces, []string{"eth1", "wg0"}) || !a.Containers {
		t.Fatalf("explicit = %+v, %v", a, err)
	}
	if _, err := i.ResolveLANAccess(cands, []string{"bad name"}, nil); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("bad name: %v", err)
	}
	// 这里没有的网卡（多半是写错了）拒绝；不在候选中、但存在的网卡（如容器网桥）可以。
	i.Interfaces = func(name string) (*netfilter.Interface, error) {
		if name == "docker0" {
			return &netfilter.Interface{Name: name}, nil
		}
		return nil, os.ErrNotExist
	}
	if _, err := i.ResolveLANAccess(cands, []string{"ens33"}, nil); !errors.Is(err, core.ErrInvalidArgument) ||
		!strings.Contains(err.Error(), "no interface ens33 here (interfaces here: ens3, eth1, tailscale0, wg0)") {
		t.Errorf("unknown interface: %v", err)
	}
	if a, err := i.ResolveLANAccess(cands, []string{"docker0"}, nil); err != nil || !slices.Equal(a.Interfaces, []string{"docker0"}) {
		t.Errorf("existing non-candidate: %+v, %v", a, err)
	}
	// init 用 CheckInterfaces 在动手之前做同样的检查。
	if err := i.CheckInterfaces(cands, "ens3", "docker0"); err != nil {
		t.Errorf("check interfaces: %v", err)
	}
	if err := i.CheckInterfaces(cands, "ens3", "ens33"); !errors.Is(err, core.ErrInvalidArgument) || !strings.Contains(err.Error(), "no interface ens33 here") {
		t.Errorf("check unknown interface: %v", err)
	}
	if err := i.CheckInterfaces(cands, "bad name"); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("check bad name: %v", err)
	}
	// 只有公网网卡时默认什么都不放行，要用 --interface 选。
	if _, err := i.ResolveLANAccess([]LANInterface{eth1}, nil, nil); !errors.Is(err, core.ErrInvalidArgument) || !strings.Contains(err.Error(), "--interface") {
		t.Errorf("only a public interface: %v", err)
	}

	// 选过之后沿用，没有给出的部分也沿用。
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	st.LANAccess = a
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	b, err := i.ResolveLANAccess(cands, nil, nil)
	if err != nil || !slices.Equal(b.Interfaces, []string{"eth1", "wg0"}) || !b.Containers {
		t.Fatalf("stored = %+v, %v", b, err)
	}
	b, err = i.ResolveLANAccess(cands, []string{"ens3"}, nil)
	if err != nil || !slices.Equal(b.Interfaces, []string{"ens3"}) || !b.Containers {
		t.Fatalf("interfaces only = %+v, %v", b, err)
	}
	// 选过、现在不在的网卡照旧可以保留（多选中预先勾着它）。
	if _, err := i.ResolveLANAccess(cands[:1], []string{"ens3", "wg0"}, nil); err != nil {
		t.Errorf("stored but gone: %v", err)
	}
	if acc := lanAccess(st, cands); acc.Default || !acc.Same(a) || acc.Same(b) || !slices.Equal(acc.Interfaces, []string{"eth1", "wg0"}) {
		t.Errorf("lanAccess = %+v", acc)
	}

	// 什么都不放行时拒绝，除非网关开着（它的局域网网卡总是放行）。
	none := []string{}
	no := false
	if _, err := i.ResolveLANAccess(cands, none, &no); !errors.Is(err, core.ErrInvalidArgument) || !strings.Contains(err.Error(), "ens3, eth1") {
		t.Errorf("nothing: %v", err)
	}
	st.Gateway = &state.Gateway{Interface: "ens4"}
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	if _, err := i.ResolveLANAccess(cands, none, &no); err != nil {
		t.Errorf("gateway on: %v", err)
	}
}

func TestAccessRules(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	st := &state.State{
		ProxyLAN:  &state.ProxyLAN{},
		PanelLAN:  &state.PanelLAN{},
		Gateway:   &state.Gateway{Interface: "ens4"},
		LANAccess: &state.LANAccess{Interfaces: []string{"ens3"}, Containers: true},
	}
	cfg := []byte("mixed-port: 7890\nredir-port: 7892\nexternal-controller: 0.0.0.0:9090\n")
	a := i.accessRules(st, cfg)
	if !slices.Equal(a.Ports, []int{7890, 7892, 9090}) || !slices.Equal(a.Interfaces, []string{"ens3", "ens4"}) || !a.Containers {
		t.Errorf("rules = %+v", a)
	}
	// 没有选过入口时只有网关的局域网网卡；开关都关着时没有端口。
	st.LANAccess = nil
	if a := i.accessRules(st, cfg); !slices.Equal(a.Interfaces, []string{"ens4"}) || a.Containers {
		t.Errorf("no access = %+v", a)
	}
	st.ProxyLAN, st.PanelLAN = nil, nil
	if a := i.accessRules(st, cfg); len(a.Ports) != 0 {
		t.Errorf("off = %+v", a)
	}
}
