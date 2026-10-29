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

// writeSubscriptionFile 写一份可以当作订阅来源的本地文件，返回它的绝对路径（正斜杠）。
func writeSubscriptionFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".yaml")
	sub := "proxies:\n  - {name: " + name + ", type: ss, server: " + name + ".example.net, port: 1, cipher: aes-128-gcm, password: p}\nrules:\n  - MATCH," + name + "\n"
	if err := os.WriteFile(path, []byte(sub), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(path)
}

// sub add 不带参数时依次问名称、来源、周期和筛选。
func TestSubAddAsks(t *testing.T) {
	et := newEditTest(t)
	file := writeSubscriptionFile(t, "home")
	et.prompt.inputs = []string{"home", " " + file + " ", "", ""}
	et.prompt.choices = []int{0}
	et.run(0, "sub", "add")
	l := subscriptionList(t, et.a)
	if len(l.Subscriptions) != 1 || l.Subscriptions[0].Name != "home" || filepath.ToSlash(l.Subscriptions[0].Path) != file || l.Active != "home" {
		t.Fatalf("list = %+v\n%s", l, et.stdout)
	}
	if !strings.HasPrefix(et.prompt.described[1], "The URL your provider gives") {
		t.Errorf("described = %q", et.prompt.described)
	}
	// 给了名称时只问来源。
	et.prompt.inputs = []string{file, "", ""}
	et.prompt.choices = []int{0}
	et.run(0, "sub", "add", "spare")
	if l := subscriptionList(t, et.a); len(l.Subscriptions) != 2 || l.Subscriptions[1].Name != "spare" {
		t.Errorf("list = %+v\n%s", l, et.stdout)
	}
}

// sub set 不带参数时先选订阅，再选改什么。
func TestSubSetChooses(t *testing.T) {
	et := newEditTest(t)
	addSubscriptions(t, et.a, "main", "main", "backup")
	et.prompt.picks = []string{"backup", "Fetch period", "every 6 hours"}
	et.run(0, "sub", "set")
	if got := subscriptionList(t, et.a).Subscriptions[1].Update; got != "6h" {
		t.Fatalf("backup update = %q\n%s", got, et.stdout)
	}
	opts := et.prompt.asked[1]
	if len(opts) != 4 || opts[0] != "URL or file (now https://example.net/...)" || opts[1] != "User-Agent (now default)" ||
		opts[2] != "Fetch period (now daily)" || opts[3] != "Proxy filter (backup/10-filter)" {
		t.Errorf("options = %q", opts)
	}

	// 换成本地文件：抓取成功才保存；文件没有 User-Agent 一项。
	file := writeSubscriptionFile(t, "backup")
	et.prompt.picks = []string{"URL or file"}
	et.prompt.inputs = []string{file}
	et.run(0, "sub", "set", "backup")
	if d := subscriptionList(t, et.a).Subscriptions[1]; filepath.ToSlash(d.Path) != file || d.URL != "" {
		t.Fatalf("backup = %+v\n%s", d, et.stdout)
	}
	et.prompt.picks = []string{""}
	et.run(0, "sub", "set", "backup")
	if opts := et.prompt.asked[len(et.prompt.asked)-1]; len(opts) != 3 || strings.HasPrefix(opts[1], "User-Agent") {
		t.Errorf("options for a file = %q", opts)
	}

	// 非交互时要给出名称和要改的项。
	et.a.terminal = func() bool { return false }
	et.run(exitUsage, "sub", "set", "backup")
	et.run(0, "sub", "set", "backup", "--update", "off")
	if got := subscriptionList(t, et.a).Subscriptions[1].Update; got != "off" {
		t.Errorf("backup update = %q", got)
	}
}
