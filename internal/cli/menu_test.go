package cli

import (
	"context"
	"strings"
	"testing"

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
	out := et.stdout.String()
	for _, want := range []string{"tunlr is not installed yet", "$ tunlr status", "Service:       not installed"} {
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

func TestMenuItems(t *testing.T) {
	a := &app{}
	labels := func(s *setup.Status, subs, revs int) string {
		var entries []subscription.Entry
		for range subs {
			entries = append(entries, subscription.Entry{})
		}
		var l []string
		for _, it := range a.menuItems(s, entries, revs) {
			l = append(l, it.label)
		}
		return strings.Join(l, "|")
	}

	running := &setup.Status{
		Installed: true,
		Service:   &service.Status{ActiveState: "active", SubState: "running"},
		Tun:       &setup.TunStatus{Switch: "on"},
	}
	got := labels(running, 2, 3)
	for _, want := range []string{"Switch subscription", "Remove subscriptions", "Roll back", "TUN) off", "Restart mihomo|Stop mihomo"} {
		if !strings.Contains(got, want) {
			t.Errorf("running: lacks %q in %s", want, got)
		}
	}

	stopped := &setup.Status{Installed: true, Service: &service.Status{ActiveState: "inactive"}, Tun: &setup.TunStatus{Switch: "off"}}
	got = labels(stopped, 1, 0)
	for _, want := range []string{"Start mihomo", "TUN) on", "Switch subscription"} {
		if !strings.Contains(got, want) {
			t.Errorf("stopped: lacks %q in %s", want, got)
		}
	}
	for _, unwanted := range []string{"Remove subscriptions", "Roll back", "Stop mihomo", "uninstall"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("stopped: offers %q in %s", unwanted, got)
		}
	}
	if !strings.HasSuffix(got, "|"+menuQuit) {
		t.Errorf("Quit is not last: %s", got)
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

	p.inputs = []string{"../bad"}
	if _, err := a.askSubscription(context.Background()); err == nil {
		t.Error("invalid name accepted")
	}
}
