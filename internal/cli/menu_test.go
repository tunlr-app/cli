package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// menuTest 驱动菜单模型：按键经 Update 送入，产生的命令同步执行、消息送回模型。选中的操作直接
// 运行（record 时只记录不运行），再送回结束的消息，相当于 tea.Exec。
type menuTest struct {
	t      *testing.T
	et     *editTest
	m      *menuModel
	data   menuData
	err    error
	record bool
	ran    [][]string
}

func newMenuTest(t *testing.T, d menuData) *menuTest {
	et := newEditTest(t)
	mt := &menuTest{t: t, et: et, data: d}
	mt.m = et.a.newMenu(context.Background(), d)
	mt.m.refresh = 0
	mt.m.load = func(context.Context) (menuData, error) { return mt.data, mt.err }
	mt.do(mt.m.Init())
	mt.send(tea.WindowSizeMsg{Width: 100, Height: 40})
	return mt
}

func (mt *menuTest) send(msg tea.Msg) {
	_, cmd := mt.m.Update(msg)
	mt.do(cmd)
}

func (mt *menuTest) do(cmd tea.Cmd) {
	for _, msg := range collect(cmd) {
		if strings.HasSuffix(fmt.Sprintf("%T", msg), ".execMsg") {
			continue
		}
		mt.send(msg)
	}
	if r := mt.m.running; r != nil {
		mt.ran = append(mt.ran, r.args)
		if !mt.record {
			if err := r.Run(); err != nil {
				mt.t.Fatal(err)
			}
		}
		mt.send(menuDoneMsg{})
	}
}

// collect 执行命令，返回产生的消息（展开 Batch 与 Sequence）。50ms 内没有返回的命令（光标闪烁等
// 定时器）丢弃。
func collect(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(50 * time.Millisecond):
		return nil
	}
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeFor[tea.Cmd]() {
		var msgs []tea.Msg
		for i := range v.Len() {
			msgs = append(msgs, collect(v.Index(i).Interface().(tea.Cmd))...)
		}
		return msgs
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// keys 依次按下 keys：up、down、enter、esc、tab 为对应的键，其他为输入的文字。
func (mt *menuTest) keys(keys ...string) {
	for _, k := range keys {
		switch k {
		case "up":
			mt.send(tea.KeyPressMsg{Code: tea.KeyUp})
		case "down":
			mt.send(tea.KeyPressMsg{Code: tea.KeyDown})
		case "enter":
			mt.send(tea.KeyPressMsg{Code: tea.KeyEnter})
		case "esc":
			mt.send(tea.KeyPressMsg{Code: tea.KeyEscape})
		default:
			for _, r := range k {
				mt.send(tea.KeyPressMsg{Code: r, Text: string(r)})
			}
		}
	}
}

// commands 是执行过的子命令，以 | 分隔。
func (mt *menuTest) commands() string {
	var c []string
	for _, args := range mt.ran {
		c = append(c, strings.Join(args, " "))
	}
	return strings.Join(c, "|")
}

// hovered 返回光标所在的菜单项。
func (mt *menuTest) hovered() string {
	mt.t.Helper()
	if mt.m.adding != nil {
		mt.t.Fatalf("showing the form of %q, not a list", mt.m.adding.label)
	}
	i, ok := mt.m.list.Hovered()
	if !ok {
		mt.t.Fatal("no item hovered")
	}
	return mt.m.items[i].label
}

func (mt *menuTest) view() string {
	return ansi.Strip(mt.m.View().Content)
}

var (
	runningStatus = &setup.Status{
		Installed:    true,
		Running:      "v1.19.33",
		Subscription: "home",
		Revision:     &config.Revision{Number: 7},
		Service:      &service.Status{ActiveState: "active", SubState: "running"},
		Tun:          &setup.TunStatus{Switch: "on"},
		Updates: []setup.Schedule{
			{Task: setup.TaskSubscription, Name: "home", Period: "1d"},
			{Task: setup.TaskSubscription, Name: "spare", Period: "1d"},
			{Task: setup.TaskGeo, Period: "7d"},
		},
	}
	stoppedStatus = &setup.Status{Installed: true, Service: &service.Status{ActiveState: "inactive"},
		Tun: &setup.TunStatus{Switch: "off"}}
)

func menuWith(s *setup.Status, subs, revs int) menuData {
	return menuData{status: s, subs: make([]subscription.Entry, subs), revisions: revs, backups: revs}
}

func TestMenuNavigation(t *testing.T) {
	mt := newMenuTest(t, menuWith(runningStatus, 2, 3))
	view := mt.view()
	if first := strings.SplitN(view, "\n", 2)[0]; first != "tunlr · mihomo running · v1.19.33 · home · revision 7 · TUN on" {
		t.Errorf("status line = %q", first)
	}
	if mt.m.title != "tunlr" || mt.hovered() != "Show status" {
		t.Fatalf("title %q, hovered %q", mt.m.title, mt.hovered())
	}

	mt.keys("down", "enter")
	if mt.m.title != "tunlr › Subscriptions · home · 2 saved" || mt.hovered() != "List subscriptions" {
		t.Fatalf("title %q, hovered %q", mt.m.title, mt.hovered())
	}
	if !strings.Contains(mt.view(), "tunlr · mihomo running") {
		t.Errorf("status line missing in a group:\n%s", mt.view())
	}
	// Esc 回到第一级，光标停在刚才进入的分类上；← Back 也一样。
	mt.keys("esc")
	if mt.m.title != "tunlr" || mt.hovered() != groupSubscriptions {
		t.Fatalf("after esc: title %q, hovered %q", mt.m.title, mt.hovered())
	}
	mt.keys("down", "enter", "up", "enter")
	if mt.m.title != "tunlr" || mt.hovered() != groupConfiguration {
		t.Fatalf("after back: title %q, hovered %q", mt.m.title, mt.hovered())
	}
	if len(mt.ran) != 0 {
		t.Errorf("ran %q", mt.ran)
	}
	// 在第一级按 q 退出，菜单不留在屏幕上。
	mt.keys("q")
	if !mt.m.quitting || mt.view() != "" {
		t.Errorf("quitting = %v, view:\n%s", mt.m.quitting, mt.view())
	}
}

func TestMenuRun(t *testing.T) {
	mt := newMenuTest(t, menuData{status: &setup.Status{}})
	if got := strings.Join(mt.m.labels, "|"); got != "Install tunlr (init)|Show status|Run diagnostics|Quit" {
		t.Fatalf("labels = %q", got)
	}
	if first := strings.SplitN(mt.view(), "\n", 2)[0]; first != "tunlr · not installed" {
		t.Errorf("status line = %q", first)
	}
	mt.keys("down", "enter")
	if mt.commands() != "status" {
		t.Fatalf("ran %q", mt.ran)
	}
	out := mt.et.stdout.String()
	for _, want := range []string{"▶ Show status  $ tunlr status  ·  Ctrl-C to interrupt\n\n", "Service:       not installed", "\n✓ Done ("} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// 结束后回到菜单，光标停在刚才那一项。
	if mt.m.running != nil || mt.hovered() != "Show status" || mt.view() == "" {
		t.Errorf("running %v, hovered %q, view:\n%s", mt.m.running, mt.hovered(), mt.view())
	}
}

func TestMenuResult(t *testing.T) {
	et := newEditTest(t)
	run := func(args ...string) string {
		t.Helper()
		et.stdout.Reset()
		if err := (&menuRun{a: et.a, ctx: context.Background(), label: "Op", args: args}).Run(); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimRight(et.stdout.String(), "\n"), "\n")
		return lines[len(lines)-1]
	}
	if got := run("--version"); !strings.HasPrefix(got, "✓ Done (") {
		t.Errorf("--version: %q", got)
	}
	if got := run("nope"); !strings.HasPrefix(got, "✗ Failed (") {
		t.Errorf("nope: %q", got)
	}
	// 在命令开头的选择中取消：什么都没做，不算失败。
	if err := os.MkdirAll(et.a.layout.MixinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(et.a.layout.MixinDir(), "10-a.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	et.prompt.picks = []string{""}
	if got := run("config", "edit"); got != "– Cancelled" {
		t.Errorf("cancelled edit: %q", got)
	}
}

func TestMenuAddSubscription(t *testing.T) {
	mt := newMenuTest(t, menuWith(runningStatus, 2, 3))
	mt.record = true
	mt.keys("down", "enter", "down", "down", "down", "enter")
	if mt.m.adding == nil || mt.m.adding.label != "Add a subscription" {
		t.Fatalf("not asking: %+v", mt.m.adding)
	}
	// Esc 回到列表，光标停在“添加”上。
	mt.keys("esc")
	if mt.m.title != "tunlr › Subscriptions · home · 2 saved" || mt.hovered() != "Add a subscription" {
		t.Fatalf("after esc: title %q, hovered %q", mt.m.title, mt.hovered())
	}
	// 名称不合法时停在这一项。
	mt.keys("enter", "../bad", "enter")
	if mt.m.adding == nil || len(mt.ran) != 0 {
		t.Fatalf("invalid name accepted: ran %q", mt.ran)
	}
	for range len("../bad") {
		mt.send(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	// 周期从列表中选，光标起始于每天；选每周。
	mt.keys("home", "enter", "https://example.net/sub?token=secret", "enter")
	if mt.m.adding == nil || len(mt.ran) != 0 {
		t.Fatalf("submitted without the period: ran %q", mt.ran)
	}
	mt.keys("down", "enter")
	if mt.commands() != "sub add home https://example.net/sub?token=secret --update 7d" {
		t.Fatalf("ran %q", mt.ran)
	}
	if mt.m.adding != nil || mt.hovered() != "Add a subscription" {
		t.Errorf("after adding: hovered %q", mt.hovered())
	}
	if line := commandLine(mt.ran[0]); strings.Contains(line, "secret") || !strings.HasPrefix(line, "$ tunlr sub add home https://example.net/") {
		t.Errorf("command line = %q", line)
	}
}

func TestMenuRefresh(t *testing.T) {
	mt := newMenuTest(t, menuWith(runningStatus, 2, 3))
	mt.record = true
	mt.keys("up", "up", "up", "enter", "down", "down", "down") // Kernel (mihomo) › Show recent logs
	if mt.hovered() != "Show recent logs" {
		t.Fatalf("hovered %q", mt.hovered())
	}
	// 内核停了：列表随之变化，光标留在原来那一项。
	mt.data = menuWith(stoppedStatus, 2, 3)
	mt.send(menuTickMsg(mt.m.tick))
	if got := strings.Join(mt.m.labels, "|"); got != "Show the kernel and its service|Start mihomo|Show recent logs|Upgrade mihomo to the latest release|Repair the installation (init)|← Back" {
		t.Fatalf("labels = %q", got)
	}
	if mt.hovered() != "Show recent logs" || !strings.Contains(mt.view(), "mihomo not running") {
		t.Errorf("hovered %q, view:\n%s", mt.hovered(), mt.view())
	}

	// 刷新失败时保留原来的状态，状态行给出提示。
	mt.err = errors.New("boom")
	mt.send(menuTickMsg(mt.m.tick))
	if !strings.Contains(mt.view(), "TUN off · not refreshed: boom") {
		t.Errorf("view:\n%s", mt.view())
	}

	// 刷新期间选中的操作等刷新完再执行，期间不显示菜单。
	mt.err = nil
	mt.m.loading = true
	mt.keys("enter")
	if len(mt.ran) != 0 || mt.m.pending == nil || mt.view() != "" {
		t.Fatalf("ran %q, pending %v", mt.ran, mt.m.pending)
	}
	mt.send(menuDataMsg{data: mt.data})
	if mt.commands() != "core logs -n 50" || mt.m.pending != nil {
		t.Fatalf("ran %q, pending %v", mt.ran, mt.m.pending)
	}

	// 过时的定时消息作废。
	mt.data = menuWith(runningStatus, 2, 3)
	mt.send(menuTickMsg(mt.m.tick - 1))
	if strings.Contains(mt.view(), "mihomo running") {
		t.Errorf("stale tick refreshed:\n%s", mt.view())
	}
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
	labels := func(group string, s *setup.Status, subs, revs int) string {
		t.Helper()
		title, items := a.menuItems(group, menuWith(s, subs, revs))
		var l []string
		for _, it := range items {
			l = append(l, it.label)
		}
		return title + ": " + strings.Join(l, "|")
	}

	// 第一级按 --help 的分组排列；分类的标题带上它在第一级右侧的状态。
	for group, want := range map[string]string{
		"":                 "tunlr: Show status|Subscriptions|Configuration|Scheduled updates|Settings|Backup|Transparent proxy|Proxy port|Panel|LAN devices|Filters|Kernel (mihomo)|Run diagnostics|Quit",
		groupSubscriptions: "tunlr › Subscriptions · home · 2 saved: List subscriptions|Switch subscription|Update subscriptions|Add a subscription|Remove subscriptions|← Back",
		groupConfiguration: "tunlr › Configuration · revision 7: Show the configuration, history and mixins|Preview changes|Preview and apply|Edit a mixin|Roll back to an earlier revision|← Back",
		groupUpdates:       "tunlr › Scheduled updates · 2 subscriptions · geo weekly: Show the schedule|Change how often a subscription is fetched|Update everything now|← Back",
		groupSettings:      "tunlr › Settings: Show tool settings|Edit tool settings|← Back",
		groupBackup:        "tunlr › Backup · 3 automatic backups: List automatic backups|Back up to a file|Restore an automatic backup (undo a restore)|Restore from a file|← Back",
		groupTun:           "tunlr › Transparent proxy · on: Turn TUN off|Turn gateway mode on (LAN devices route through mihomo)|Show details|← Back",
		groupProxy:         "tunlr › Proxy port · this server only: Show the proxy address|Let LAN devices use it (HTTP/SOCKS5 proxy)|← Back",
		groupPanel:         "tunlr › Panel · this server only: Show how to open it|Let LAN devices open it|Replace the API secret|← Back",
		groupDevices:       "tunlr › LAN devices · gateway and proxy LAN access off: List devices|List all devices on the LAN|Name a device|Forget a device|← Back",
		groupFilter:        "tunlr › Filters · none: Show filters|Set a device to go through proxies or directly|Send a network direct|Block QUIC (browsers use TCP)|Proxy only common ports (others go direct)|Change the proxied ports|← Back",
		groupKernel:        "tunlr › Kernel (mihomo) · running · v1.19.33: Show the kernel and its service|Restart mihomo|Stop mihomo|Show recent logs|Upgrade mihomo to the latest release|Repair the installation (init)|← Back",
	} {
		if got := labels(group, runningStatus, 2, 3); got != want {
			t.Errorf("running %q:\n got %s\nwant %s", group, got, want)
		}
	}
	_, top := a.menuItems("", menuWith(runningStatus, 2, 3))
	if got := strings.Join(a.menuLabels(top[1:3]), "|"); got != "Subscriptions   home · 2 saved|Configuration   revision 7" {
		t.Errorf("notes = %q", got)
	}

	for group, want := range map[string]string{
		groupSubscriptions: "tunlr › Subscriptions · none active · 1 saved: List subscriptions|Switch subscription|Update subscriptions|Add a subscription|← Back",
		groupConfiguration: "tunlr › Configuration · not published: Show the configuration, history and mixins|Preview changes|Preview and apply|Edit a mixin|← Back",
		groupTun:           "tunlr › Transparent proxy · off: Turn TUN on|Turn gateway mode on (LAN devices route through mihomo)|Show details|← Back",
		groupKernel:        "tunlr › Kernel (mihomo) · not running: Show the kernel and its service|Start mihomo|Show recent logs|Upgrade mihomo to the latest release|Repair the installation (init)|← Back",
		groupBackup:        "tunlr › Backup · no automatic backups: List automatic backups|Back up to a file|Restore from a file|← Back",
	} {
		if got := labels(group, stoppedStatus, 1, 0); got != want {
			t.Errorf("stopped %q:\n got %s\nwant %s", group, got, want)
		}
	}

	// 没安装时分类不存在（例如在分类里卸载了），菜单回到第一级。
	if _, items := a.menuItems(groupKernel, menuData{status: &setup.Status{}}); items != nil {
		t.Errorf("not installed: %v", items)
	}

	// 有可以换回的已知可用内核时才能回滚：当前的是已知可用版本时要有另一份，不是时有一份就行。
	for _, tt := range []struct {
		known   bool
		kernels int
		want    bool
	}{{true, 1, false}, {true, 2, true}, {false, 0, false}, {false, 1, true}} {
		s := *runningStatus
		s.Kernel = &mihomo.KernelStatus{Installed: true, KnownGood: tt.known, Kernels: make([]state.Kernel, tt.kernels)}
		got := strings.Contains(labels(groupKernel, &s, 2, 3), "Roll back to the previous known-good version")
		if got != tt.want {
			t.Errorf("known-good %v, %d kernels: rollback offered = %v", tt.known, tt.kernels, got)
		}
	}
}

// 第一级最上面是需要处理的事项，各对应一个命令；没有时第一项是 Show status。
func TestMenuAttention(t *testing.T) {
	a := &app{}
	attention := func(s *setup.Status, revs int) string {
		t.Helper()
		_, items := a.menuItems("", menuWith(s, 2, revs))
		var l []string
		for _, it := range items {
			if it.label == "Show status" {
				break
			}
			l = append(l, it.label+" = "+strings.Join(it.args, " "))
		}
		return strings.Join(l, "|")
	}
	if got := attention(runningStatus, 3); got != "" {
		t.Errorf("running: %q", got)
	}
	// 从没发布过配置时内核起不来，不提示启动。
	if got := attention(stoppedStatus, 0); got != "" {
		t.Errorf("stopped, nothing published: %q", got)
	}
	if got := attention(stoppedStatus, 3); got != "! Start mihomo (not running) = core start" {
		t.Errorf("stopped: %q", got)
	}

	s := *runningStatus
	s.Service = &service.Status{ActiveState: "failed"}
	s.Running = ""
	s.Drift = &pipeline.Drift{State: pipeline.DriftPending, Sections: []string{"log-level"}}
	s.Updates = []setup.Schedule{
		{Task: setup.TaskSubscription, Name: "home", Last: &state.UpdateRun{Status: setup.StepFailed}},
		{Task: setup.TaskSubscription, Name: "spare", Last: &state.UpdateRun{Status: setup.StepFailed}},
		{Task: setup.TaskGeo, Last: &state.UpdateRun{Status: setup.StepOK}},
	}
	want := "! Publish the changes not yet published = config apply|! Start mihomo (failed) = core start|! Run the failed update again = update run sub"
	if got := attention(&s, 3); got != want {
		t.Errorf("attention:\n got %s\nwant %s", got, want)
	}
	s.Updates[2].Last.Status = setup.StepFailed
	if got := attention(&s, 3); !strings.HasSuffix(got, "|! Run the failed updates again = update run") {
		t.Errorf("both failed: %q", got)
	}
	// 手工改过的配置不算没发布的改动（apply 会覆盖它，留给 Configuration 分类）。
	s.Drift.State = pipeline.DriftEdited
	if got := attention(&s, 3); strings.Contains(got, "Publish") {
		t.Errorf("edited: %q", got)
	}
}

// 列表下方显示光标所在那一项的等价命令；第一级按终端高度尽量一屏放下。
func TestMenuHint(t *testing.T) {
	mt := newMenuTest(t, menuWith(runningStatus, 2, 3))
	last := func() string {
		lines := strings.Split(mt.view(), "\n")
		return lines[len(lines)-1]
	}
	if got := last(); got != "$ tunlr status" {
		t.Errorf("status: %q", got)
	}
	// 分类显示对应的查看命令；退出没有命令。
	mt.keys("down")
	if got := last(); got != "$ tunlr sub" {
		t.Errorf("Subscriptions: %q", got)
	}
	mt.keys("up", "up")
	if got := last(); mt.hovered() != menuQuit || got != "" {
		t.Errorf("hovered %q, hint %q", mt.hovered(), got)
	}
	// 先询问再执行的项显示参数的形式，表单中也显示。
	mt.keys("down", "down", "enter", "down", "down", "down")
	if got := last(); got != "$ tunlr sub add <name> <url> --update <period>" {
		t.Errorf("add: %q", got)
	}
	mt.keys("enter")
	if got := last(); mt.m.adding == nil || got != "$ tunlr sub add <name> <url> --update <period>" {
		t.Errorf("form: %q", got)
	}

	// 40 行的终端放得下第一级的全部 14 项；矮的终端至少显示 maxListRows 行，其余滚动。
	mt.keys("esc", "esc")
	if !strings.Contains(mt.view(), "Run diagnostics") || !strings.Contains(mt.view(), "Quit") {
		t.Errorf("first level does not fit:\n%s", mt.view())
	}
	mt.send(tea.WindowSizeMsg{Width: 100, Height: 12})
	if mt.m.listRows() != maxListRows || strings.Contains(mt.view(), "Run diagnostics") {
		t.Errorf("rows %d, view:\n%s", mt.m.listRows(), mt.view())
	}
	if mt.hovered() != groupSubscriptions {
		t.Errorf("cursor moved on resize: %q", mt.hovered())
	}
	// 大小没变（huh 的表单在 Init 时会再请求一次）时不重建列表，否则会没完没了地重画。
	list := mt.m.list
	mt.send(tea.WindowSizeMsg{Width: 100, Height: 12})
	mt.send(tea.WindowSizeMsg{Width: 90, Height: 14})
	if mt.m.list != list {
		t.Error("the list was rebuilt although the number of rows did not change")
	}
}
