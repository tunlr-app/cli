package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// addSubscriptions 直接写入订阅列表、状态文件和订阅内容（不经网络），active 为启用的订阅。
func addSubscriptions(t *testing.T, a *app, active string, names ...string) {
	t.Helper()
	st, _, err := state.LoadOrNew(a.layout.StateFile(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	l, err := subscription.LoadList(a.layout.SubscriptionList())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		l.Subscriptions = append(l.Subscriptions, subscription.Def{Name: n, URL: "https://example.net/" + n})
		path := a.layout.SubscriptionFile(n)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		sub := "proxies:\n  - {name: " + n + ", type: ss, server: " + n + ".example.net, port: 1, cipher: aes-128-gcm, password: p}\nrules:\n  - MATCH," + n + "\n"
		if err := os.WriteFile(path, []byte(sub), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l.Active = active
	if err := subscription.SaveList(a.layout.SubscriptionList(), l); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(a.layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
}

// subscriptionList 读取订阅列表。
func subscriptionList(t *testing.T, a *app) *subscription.List {
	t.Helper()
	l, err := subscription.LoadList(a.layout.SubscriptionList())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func activeSubscription(t *testing.T, a *app) string {
	t.Helper()
	return subscriptionList(t, a).Active
}

func TestSubUseChooses(t *testing.T) {
	et := newEditTest(t)
	addSubscriptions(t, et.a, "main", "backup", "main")
	et.prompt.choices = []int{0}
	et.run(0, "sub", "use", "--yes")
	if len(et.prompt.asked) != 1 || len(et.prompt.asked[0]) != 2 {
		t.Fatalf("asked = %q", et.prompt.asked)
	}
	opts := et.prompt.asked[0]
	if !strings.HasPrefix(opts[0], "backup  ") || !strings.HasPrefix(opts[1], "main    active  1 proxies") {
		t.Errorf("options = %q", opts)
	}
	if et.prompt.initials[0] != 1 {
		t.Errorf("cursor starts at %d, want the active subscription (1)", et.prompt.initials[0])
	}
	if got := activeSubscription(t, et.a); got != "backup" {
		t.Errorf("active = %q", got)
	}
	if out := et.stdout.String(); !strings.Contains(out, `Switched to "backup"`) || !strings.Contains(out, "Published revision 1") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSubUseCancelled(t *testing.T) {
	et := newEditTest(t)
	addSubscriptions(t, et.a, "main", "main", "backup")
	et.prompt.choices = []int{-1}
	et.run(0, "sub", "use")
	if got := activeSubscription(t, et.a); got != "main" || !strings.Contains(et.stdout.String(), "Cancelled") {
		t.Errorf("active = %q, output:\n%s", got, et.stdout)
	}
}

func TestSubUseNeedsNameWithoutTerminal(t *testing.T) {
	et := newEditTest(t)
	et.a.terminal = func() bool { return false }
	et.run(exitUsage, "sub", "use")
	if !strings.Contains(et.stdout.String(), "sub use needs the name") {
		t.Errorf("output:\n%s", et.stdout)
	}
}

func TestSubRemoveChooses(t *testing.T) {
	et := newEditTest(t)
	addSubscriptions(t, et.a, "main", "main", "a", "b", "c")
	// 第一次在确认时回答否，什么都不删。
	et.prompt.many = [][]int{{0, 2}, {0, 2}}
	et.prompt.confirms = []bool{false, true}
	et.run(0, "sub", "remove")
	if len(et.prompt.confirmed) != 1 || !strings.HasPrefix(et.prompt.confirmed[0], `Remove the subscriptions "a", "c"?|• a  https://example.net/...`) ||
		!strings.Contains(et.stdout.String(), "Nothing removed.") || !et.a.aborted {
		t.Fatalf("confirmed = %q, output:\n%s", et.prompt.confirmed, et.stdout)
	}
	et.run(0, "sub", "remove")
	if len(et.prompt.asked) != 2 || len(et.prompt.asked[0]) != 3 || strings.Contains(strings.Join(et.prompt.asked[0], "\n"), "main") {
		t.Fatalf("the active subscription must not be offered: %q", et.prompt.asked)
	}
	var left []string
	for _, s := range subscriptionList(t, et.a).Subscriptions {
		left = append(left, s.Name)
	}
	if strings.Join(left, ",") != "main,b" {
		t.Errorf("left = %v\n%s", left, et.stdout)
	}

	// 只剩启用的那个以外的一个；什么都没勾选时不删除。
	et.stdout.Reset()
	et.prompt.many = [][]int{{}}
	et.run(0, "sub", "remove")
	if !strings.Contains(et.stdout.String(), "Nothing selected") {
		t.Errorf("output:\n%s", et.stdout)
	}
}

func TestRollbackChooses(t *testing.T) {
	et := newEditTest(t)
	addSubscriptions(t, et.a, "main", "main", "backup")
	et.run(0, "config", "apply", "--yes")
	et.run(0, "sub", "use", "backup", "--apply", "--yes")
	et.stdout.Reset()

	// 修订 2 是生效的配置，光标停在修订 1 上。
	et.prompt.choices = []int{1}
	et.run(0, "config", "rollback", "--dry-run")
	opts := et.prompt.asked[0]
	if len(opts) != 2 || !strings.HasPrefix(opts[0], "#2  ") || !strings.HasSuffix(opts[0], "(current)") || !strings.Contains(opts[1], "main") {
		t.Errorf("options = %q", opts)
	}
	if et.prompt.initials[0] != 1 {
		t.Errorf("cursor starts at %d", et.prompt.initials[0])
	}
	if out := et.stdout.String(); !strings.Contains(out, "Rolling back to revision 1") || !strings.Contains(out, "Dry run") {
		t.Errorf("output:\n%s", out)
	}

	// --yes 时不询问，回到默认的那一份。
	et.stdout.Reset()
	et.run(0, "config", "rollback", "--yes")
	if len(et.prompt.asked) != 1 || !strings.Contains(et.stdout.String(), "Rolling back to revision 1") {
		t.Errorf("asked = %q, output:\n%s", et.prompt.asked, et.stdout)
	}
}

func TestUpdateSetChooses(t *testing.T) {
	et := newEditTest(t)
	addSubscriptions(t, et.a, "main", "main", "backup")
	// 先选订阅，再选周期。
	et.prompt.picks = []string{"backup", "every 6 hours"}
	et.run(0, "update", "set")
	if got := subscriptionList(t, et.a).Subscriptions[1].Update; got != "6h" {
		t.Errorf("backup update = %q\n%s", got, et.stdout)
	}

	// 取消时什么都不改。
	et.stdout.Reset()
	et.prompt.picks = []string{""}
	et.run(0, "update", "set")
	if !strings.Contains(et.stdout.String(), "Cancelled") {
		t.Errorf("output:\n%s", et.stdout)
	}
}
