package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/huh/v2"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

func TestMenuNotInstalled(t *testing.T) {
	et := newEditTest(t)
	et.prompt.picks = []string{"Show status", menuQuit}
	et.run(0)
	if len(et.prompt.asked) != 2 || strings.Join(et.prompt.asked[0], "|") != "Install tunlr (init)|Show status|Quit" {
		t.Fatalf("asked = %q", et.prompt.asked)
	}
	// 回到菜单时光标停在刚才那一项。
	if et.prompt.initials[0] != 0 || et.prompt.initials[1] != 1 {
		t.Errorf("initials = %v", et.prompt.initials)
	}
	// 操作结束后停在结果页。
	if strings.Join(et.prompt.paused, "|") != "✓ Done" {
		t.Errorf("paused = %q", et.prompt.paused)
	}
	out := et.stdout.String()
	for _, want := range []string{"Show status  $ tunlr status", "Service:       not installed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestMenuCancelled(t *testing.T) {
	et := newEditTest(t)
	et.prompt.picks = []string{""}
	et.run(0)
}

func TestMenuWithoutTerminalShowsHelp(t *testing.T) {
	et := newEditTest(t)
	et.a.terminal = func() bool { return false }
	et.run(0)
	if !strings.Contains(et.stdout.String(), "Usage:") {
		t.Errorf("output:\n%s", et.stdout)
	}
}

func TestMenuItemResult(t *testing.T) {
	et := newEditTest(t)
	ctx := context.Background()
	run := func(it menuItem) {
		t.Helper()
		if err := et.a.runMenuItem(ctx, it, globalOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	run(menuItem{label: "Version", args: []string{"version"}})
	run(menuItem{label: "Bad", args: []string{"nope"}})
	// 在添加订阅的表单中取消：不执行命令，不显示结果页。
	run(menuItem{label: "Add", prepare: func(context.Context) ([]string, error) { return nil, huh.ErrUserAborted }})

	// 在命令开头的选择中取消：什么都没做，直接回到菜单。
	if err := os.MkdirAll(et.a.layout.MixinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(et.a.layout.MixinDir(), "10-a.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	et.prompt.picks = []string{""}
	run(menuItem{label: "Edit a mixin", args: []string{"edit"}})

	if got := strings.Join(et.prompt.paused, "|"); got != "✓ Done|✗ Failed" {
		t.Errorf("paused = %q", got)
	}
}

func TestMenuItems(t *testing.T) {
	a := &app{}
	labels := func(group string, s *setup.Status, subs, revs int) string {
		t.Helper()
		var entries []subscription.Entry
		for range subs {
			entries = append(entries, subscription.Entry{})
		}
		title, items := a.menuItems(group, menuData{s, entries, revs})
		var l []string
		for _, it := range items {
			l = append(l, it.label)
		}
		return title + ": " + strings.Join(l, "|")
	}

	running := &setup.Status{
		Installed:    true,
		Running:      "v1.19.33",
		Subscription: "home",
		Revision:     &config.Revision{Number: 7},
		Service:      &service.Status{ActiveState: "active", SubState: "running"},
		Tun:          &setup.TunStatus{Switch: "on"},
	}
	for group, want := range map[string]string{
		"":                 "tunlr: Show status|Subscriptions|Configuration|Transparent proxy|mihomo service|Quit",
		groupSubscriptions: "tunlr › Subscriptions: List subscriptions|Switch subscription|Update subscriptions|Add a subscription|Remove subscriptions|← Back",
		groupConfiguration: "tunlr › Configuration: Preview changes|Preview and apply|Edit a mixin|Roll back|List revisions|← Back",
		groupTun:           "tunlr › Transparent proxy: Turn TUN off|Show details|← Back",
		groupService:       "tunlr › mihomo service: Restart mihomo|Stop mihomo|Show recent logs|Upgrade mihomo to the latest release|← Back",
	} {
		if got := labels(group, running, 2, 3); got != want {
			t.Errorf("running %q:\n got %s\nwant %s", group, got, want)
		}
	}
	_, top := a.menuItems("", menuData{running, make([]subscription.Entry, 2), 3})
	if got := strings.Join(a.menuLabels(top[1:3]), "|"); got != "Subscriptions   home · 2 saved|Configuration   revision 7" {
		t.Errorf("notes = %q", got)
	}

	stopped := &setup.Status{Installed: true, Service: &service.Status{ActiveState: "inactive"}, Tun: &setup.TunStatus{Switch: "off"}}
	for group, want := range map[string]string{
		groupSubscriptions: "tunlr › Subscriptions: List subscriptions|Switch subscription|Update subscriptions|Add a subscription|← Back",
		groupConfiguration: "tunlr › Configuration: Preview changes|Preview and apply|Edit a mixin|← Back",
		groupTun:           "tunlr › Transparent proxy: Turn TUN on|Show details|← Back",
		groupService:       "tunlr › mihomo service: Start mihomo|Show recent logs|Upgrade mihomo to the latest release|← Back",
	} {
		if got := labels(group, stopped, 1, 0); got != want {
			t.Errorf("stopped %q:\n got %s\nwant %s", group, got, want)
		}
	}

	// 没安装时分类不存在（例如在分类里卸载了），菜单回到第一级。
	if _, items := a.menuItems(groupService, menuData{status: &setup.Status{}}); items != nil {
		t.Errorf("not installed: %v", items)
	}
}

func TestAskSubscription(t *testing.T) {
	p := &scriptedPrompt{t: t, inputs: []string{"home", "https://example.net/sub?token=secret"}}
	a := &app{prompt: p}
	args, err := a.askSubscription(context.Background())
	if err != nil || strings.Join(args, " ") != "sub add home https://example.net/sub?token=secret" {
		t.Fatalf("args = %q, %v", args, err)
	}
	if line := commandLine(args); strings.Contains(line, "secret") || !strings.HasPrefix(line, "$ tunlr sub add home https://example.net/") {
		t.Errorf("command line = %q", line)
	}

	p.inputs = []string{"../bad", ""}
	if _, err := a.askSubscription(context.Background()); err == nil {
		t.Error("invalid name accepted")
	}
}
