package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// newGatewayInstaller 返回透明代理已开启、ip_forward 为 forward 的安装。
func newGatewayInstaller(t *testing.T, forward string) (*Installer, *fakeHost) {
	t.Helper()
	i, h, _ := newTunInstaller(t)
	i.Interfaces = func(name string) (*netfilter.Interface, error) {
		if name != "ens3" && name != "eth1" {
			return nil, errors.New("no such network interface")
		}
		return &netfilter.Interface{Name: name, Up: true, IPv4: []string{"192.0.2.10"}}, nil
	}
	if _, err := turnOn(t, i); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(i.Layout.IPForward()), 0o755); err != nil {
		t.Fatal(err)
	}
	writeForward(t, i, forward)
	return i, h
}

func writeForward(t *testing.T, i *Installer, v string) {
	t.Helper()
	if err := os.WriteFile(i.Layout.IPForward(), []byte(v+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gatewayOn(t *testing.T, i *Installer, iface string) *GatewayResult {
	t.Helper()
	res, err := i.GatewayOn(context.Background(), iface)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGatewayOnOff(t *testing.T) {
	i, h := newGatewayInstaller(t, "0")
	ctx := context.Background()
	res := gatewayOn(t, i, "ens3")
	if res.Interface != "ens3" || !slices.Equal(res.Addresses, []string{"192.0.2.10"}) || res.Docker {
		t.Errorf("result = %+v", res)
	}
	if i.ipForward() != "1" || !slices.Contains(h.tables, netfilter.GatewayTable) {
		t.Fatalf("forward %q, tables %v", i.ipForward(), h.tables)
	}
	if data, err := os.ReadFile(i.Layout.SysctlFile()); err != nil || !strings.Contains(string(data), "net.ipv4.ip_forward = 1\n") {
		t.Fatalf("sysctl file: %q %v", data, err)
	}
	if gw := tunSwitch(t, i).Gateway; gw == nil || gw.Interface != "ens3" || gw.ForwardBefore != "0" {
		t.Fatalf("state = %+v", gw)
	}
	s, err := i.GatewayStatus(ctx)
	if err != nil || !s.On || !s.Forward || !s.Sysctl || s.Rules == nil || !s.Rules.Complete() || s.Tun != state.TunOn {
		t.Fatalf("status = %+v, %v", s, err)
	}
	if ts, _ := i.TunStatus(ctx); ts.Gateway != "ens3" {
		t.Errorf("tun status gateway = %q", ts.Gateway)
	}

	// 记录设备的表随网关加载，集合 lan 是局域网网卡。
	if !strings.Contains(h.devicesSync, `add element inet tunlr_devices lan { "ens3" }`) || strings.Contains(h.devicesSync, "add element inet tunlr_devices ports") {
		t.Errorf("devices sync:\n%s", h.devicesSync)
	}

	// 内核停止时网关和记录设备的表保留（失败时关闭）。
	if err := i.PostStop(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.tables, []string{netfilter.GatewayTable, netfilter.DevicesTable}) {
		t.Fatalf("after poststop: %v", h.tables)
	}

	on, err := i.GatewayOff(ctx)
	if err != nil || !on {
		t.Fatalf("off = %v, %v", on, err)
	}
	if _, err := os.Stat(i.Layout.SysctlFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sysctl file left: %v", err)
	}
	if i.ipForward() != "0" || len(h.tables) != 0 || tunSwitch(t, i).Gateway != nil || tunSwitch(t, i).Tun != state.TunOn {
		t.Errorf("after off: forward %q, tables %v, state %+v", i.ipForward(), h.tables, tunSwitch(t, i))
	}
	if on, err := i.GatewayOff(ctx); err != nil || on {
		t.Errorf("second off = %v, %v", on, err)
	}
}

func TestGatewayDocker(t *testing.T) {
	i, h := newGatewayInstaller(t, "0")
	h.docker = []string{"-j RETURN"}
	if res := gatewayOn(t, i, "ens3"); !res.Docker {
		t.Errorf("result = %+v", res)
	}
	if len(h.docker) != 3 {
		t.Fatalf("DOCKER-USER = %q", h.docker)
	}
	// 换网卡：旧网卡的规则撤掉，开启前的 ip_forward 保留最初的值。
	gatewayOn(t, i, "eth1")
	if len(h.docker) != 3 || !strings.Contains(h.docker[0]+h.docker[1], "eth1") || strings.Contains(strings.Join(h.docker, " "), "ens3") {
		t.Fatalf("DOCKER-USER = %q", h.docker)
	}
	if gw := tunSwitch(t, i).Gateway; gw.Interface != "eth1" || gw.ForwardBefore != "0" {
		t.Fatalf("state = %+v", gw)
	}
	if _, err := i.GatewayOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Docker 需要转发：开启前为 0 也保持 1。
	if !slices.Equal(h.docker, []string{"-j RETURN"}) || i.ipForward() != "1" {
		t.Errorf("after off: %q, forward %q", h.docker, i.ipForward())
	}
}

// 不给网卡时取默认路由所在的网卡；已经开启时沿用正在使用的网卡（再次执行是为了恢复缺失的部分）。
func TestGatewayInterface(t *testing.T) {
	i, _ := newGatewayInstaller(t, "0")
	ctx := context.Background()
	iface := func(given string) string {
		t.Helper()
		s, err := i.GatewayStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, err := i.gatewayInterface(ctx, given, s)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := iface(""); got != "ens3" {
		t.Errorf("off: %q", got)
	}
	gatewayOn(t, i, "eth1")
	if got := iface(""); got != "eth1" {
		t.Errorf("on eth1: %q", got)
	}
	if got := iface("ens3"); got != "ens3" {
		t.Errorf("given ens3: %q", got)
	}
}

func TestGatewayNeedsTun(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	if _, err := i.GatewayOn(context.Background(), "ens3"); !errors.Is(err, core.ErrCheckFailed) {
		t.Fatalf("err = %v", err)
	}
}

func TestGatewayOnFailureUndoes(t *testing.T) {
	i, h := newGatewayInstaller(t, "0")
	h.failGateway = true
	_, err := i.GatewayOn(context.Background(), "ens3")
	if !errors.Is(err, core.ErrRolledBack) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(i.Layout.SysctlFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sysctl file left: %v", err)
	}
	if i.ipForward() != "0" || tunSwitch(t, i).Gateway != nil {
		t.Errorf("forward %q, state %+v", i.ipForward(), tunSwitch(t, i).Gateway)
	}
}

func TestTunOffTurnsGatewayOff(t *testing.T) {
	i, h := newGatewayInstaller(t, "1")
	gatewayOn(t, i, "ens3")
	plan, err := i.pipeline(nil).PlanTun(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.TunOff(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	st := tunSwitch(t, i)
	if st.Gateway != nil || st.Tun != "" || slices.Contains(h.tables, netfilter.GatewayTable) {
		t.Errorf("state %+v, tables %v", st, h.tables)
	}
	// 开启前转发已经是 1：保持。
	if i.ipForward() != "1" {
		t.Errorf("forward = %q", i.ipForward())
	}
}

func TestCheckRepairsGateway(t *testing.T) {
	i, h := newGatewayInstaller(t, "0")
	gatewayOn(t, i, "ens3")
	h.docker = []string{"-j RETURN"} // Docker 在网关开启后才启动
	h.tables = slices.DeleteFunc(h.tables, func(t string) bool { return t == netfilter.GatewayTable || t == netfilter.DevicesTable })
	writeForward(t, i, "0")
	res, err := i.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 第四项是拉起没在运行的 tunlr-firewall.service（假的 systemd 报告所有单元都没在运行）。
	if len(res.Actions) != 4 || !slices.Contains(h.tables, netfilter.GatewayTable) || !slices.Contains(h.tables, netfilter.DevicesTable) ||
		len(h.docker) != 3 || i.ipForward() != "1" {
		t.Fatalf("actions %q, tables %v, docker %q, forward %q", res.Actions, h.tables, h.docker, i.ipForward())
	}
	// 都在时什么都不做。
	h.active = true
	if res, err := i.Check(context.Background()); err != nil || len(res.Actions) != 0 {
		t.Fatalf("second check: %+v %v", res, err)
	}
}

func TestPreStartReloadsGateway(t *testing.T) {
	i, h := newGatewayInstaller(t, "1")
	gatewayOn(t, i, "ens3")
	h.tables = nil // 重启后 nft 规则都不在
	if err := i.PreStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.tables, netfilter.GatewayTable) || !slices.Contains(h.tables, netfilter.Table) {
		t.Fatalf("tables = %v", h.tables)
	}
}
