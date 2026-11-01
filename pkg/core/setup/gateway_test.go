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
	"github.com/tunlr-app/cli/pkg/core/doctor"
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

// ufwRuleset 是 UFW 与 Docker 共存时 nft -j list ruleset 的要点：ip filter 的入站、转发链默认丢弃。
const ufwRuleset = `{"nftables": [
{"chain": {"family": "ip", "table": "filter", "name": "INPUT", "type": "filter", "hook": "input", "prio": 0, "policy": "drop"}},
{"chain": {"family": "ip", "table": "filter", "name": "FORWARD", "type": "filter", "hook": "forward", "prio": 0, "policy": "drop"}},
{"chain": {"family": "ip", "table": "filter", "name": "DOCKER-USER"}},
{"chain": {"family": "ip", "table": "filter", "name": "ufw-before-input"}}]}`

// firewalldRuleset 是 firewalld（nftables 后端）的要点：入站、转发链先放行 DNAT 的连接，最后无条件拒绝。
const firewalldRuleset = `{"nftables": [
{"chain": {"family": "inet", "table": "firewalld", "name": "filter_INPUT", "type": "filter", "hook": "input", "prio": 10, "policy": "accept"}},
{"chain": {"family": "inet", "table": "firewalld", "name": "filter_FORWARD", "type": "filter", "hook": "forward", "prio": 10, "policy": "accept"}},
{"rule": {"family": "inet", "table": "firewalld", "chain": "filter_INPUT", "expr": [{"match": {"op": "in", "left": {"ct": {"key": "status"}}, "right": "dnat"}}, {"accept": null}]}},
{"rule": {"family": "inet", "table": "firewalld", "chain": "filter_INPUT", "expr": [{"reject": {"type": "icmpx", "expr": "admin-prohibited"}}]}},
{"rule": {"family": "inet", "table": "firewalld", "chain": "filter_FORWARD", "expr": [{"match": {"op": "in", "left": {"ct": {"key": "status"}}, "right": "dnat"}}, {"accept": null}]}},
{"rule": {"family": "inet", "table": "firewalld", "chain": "filter_FORWARD", "expr": [{"reject": {"type": "icmpx", "expr": "admin-prohibited"}}]}}]}`

// TestGatewayBlocked 验证按最近 5 分钟记下的新连接判断被其他防火墙丢弃的流量：某台设备的一类流量一个都
// 没有通过时 doctor 报 fail，列出设备、可疑的规则链和放行方法；没有流量或都通过为 ok。
func TestGatewayBlocked(t *testing.T) {
	i, h := newGatewayInstaller(t, "1")
	ctx := context.Background()
	h.docker = []string{"-j RETURN"}
	gatewayOn(t, i, "ens3")
	h.ruleset = ufwRuleset
	check := func() doctor.Check {
		t.Helper()
		c := &doctor.Checker{Group: doctor.GroupInstall}
		i.checkGatewayTraffic(ctx, c, "ens3", true)
		if len(c.List) != 1 {
			t.Fatalf("checks = %+v", c.List)
		}
		return c.List[0]
	}
	if c := check(); c.Status != doctor.OK || c.Message != "no LAN device used the gateway in the last 5 minutes" {
		t.Errorf("no traffic: %+v", c)
	}

	// UFW 与 Docker 共存：转发经 DOCKER-USER 放行，重定向的 TCP 被 INPUT 丢弃。另有一台设备的 ping 被丢弃，
	// 但另一台的通过（不同设备分开判断）；UDP 的 DNS 被丢弃而 TCP 的通过，按协议分开，DNS 算被丢弃；
	// 不认识的协议写作协议号。
	h.uses = map[string][]string{
		"dns_seen":       {"10.0.0.2 udp", "10.0.0.2 tcp", "10.0.0.3 udp"},
		"dns_passed":     {"10.0.0.2 tcp", "10.0.0.3 udp"},
		"forward_seen":   {"10.0.0.2 icmp", "10.0.0.3 icmp", "10.0.0.3 47"},
		"forward_passed": {"10.0.0.3 icmp"},
		"input_seen":     {"10.0.0.3 tcp", "10.0.0.2 tcp"},
	}
	s, err := i.GatewayStatus(ctx)
	if err != nil || !slices.Equal(s.Recent, []string{"10.0.0.2", "10.0.0.3"}) || len(s.Blocks) != 4 {
		t.Fatalf("status = %+v, %v", s, err)
	}
	if b := s.Blocks[0]; b.Class != "DNS" || b.Path != netfilter.PathDNS || !slices.Equal(b.Devices, []string{"10.0.0.2"}) || len(b.Suspects) != 0 {
		t.Errorf("DNS = %+v", b)
	}
	if b := s.Blocks[1]; b.Class != "ping" || !slices.Equal(b.Devices, []string{"10.0.0.2"}) || len(b.Suspects) != 0 ||
		!strings.Contains(b.Fixes[0], "iptables-legacy") {
		t.Errorf("ping = %+v", b)
	}
	if b := s.Blocks[2]; b.Class != "protocol 47" || !slices.Equal(b.Devices, []string{"10.0.0.3"}) {
		t.Errorf("protocol 47 = %+v", b)
	}
	b := s.Blocks[3]
	if b.Class != "TCP" || b.Path != netfilter.PathInput || len(b.Suspects) != 1 || b.Suspects[0].Chain != "INPUT" ||
		!slices.Equal(b.Fixes, []string{"ufw allow in on ens3"}) {
		t.Errorf("TCP = %+v", b)
	}
	if want := "TCP from 10.0.0.2 and 10.0.0.3 is dropped by ip filter INPUT (UFW)"; b.Describe() != want {
		t.Errorf("Describe() = %q", b.Describe())
	}
	if c := check(); c.Status != doctor.Fail || !strings.HasPrefix(c.Message, "DNS from 10.0.0.2 is dropped by another firewall; ping from 10.0.0.2 is dropped by another firewall; protocol 47 from 10.0.0.3 is dropped by another firewall; TCP from") ||
		!strings.HasSuffix(c.Hint, "; ufw allow in on ens3") {
		t.Errorf("dropped: %+v", c)
	}
	// DOCKER-USER 中的放行规则不在时，UFW 的转发链也可疑。
	c := &doctor.Checker{Group: doctor.GroupInstall}
	i.checkGatewayTraffic(ctx, c, "ens3", false)
	if !strings.Contains(c.List[0].Message, "ping from 10.0.0.2 is dropped by ip filter FORWARD (UFW)") ||
		!strings.Contains(c.List[0].Hint, "ufw route allow in on ens3 out on tunlr0") {
		t.Errorf("without the DOCKER-USER rules: %+v", c.List[0])
	}

	// firewalld：先放行 DNAT 的连接，所以只有转发的 UDP、ping 被丢弃（UDP、TCP 的 DNS 都通过）；放行方法只列一次。
	h.ruleset = firewalldRuleset
	h.uses = map[string][]string{
		"dns_seen": {"10.0.0.2 udp", "10.0.0.2 tcp"}, "dns_passed": {"10.0.0.2 udp", "10.0.0.2 tcp"},
		"forward_seen": {"10.0.0.2 udp", "10.0.0.2 icmp"},
		"input_seen":   {"10.0.0.2 tcp"}, "input_passed": {"10.0.0.2 tcp"},
	}
	want := "firewall-cmd --permanent --zone=trusted --add-interface=ens3, then firewall-cmd --reload"
	if c := check(); c.Status != doctor.Fail || c.Message != "UDP from 10.0.0.2 is dropped by inet firewalld filter_FORWARD (firewalld); "+
		"ping from 10.0.0.2 is dropped by inet firewalld filter_FORWARD (firewalld)" || c.Hint != want {
		t.Errorf("firewalld: %+v", c)
	}

	// 都通过。
	h.uses = map[string][]string{"dns_seen": {"10.0.0.2 udp", "10.0.0.3 udp"}, "dns_passed": {"10.0.0.2 udp", "10.0.0.3 udp"}}
	if c := check(); c.Status != doctor.OK || c.Message != "2 LAN devices used the gateway in the last 5 minutes; nothing was dropped" {
		t.Errorf("passed: %+v", c)
	}
}

func TestGatewayBlockDescribe(t *testing.T) {
	b := GatewayBlock{Class: "DNS", Devices: []string{"10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6"}}
	if got := b.Describe(); got != "DNS from 10.0.0.2, 10.0.0.3, 10.0.0.4 and 2 more is dropped by another firewall" {
		t.Errorf("five: %q", got)
	}
	b.Devices = b.Devices[:3]
	if got := b.Describe(); got != "DNS from 10.0.0.2, 10.0.0.3 and 10.0.0.4 is dropped by another firewall" {
		t.Errorf("three: %q", got)
	}
}

// TestBlockerWarnings 验证开启前的提示：DOCKER-USER 能处理的转发链、先放行 DNAT 连接的入站链不提示，
// 其余提示可能挡住的流量和放行方法。
func TestBlockerWarnings(t *testing.T) {
	blockers := func(ruleset string) []netfilter.Blocker {
		t.Helper()
		list, err := (&netfilter.Rules{Run: func(context.Context, []byte, string, ...string) ([]byte, error) {
			return []byte(ruleset), nil
		}}).Blockers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return list
	}
	ufw := blockers(ufwRuleset)
	w := blockerWarnings("ens3", ufw)
	if len(w) != 1 || !strings.HasPrefix(w[0], "ip filter INPUT (UFW) drops incoming traffic by default, which may block LAN devices' TCP") ||
		!strings.Contains(w[0], "ufw allow in on ens3") {
		t.Errorf("warnings = %q", w)
	}
	// 没有 Docker 时转发链也提示。
	for n := range ufw {
		ufw[n].DockerChain = false
	}
	if w := blockerWarnings("ens3", ufw); len(w) != 2 || !strings.Contains(w[0], "LAN devices' DNS, UDP and ping") ||
		!strings.Contains(w[0], "ufw route allow in on ens3 out on tunlr0") {
		t.Errorf("without Docker: %q", w)
	}
	// firewalld 放行被劫持的 DNS 和重定向的 TCP：只提示 UDP 和 ping。
	w = blockerWarnings("ens3", blockers(firewalldRuleset))
	if len(w) != 1 || !strings.Contains(w[0], "inet firewalld filter_FORWARD (firewalld) drops forwarded traffic by default, which may block LAN devices' UDP and ping") ||
		!strings.Contains(w[0], "firewall-cmd --permanent --zone=trusted --add-interface=ens3") {
		t.Errorf("firewalld: %q", w)
	}
}

// TestReloadTables 验证 init 不重启内核时，各表都在也按现在的模板整表重新加载（升级后不留旧版本的规则）。
func TestReloadTables(t *testing.T) {
	i, h := newGatewayInstaller(t, "1")
	gatewayOn(t, i, "ens3")
	if err := i.PreStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.tunlr, h.gateway, h.devicesSync, h.calls = "", "", "", nil
	if err := i.reloadTables(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.tunlr == "" || h.gateway == "" || h.devicesSync == "" || h.called("systemctl restart") {
		t.Errorf("tunlr %v, gateway %v, devices %v; calls %q", h.tunlr != "", h.gateway != "", h.devicesSync != "", h.calls)
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
