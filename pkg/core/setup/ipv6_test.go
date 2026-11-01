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
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
)

// setIPv6 用 PlanIPv6、SaveIPv6 把 IPv6 设为 set，返回修改和 SaveIPv6 的错误。
func setIPv6(t *testing.T, i *Installer, set string) (*IPv6Edit, error) {
	t.Helper()
	e, plan, err := i.PlanIPv6(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	_, err = i.SaveIPv6(context.Background(), e, plan)
	return e, err
}

func published(t *testing.T, i *Installer) []byte {
	t.Helper()
	cfg, err := os.ReadFile(i.Layout.CoreConfig())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestIPv6Answers 验证 IPv6 的回答：on 写 dns.ipv6，配置中已有 IPv6 假地址段时不另写；网关开着时提醒；off 删去
// mixin 中的地址段；default 删去两者。
func TestIPv6Answers(t *testing.T) {
	i, _ := newGatewayInstaller(t, "1")
	ctx := context.Background()
	set := func(v string) *IPv6Edit {
		t.Helper()
		e, err := setIPv6(t, i, v)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	changes := func(e *IPv6Edit) string {
		var out []string
		for _, c := range e.Changes {
			out = append(out, c.Name+" set "+strings.Join(c.Set, ",")+" unset "+strings.Join(c.Unset, ","))
		}
		return strings.Join(out, "; ")
	}

	s, err := i.DNSStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.IPv6.Mode != IPv6Off || s.IPv6.Answers.On || !s.IPv6.Answers.Fallback || s.FakeIPRange != "198.18.0.1/16" || s.FakeIPRange6 != "" {
		t.Errorf("fallback: %+v, ranges %q %q", s.IPv6, s.FakeIPRange, s.FakeIPRange6)
	}

	// 共用的 mixin 写了 IPv6 假地址段：on 只写 dns.ipv6。
	writeFileIn(t, filepath.Join(i.Layout.MixinDir(), "10-range.yaml"), "mihomo:\n  dns:\n    fake-ip-range6: fd00:1::/64\n")
	e := set(IPv6On)
	if got := changes(e); got != DNSMixinFile+" set dns.ipv6 unset " || !e.After || e.Gateway != "" || e.NoRoute {
		t.Errorf("on: %s %+v", got, e)
	}
	if cfg := published(t, i); !config.DNSIPv6(cfg) || !strings.Contains(string(cfg), "fake-ip-range6: fd00:1::/64") {
		t.Errorf("published:\n%s", cfg)
	}
	if s, _ := i.IPv6(ctx); s.Mode != IPv6On || !s.Answers.Pinned() || !slices.Equal(s.Answers.Files, []string{"10-range.yaml", DNSMixinFile}) {
		t.Errorf("on: %+v", s)
	}
	if s, _ := i.DNSStatus(ctx); s.FakeIPRange6 != "fd00:1::/64" {
		t.Errorf("on: range %q", s.FakeIPRange6)
	}
	if e, plan, _ := i.PlanIPv6(ctx, IPv6On); e.Changed || plan != nil {
		t.Errorf("on again: %s", changes(e))
	}

	// 网关开着：status 和 doctor 都提醒，fake-ip 下说假地址连不上。
	gatewayOn(t, i, "ens3")
	if s, _ := i.DNSStatus(ctx); !strings.Contains(s.GatewayIPv6, "the fake addresses lead nowhere") {
		t.Errorf("gateway: %q", s.GatewayIPv6)
	}
	if w := i.publishedGatewayIPv6Warning(); !strings.Contains(w, "the gateway forwards only IPv4") {
		t.Errorf("published warning: %q", w)
	}

	// off：两个文件都改，地址段所在的文件删空。
	e = set(IPv6Off)
	if got := changes(e); got != "10-range.yaml set  unset dns.fake-ip-range6; "+DNSMixinFile+" set dns.ipv6 unset " || e.After || e.Gateway != "" {
		t.Errorf("off: %s %+v", got, e)
	}
	if _, err := os.Stat(filepath.Join(i.Layout.MixinDir(), "10-range.yaml")); !os.IsNotExist(err) {
		t.Errorf("10-range.yaml: %v", err)
	}
	if w := i.publishedGatewayIPv6Warning(); w != "" {
		t.Errorf("off, warning: %q", w)
	}

	// default：删去 dns.ipv6，回到兜底值。
	e = set(IPv6Default)
	if got := changes(e); got != DNSMixinFile+" set  unset dns.ipv6" || !e.Changes[0].Removed {
		t.Errorf("default: %s", got)
	}
	if s, _ := i.IPv6(ctx); s.Mode != IPv6Off || s.Answers.Pinned() || len(s.Answers.Files) != 0 {
		t.Errorf("default: %+v", s)
	}
	if _, _, err := i.PlanIPv6(ctx, "yes"); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("invalid: %v", err)
	}
}

// TestIPv6Bypass 验证 bypass：只改状态，配置写顶层 ipv6: false；TUN 开着时 inet tunlr 拒绝经 IPv6 的 DNS 查询，
// TUN 关闭、换回 off 时去掉；tun on 的 IPv6 检测跳过，doctor 只在拒绝的规则不在时警告；系统解析不了时改回原样。
func TestIPv6Bypass(t *testing.T) {
	i, h := newGatewayInstaller(t, "1")
	ctx := context.Background()
	i.CheckResolve = func(context.Context) TunCheck { return TunCheck{Name: "dns", Status: StepOK} }
	h.mainIPv6 = "default via fe80::1 dev ens3 proto ra metric 100 pref medium\n"
	h.ipv6Route = "2001:4860:4860::8888 from :: via fe80::1 dev ens3 proto ra metric 100 pref medium\n"
	rejects := func() bool { return strings.Contains(h.tunlr, "chain "+netfilter.IPv6DNSChain+" {") }

	e, err := setIPv6(t, i, IPv6Bypass)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Changed || !e.Bypass || len(e.Changes) != 0 || e.After || !e.Route {
		t.Errorf("bypass: %+v", e)
	}
	if cfg := published(t, i); !strings.Contains(string(cfg), "\nipv6: false\n") || config.TopIPv6(cfg) {
		t.Errorf("published:\n%s", cfg)
	}
	if !tunSwitch(t, i).IPv6Bypass || !rejects() {
		t.Errorf("state %+v, inet tunlr:\n%s", tunSwitch(t, i), h.tunlr)
	}
	if s, _ := i.IPv6(ctx); s.Mode != IPv6Bypass {
		t.Errorf("status: %+v", s)
	}
	if e, plan, _ := i.PlanIPv6(ctx, IPv6Bypass); e.Changed || plan != nil {
		t.Errorf("bypass again: %+v", e)
	}

	// 检测：tun on 的跳过；doctor 在拒绝的规则在时不算泄露，不在时警告，_check 补上。
	if c := i.checkIPv6(ctx); c.Status != StepSkipped || !strings.Contains(c.Message, "bypass") {
		t.Errorf("tun on check: %+v", c)
	}
	st := tunSwitch(t, i)
	if leaks := i.DNSLeaks(ctx, st); slices.ContainsFunc(leaks, func(l DNSLeak) bool { return l.ID == LeakTunIPv6 }) {
		t.Errorf("leaks: %+v", leaks)
	}
	h.tunlr = ""
	if leaks := i.DNSLeaks(ctx, st); !slices.ContainsFunc(leaks, func(l DNSLeak) bool { return l.ID == LeakTunIPv6 && strings.Contains(l.Message, "missing") }) {
		t.Errorf("leaks without the rule: %+v", leaks)
	}
	h.active = true
	if actions, err := i.repairTables(ctx, st); err != nil || !rejects() || !slices.Contains(actions, "reloaded the nftables table inet "+netfilter.Table) {
		t.Errorf("repair: %v, %v", actions, err)
	}

	// TUN 关闭后不再拒绝，重新开启时又拒绝。
	plan, err := i.pipeline(nil).PlanTun(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.TunOff(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if rejects() {
		t.Errorf("TUN off still rejects:\n%s", h.tunlr)
	}
	if _, err := turnOn(t, i); err != nil {
		t.Fatal(err)
	}
	if !rejects() {
		t.Errorf("TUN on again does not reject:\n%s", h.tunlr)
	}

	// off：dns.ipv6 写进 90-dns.yaml，顶层 ipv6 回到 true。
	e, err = setIPv6(t, i, IPv6Off)
	if err != nil {
		t.Fatal(err)
	}
	if e.Bypass || len(e.Changes) != 1 || tunSwitch(t, i).IPv6Bypass || rejects() || !config.TopIPv6(published(t, i)) {
		t.Errorf("off: %+v, inet tunlr:\n%s", e, h.tunlr)
	}

	// 系统解析不了（只有 IPv6 的 DNS 服务器）：改回原样。
	i.CheckResolve = func(context.Context) TunCheck {
		return TunCheck{Name: "dns", Status: StepFailed, Message: "no such host"}
	}
	if _, err := setIPv6(t, i, IPv6Bypass); !errors.Is(err, core.ErrRolledBack) || !strings.Contains(err.Error(), "no such host") {
		t.Errorf("rollback: %v", err)
	}
	if tunSwitch(t, i).IPv6Bypass || rejects() || !config.TopIPv6(published(t, i)) {
		t.Errorf("after rollback: %+v, inet tunlr:\n%s", tunSwitch(t, i), h.tunlr)
	}
}

func TestGatewayIPv6Warning(t *testing.T) {
	for cfg, want := range map[string]string{
		"dns:\n  enable: true\n":                                          "",
		"dns:\n  ipv6: false\n  enhanced-mode: fake-ip\n":                 "",
		"dns:\n  ipv6: true\n  enhanced-mode: fake-ip\n":                  "the fake addresses lead nowhere",
		"dns:\n  ipv6: true\n  enhanced-mode: redir-host\n":               "bypassing mihomo",
		"ipv6: true\ndns:\n  enable: true\n  enhanced-mode: redir-host\n": "",
		"ipv6: false\ndns:\n  ipv6: true\n  enhanced-mode: fake-ip\n":     "",
	} {
		got := gatewayIPv6Warning([]byte(cfg))
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("gatewayIPv6Warning(%q) = %q, want %q", cfg, got, want)
		}
	}
}

// TestCheckIPv6 验证 tun on 的 IPv6 检测：本机没有 IPv6 出口时跳过，有时须经 TUN，不经过时只警告。
func TestCheckIPv6(t *testing.T) {
	i, h := newGatewayInstaller(t, "1")
	ctx := context.Background()
	if c := i.checkIPv6(ctx); c.Status != StepSkipped {
		t.Errorf("no IPv6: %+v", c)
	}
	h.mainIPv6 = "default via fe80::1 dev ens3 proto ra metric 100 pref medium\n"
	h.ipv6Route = "2001:4860:4860::8888 from :: via fe80::1 dev ens3 proto ra metric 100 pref medium\n"
	if c := i.checkIPv6(ctx); c.Status != StepWarn || !strings.Contains(c.Message, "leaves through ens3") {
		t.Errorf("outside TUN: %+v", c)
	}
	h.ipv6Route = "2001:4860:4860::8888 from :: dev tunlr0 table 2022 src fdfe:dcba:9876::1 metric 1024 pref medium\n"
	if c := i.checkIPv6(ctx); c.Status != StepOK {
		t.Errorf("through TUN: %+v", c)
	}
}
