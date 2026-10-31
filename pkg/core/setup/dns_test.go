package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// TestSetSubscriptionDNS 验证按订阅选择用谁的 DNS：tunlr 写进订阅的 dns mixin，订阅的 dns 不再采用；
// subscription 去掉 dns、sniffer，文件中别的键保留，删空时删除文件；别的 mixin 也不采用时拒绝；订阅不存在、
// 取值不对时报错。
func TestSetSubscriptionDNS(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	ctx := context.Background()
	useSub(t, i, "home", "proxies: []\nrules:\n  - MATCH,DIRECT\ndns:\n  enable: true\n  nameserver: [192.0.2.53]\n")
	if from, _, err := i.SubscriptionDNS("home"); err != nil || from != DNSFromSubscription {
		t.Fatalf("default: %s, %v", from, err)
	}
	w, err := i.SetSubscriptionDNS(ctx, "home", DNSFromTunlr)
	if err != nil || !w.Active {
		t.Fatalf("tunlr: %+v, %v", w, err)
	}
	path := filepath.Join(i.Layout.MixinDir(), "home", DNSMixinFile)
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "tunlr:\n  ignore-from-subscription:\n    - dns\n    - sniffer\n") {
		t.Fatalf("mixin: %v\n%s", err, data)
	}
	if from, file, err := i.SubscriptionDNS("home"); err != nil || from != DNSFromTunlr || file != "home/"+DNSMixinFile {
		t.Fatalf("after: %s %s, %v", from, file, err)
	}
	plan, err := i.pipeline(nil).Plan(ctx)
	if err != nil || strings.Contains(string(plan.Config), "192.0.2.53") || len(plan.Ignored) != 1 || plan.Ignored[0].Key != "dns" {
		t.Fatalf("plan: %v, ignored %v\n%s", err, plan.Ignored, plan.Config)
	}

	// 手写的其他键保留。
	writeFileIn(t, path, "tunlr:\n  ignore-from-subscription: [hosts, dns, sniffer]\n")
	if _, err := i.SetSubscriptionDNS(ctx, "home", DNSFromSubscription); err != nil {
		t.Fatal(err)
	}
	if keys, err := config.MixinIgnores("x", mustRead(t, path)); err != nil || strings.Join(keys, ",") != "hosts" {
		t.Fatalf("kept: %v, %v", keys, err)
	}
	writeFileIn(t, path, "tunlr:\n  ignore-from-subscription: [dns, sniffer]\n")
	if _, err := i.SetSubscriptionDNS(ctx, "home", DNSFromSubscription); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("emptied mixin not removed: %v", err)
	}

	writeFileIn(t, filepath.Join(i.Layout.MixinDir(), "10-mine.yaml"), "tunlr:\n  ignore-from-subscription: [sniffer]\n")
	if _, err := i.SetSubscriptionDNS(ctx, "home", DNSFromSubscription); !errors.Is(err, core.ErrInvalidArgument) || !strings.Contains(err.Error(), "10-mine.yaml") {
		t.Errorf("another mixin: %v", err)
	}
	if _, err := i.SetSubscriptionDNS(ctx, "nope", DNSFromTunlr); err == nil {
		t.Error("unknown subscription accepted")
	}
	if _, err := i.SetSubscriptionDNS(ctx, "home", "mine"); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("unknown choice: %v", err)
	}
}

// TestDNSMode 验证 DNS 模式开关：设了时写进 tunlr:tun 的 dns.enhanced-mode，mixin 不能再写它；回到按配置时
// 不再独占，mixin 写的生效。
func TestDNSMode(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	ctx := context.Background()
	if s, err := i.TunStatus(ctx); err != nil || s.DNSMode != "" || s.PublishedDNSMode != state.DNSFakeIP {
		t.Fatalf("fallbacks: %+v, %v", s, err)
	}
	before, plan, err := i.PlanDNSMode(ctx, state.DNSRedirHost)
	if err != nil || before != "" || !plan.Changed() || !strings.Contains(string(plan.Config), "enhanced-mode: redir-host\n") {
		t.Fatalf("plan: %v\n%s", err, plan.Config)
	}
	if _, err := i.SaveDNSMode(ctx, state.DNSRedirHost, plan); err != nil {
		t.Fatal(err)
	}
	if s, err := i.TunStatus(ctx); err != nil || s.DNSMode != state.DNSRedirHost || s.PublishedDNSMode != state.DNSRedirHost {
		t.Fatalf("saved: %+v, %v", s, err)
	}

	writeFileIn(t, filepath.Join(i.Layout.MixinDir(), "20-mine.yaml"), "mihomo:\n  dns:\n    enhanced-mode: fake-ip\n")
	var me *config.MixinError
	if _, err := i.pipeline(nil).Plan(ctx); !errors.As(err, &me) || !strings.Contains(err.Error(), "tunlr tun dns") {
		t.Errorf("mixin owning the mode: %v", err)
	}
	before, plan, err = i.PlanDNSMode(ctx, "")
	if err != nil || before != state.DNSRedirHost || !strings.Contains(string(plan.Config), "enhanced-mode: fake-ip\n") {
		t.Fatalf("default: %v\n%s", err, plan.Config)
	}
	if _, err := i.SaveDNSMode(ctx, "", plan); err != nil {
		t.Fatal(err)
	}
	if s, err := i.TunStatus(ctx); err != nil || s.DNSMode != "" || s.PublishedDNSMode != state.DNSFakeIP {
		t.Fatalf("back to the configuration: %+v, %v", s, err)
	}
	if _, _, err := i.PlanDNSMode(ctx, "normal"); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("unknown mode: %v", err)
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
