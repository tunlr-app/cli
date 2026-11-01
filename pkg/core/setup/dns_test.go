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
	"github.com/tunlr-app/cli/pkg/core/pipeline"
)

// TestSetSubscriptionDNS 验证按订阅选择用谁的 DNS：默认用 tunlr 的；sub 写进订阅的 dns mixin（tunlr.sub-dns: true），
// 订阅的 dns 采用；tunlr 去掉它，文件中别的键保留，删空时删除文件；共用的 mixin 仍采用时写 false；
// 订阅不存在、取值不对时报错。
func TestSetSubscriptionDNS(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	ctx := context.Background()
	useSub(t, i, "home", "proxies: []\nrules:\n  - MATCH,DIRECT\ndns:\n  enable: true\n  nameserver: [192.0.2.53]\n")
	if from, _, err := i.SubscriptionDNS("home"); err != nil || from != DNSFromTunlr {
		t.Fatalf("default: %s, %v", from, err)
	}
	plan, err := i.pipeline(nil).Plan(ctx)
	if err != nil || strings.Contains(string(plan.Config), "192.0.2.53") || !slices.Equal(plan.UnusedDNS, []string{"dns"}) {
		t.Fatalf("default plan: %v, unused %v", err, plan.UnusedDNS)
	}

	w, err := i.SetSubscriptionDNS(ctx, "home", DNSFromSubscription)
	if err != nil || !w.Active || len(w.Changes) != 1 || w.Changes[0].Name != "home/"+SubscriptionDNSMixinFile {
		t.Fatalf("sub: %+v, %v", w, err)
	}
	path := filepath.Join(i.Layout.MixinDir(), "home", SubscriptionDNSMixinFile)
	if data := mustRead(t, path); !strings.Contains(string(data), "tunlr:\n  sub-dns: true\n") {
		t.Fatalf("mixin:\n%s", data)
	}
	if from, file, err := i.SubscriptionDNS("home"); err != nil || from != DNSFromSubscription || file != "home/"+SubscriptionDNSMixinFile {
		t.Fatalf("after: %s %s, %v", from, file, err)
	}
	if plan, err := i.pipeline(nil).Plan(ctx); err != nil || !strings.Contains(string(plan.Config), "192.0.2.53") || plan.SubDNS == "" {
		t.Fatalf("plan: %v", err)
	}
	if w, err := i.SetSubscriptionDNS(ctx, "home", DNSFromSubscription); err != nil || len(w.Changes) != 0 {
		t.Errorf("again: %+v, %v", w, err)
	}

	// 手写的其他内容保留；删空时删除文件。
	writeFileIn(t, path, "tunlr:\n  sub-dns: true\nmihomo:\n  hosts:\n    a.example: 10.0.0.1\n")
	if _, err := i.SetSubscriptionDNS(ctx, "home", DNSFromTunlr); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, path)); strings.Contains(got, "sub-dns") || !strings.Contains(got, "a.example") {
		t.Fatalf("kept:\n%s", got)
	}
	writeFileIn(t, path, "tunlr:\n  sub-dns: true\n")
	if _, err := i.SetSubscriptionDNS(ctx, "home", DNSFromTunlr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("emptied mixin not removed: %v", err)
	}

	// 共用的 mixin 采用订阅的：tunlr 在订阅自己的 mixin 中写 false，共用的不动。
	shared := filepath.Join(i.Layout.MixinDir(), "10-mine.yaml")
	writeFileIn(t, shared, "tunlr:\n  sub-dns: true\n")
	if from, _, _ := i.SubscriptionDNS("home"); from != DNSFromSubscription {
		t.Fatalf("shared: %s", from)
	}
	if _, err := i.SetSubscriptionDNS(ctx, "home", DNSFromTunlr); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, path)); !strings.Contains(got, "sub-dns: false") || string(mustRead(t, shared)) != "tunlr:\n  sub-dns: true\n" {
		t.Errorf("false over the shared mixin:\n%s", got)
	}
	if from, _, _ := i.SubscriptionDNS("home"); from != DNSFromTunlr {
		t.Errorf("after false: %s", from)
	}
	// 再改回 sub：改的是现在决定它的订阅 mixin。
	if w, err := i.SetSubscriptionDNS(ctx, "home", DNSFromSubscription); err != nil || len(w.Changes) != 1 || !strings.Contains(string(mustRead(t, path)), "sub-dns: true") {
		t.Errorf("back to sub: %+v, %v", w, err)
	}

	if _, err := i.SetSubscriptionDNS(ctx, "nope", DNSFromTunlr); err == nil {
		t.Error("unknown subscription accepted")
	}
	if _, err := i.SetSubscriptionDNS(ctx, "home", "mine"); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("unknown choice: %v", err)
	}
}

// TestDNSMode 验证 DNS 模式写进 mixin：没有 mixin 写它时写进 DNSMixinFile；mixin 已经写了时改那个文件；
// default 从所有写了它的文件中删除；已经是这样时没有修改。
func TestDNSMode(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	ctx := context.Background()
	if d, err := i.DNSMode(ctx); err != nil || d.Mode != DNSFakeIP || d.From != pipeline.ManagedDefaults || !d.Fallback || d.Pinned() {
		t.Fatalf("fallbacks: %+v, %v", d, err)
	}
	if e, plan, err := i.PlanDNSMode(ctx, ""); err != nil || len(e.Changes) != 0 || plan != nil {
		t.Fatalf("default already: %+v, %v", e, err)
	}
	e, plan, err := i.PlanDNSMode(ctx, DNSRedirHost)
	if err != nil || len(e.Changes) != 1 || e.Changes[0].Name != DNSMixinFile || !strings.Contains(string(plan.Config), "enhanced-mode: redir-host\n") {
		t.Fatalf("plan: %+v, %v", e, err)
	}
	if _, err := i.SaveDNSMode(ctx, e, plan); err != nil {
		t.Fatal(err)
	}
	ours := filepath.Join(i.Layout.MixinDir(), DNSMixinFile)
	if got := string(mustRead(t, ours)); !strings.Contains(got, "mihomo:\n  dns:\n    enhanced-mode: redir-host\n") || !strings.HasPrefix(got, "# DNS settings") {
		t.Fatalf("mixin:\n%s", got)
	}
	if s, err := i.TunStatus(ctx); err != nil || s.DNSMode != DNSRedirHost {
		t.Fatalf("published: %+v, %v", s, err)
	}
	if d, _ := i.DNSMode(ctx); !d.Pinned() || d.From != DNSMixinFile {
		t.Errorf("pinned: %+v", d)
	}
	if e, plan, err := i.PlanDNSMode(ctx, DNSRedirHost); err != nil || len(e.Changes) != 0 || plan != nil {
		t.Errorf("already: %+v, %v", e, err)
	}

	// 手写的 mixin 在后面写了它：改的是那个文件。
	mine := filepath.Join(i.Layout.MixinDir(), "95-mine.yaml")
	writeFileIn(t, mine, "# mine\nmihomo:\n  dns:\n    enhanced-mode: fake-ip\n    ipv6: false\n")
	e, plan, err = i.PlanDNSMode(ctx, DNSRedirHost)
	if err != nil || len(e.Changes) != 1 || e.Changes[0].Name != "95-mine.yaml" || e.Before.From != "95-mine.yaml" {
		t.Fatalf("edit where it is set: %+v, %v", e, err)
	}
	if _, err := i.SaveDNSMode(ctx, e, plan); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, mine)); !strings.Contains(got, "# mine") || !strings.Contains(got, "enhanced-mode: redir-host") || !strings.Contains(got, "ipv6: false") {
		t.Errorf("mine:\n%s", got)
	}

	// default 从两个文件中都删除；删空的 DNSMixinFile 被删除，手写的其他键保留。
	e, plan, err = i.PlanDNSMode(ctx, "")
	if err != nil || len(e.Changes) != 2 || !slices.Equal(e.Before.Files, []string{DNSMixinFile, "95-mine.yaml"}) || !e.Changes[0].Removed {
		t.Fatalf("default: %+v, %v", e, err)
	}
	if _, err := i.SaveDNSMode(ctx, e, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Errorf("emptied mixin not removed: %v", err)
	}
	if got := string(mustRead(t, mine)); strings.Contains(got, "enhanced-mode") || !strings.Contains(got, "ipv6: false") {
		t.Errorf("mine after default:\n%s", got)
	}
	if d, _ := i.DNSMode(ctx); d.Mode != DNSFakeIP || d.Pinned() {
		t.Errorf("back to the fallbacks: %+v", d)
	}
	if _, _, err := i.PlanDNSMode(ctx, "normal"); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("unknown mode: %v", err)
	}
}

// TestDNSServers 验证问谁：读出生效的和 tunlr 的兜底值；中国的写进 nameserver-policy（加 #DIRECT）和
// proxy-server-nameserver，其他的写进 nameserver；与现在相同的不写；清空、reset 删除你的设置。
func TestDNSServers(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	ctx := context.Background()
	s, err := i.DNSServers(ctx)
	if err != nil || !slices.Equal(s.China.Servers, []string{"https://223.5.5.5/dns-query", "https://1.12.12.12/dns-query"}) ||
		!slices.Equal(s.China.Default, s.China.Servers) || !s.Other.Fallback || len(s.Other.Servers) != 2 {
		t.Fatalf("fallbacks: %+v, %v", s, err)
	}

	// 中国的不变，不写；其他的写进 nameserver。
	e, plan, err := i.PlanDNSServers(ctx, DNSServersChange{China: s.China.Servers, Other: []string{"tls://9.9.9.9"}})
	if err != nil || len(e.Changes) != 1 || !slices.Equal(e.Changes[0].Set, []string{"dns.nameserver"}) {
		t.Fatalf("other: %+v, %v", e, err)
	}
	if _, err := i.SaveDNSServers(ctx, e, plan); err != nil {
		t.Fatal(err)
	}
	e, plan, err = i.PlanDNSServers(ctx, DNSServersChange{China: []string{"223.5.5.5", "system"}})
	if err != nil || len(e.Changes) != 1 {
		t.Fatalf("china: %+v, %v", e, err)
	}
	if _, err := i.SaveDNSServers(ctx, e, plan); err != nil {
		t.Fatal(err)
	}
	cfg := string(mustRead(t, i.Layout.CoreConfig()))
	for _, want := range []string{"\n    geosite:cn:\n      - 223.5.5.5#DIRECT\n      - system\n", "\n  proxy-server-nameserver:\n    - 223.5.5.5\n    - system\n",
		"\n  nameserver:\n    - tls://9.9.9.9\n"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q:\n%s", want, cfg)
		}
	}
	if s, _ := i.DNSServers(ctx); !slices.Equal(s.China.Servers, []string{"223.5.5.5", "system"}) || s.China.From != DNSMixinFile {
		t.Errorf("read back: %+v", s)
	}
	if e, plan, err := i.PlanDNSServers(ctx, DNSServersChange{China: []string{"223.5.5.5", "system"}}); err != nil || len(e.Changes) != 0 || plan != nil {
		t.Errorf("unchanged: %+v, %v", e, err)
	}

	// 清空其他的：删除 nameserver，回到兜底值；reset 删除全部，文件随之删除。
	e, plan, err = i.PlanDNSServers(ctx, DNSServersChange{Other: []string{}})
	if err != nil || len(e.Changes) != 1 || !slices.Equal(e.Changes[0].Unset, []string{"dns.nameserver"}) {
		t.Fatalf("clear other: %+v, %v", e, err)
	}
	if _, err := i.SaveDNSServers(ctx, e, plan); err != nil {
		t.Fatal(err)
	}
	e, plan, err = i.PlanDNSServers(ctx, DNSServersChange{Reset: true})
	if err != nil || len(e.Changes) != 1 || !e.Changes[0].Removed {
		t.Fatalf("reset: %+v, %v", e, err)
	}
	if _, err := i.SaveDNSServers(ctx, e, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(i.Layout.MixinDir(), DNSMixinFile)); !os.IsNotExist(err) {
		t.Errorf("emptied mixin not removed: %v", err)
	}

	// 发布后放回原来的。
	e, plan, err = i.PlanDNSServers(ctx, DNSServersChange{Other: []string{"9.9.9.9"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.SaveDNSServers(ctx, e, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := i.RevertDNSServers(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, other := config.DNSServers(mustRead(t, i.Layout.CoreConfig())); slices.Contains(other, "9.9.9.9") {
		t.Errorf("not reverted: %v", other)
	}

	for in, ok := range map[string]bool{"223.5.5.5": true, "223.5.5.5:53": true, "[2400:3200::1]:53": true, "system": true,
		"https://dns.alidns.com/dns-query#DIRECT": true, "dhcp://eth0": true, "quic://dns.adguard.com": true,
		"dns.google": false, "ftp://x": false, "https://": false} {
		if _, err := ParseDNSServers(in); (err == nil) != ok {
			t.Errorf("ParseDNSServers(%q): %v", in, err)
		}
	}
	if got, _ := ParseDNSServers(" 223.5.5.5 \n\n223.5.5.5\nsystem\n"); !slices.Equal(got, []string{"223.5.5.5", "system"}) {
		t.Errorf("parsed: %q", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
