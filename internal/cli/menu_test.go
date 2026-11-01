package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	i, ok := mt.m.list.Hovered()
	if !ok {
		mt.t.Fatal("no item hovered")
	}
	return mt.m.items[i].label
}

func (mt *menuTest) view() string {
	return ansi.Strip(mt.m.View().Content)
}

// hintLines 返回列表下方的两行：说明和等价命令。
func (mt *menuTest) hintLines() (desc, command string) {
	lines := strings.Split(mt.view(), "\n")
	return lines[len(lines)-2], lines[len(lines)-1]
}

var (
	runningStatus = &setup.Status{
		Installed:    true,
		Running:      "v1.19.33",
		Subscription: "home",
		Revision:     &config.Revision{Number: 7},
		Service:      &service.Status{ActiveState: "active", SubState: "running"},
		Tun:          &setup.TunStatus{Switch: "on", DNSMode: "fake-ip"},
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

	// 进入分类：标题下是这一分类的状态。
	mt.keys("down", "enter")
	if mt.m.title != "tunlr › Subscriptions" || mt.hovered() != "Add a sub" {
		t.Fatalf("title %q, hovered %q", mt.m.title, mt.hovered())
	}
	if v := mt.view(); !strings.Contains(v, "tunlr · mihomo running") || !strings.Contains(v, "home · 2 saved") {
		t.Errorf("status line or note missing in a group:\n%s", v)
	}
	// Esc 回到第一级，光标停在刚才进入的分类上；← Back 也一样。
	mt.keys("esc")
	if mt.m.title != "tunlr" || mt.hovered() != groupSubs {
		t.Fatalf("after esc: title %q, hovered %q", mt.m.title, mt.hovered())
	}
	mt.keys("down", "enter", "up", "enter")
	if mt.m.title != "tunlr" || mt.hovered() != groupConfig {
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
	if got := strings.Join(mt.m.labels, "|"); got != "Install tunlr|Show status|Diagnose|Quit" {
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

// 菜单不嵌表单：需要提问的项直接执行子命令，由子命令自己问。
func TestMenuRunsWithoutForm(t *testing.T) {
	mt := newMenuTest(t, menuWith(runningStatus, 2, 3))
	mt.record = true
	mt.keys("down", "enter", "enter")
	if mt.commands() != "sub add" || mt.hovered() != "Add a sub" {
		t.Fatalf("ran %q, hovered %q", mt.ran, mt.hovered())
	}
}

func TestMenuRefresh(t *testing.T) {
	mt := newMenuTest(t, menuWith(runningStatus, 2, 3))
	mt.record = true
	mt.keys("up", "up", "up", "up", "enter", "down", "down") // mihomo › Recent logs
	if mt.hovered() != "Recent logs" {
		t.Fatalf("hovered %q", mt.hovered())
	}
	// 内核停了：列表随之变化，光标留在原来那一项。
	mt.data = menuWith(stoppedStatus, 2, 3)
	mt.send(menuTickMsg(mt.m.tick))
	if got := strings.Join(mt.m.labels, "|"); got != "Start mihomo|Recent logs|Upgrade mihomo|mihomo details|← Back" {
		t.Fatalf("labels = %q", got)
	}
	if mt.hovered() != "Recent logs" || !strings.Contains(mt.view(), "mihomo not running") {
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
		title, _, items := a.menuItems(group, menuWith(s, subs, revs))
		var l []string
		for _, it := range items {
			l = append(l, it.label)
		}
		return title + ": " + strings.Join(l, "|")
	}

	for group, want := range map[string]string{
		"":            "tunlr: Show status|Subscriptions|Config|Traffic|Filters|LAN devices|mihomo|Panel|Maintenance|Quit",
		groupSubs:     "tunlr › Subscriptions: Add a sub|Switch sub|Change a sub|Fetch subs now|Remove a sub|List subs|← Back",
		groupConfig:   "tunlr › Config: Edit a mixin|Publish config|Roll back config|Update geo data now|Config details|← Back",
		groupTraffic:  "tunlr › Traffic: Turn TUN off|Turn gateway on|Open proxy port to LAN|DNS|TUN details|Proxy port details|← Back",
		groupFilters:  "tunlr › Filters: Block QUIC|Proxy some ports only|Send a network direct|Filter details|← Back",
		groupCore:     "tunlr › mihomo: Restart mihomo|Stop mihomo|Recent logs|Upgrade mihomo|mihomo details|← Back",
		groupPanel:    "tunlr › Panel: Open panel to LAN|New API secret|Open the panel|← Back",
		groupMaintain: "tunlr › Maintenance: Back up to a file|Restore a backup|Edit settings|Diagnose|Repair install|← Back",
	} {
		if got := labels(group, runningStatus, 2, 3); got != want {
			t.Errorf("running %q:\n got %s\nwant %s", group, got, want)
		}
	}
	// 分类右侧的状态，也是进入后标题下的说明。
	_, _, top := a.menuItems("", menuWith(runningStatus, 2, 3))
	want := []string{
		"Subscriptions   home · 2 saved",
		"Config          revision 7",
		"Traffic         TUN on · proxy port this server only",
		"Filters         none",
		"LAN devices     none: gateway and proxy port LAN access are off",
		"mihomo          running · v1.19.33",
		"Panel           this server only",
		"Maintenance     3 automatic backups",
	}
	if got := a.menuLabels(top)[1:9]; !slices.Equal(got, want) {
		t.Errorf("notes:\n got %q\nwant %q", got, want)
	}
	if _, note, _ := a.menuItems(groupTraffic, menuWith(runningStatus, 2, 3)); note != top[3].note {
		t.Errorf("group note %q, first-level note %q", note, top[3].note)
	}

	// 只代理部分端口开启时，改列表和恢复全部是两项；说明中是当前的列表。
	ports := *runningStatus
	ports.Filter = &setup.FilterStatus{Ports: true, PortList: []string{"80", "443"}, Sources: []string{"10.0.0.0/8"}}
	_, _, items := a.menuItems(groupFilters, menuWith(&ports, 2, 3))
	k := slices.IndexFunc(items, func(it menuItem) bool { return it.label == "Change proxied ports" })
	if k < 0 || items[k].desc != "Now: 80,443" || items[k+1].label != "Proxy all ports" ||
		slices.ContainsFunc(items, func(it menuItem) bool { return it.label == "Proxy some ports only" }) ||
		!slices.ContainsFunc(items, func(it menuItem) bool { return it.label == "Stop sending a network direct" }) {
		t.Errorf("ports on: %+v", items)
	}
	// Traffic 最满时（TUN 开着、代理端口开放给局域网）正好 7 项。
	lan := *runningStatus
	lan.ProxyLAN = true
	if got := labels(groupTraffic, &lan, 2, 3); got != "tunlr › Traffic: Turn TUN off|Turn gateway on|Close proxy port to LAN|"+
		"Change LAN interfaces|DNS|TUN details|Proxy port details|← Back" {
		t.Errorf("proxy port open: %s", got)
	}

	for group, want := range map[string]string{
		groupSubs:    "tunlr › Subscriptions: Add a sub|Change a sub|Fetch subs now|List subs|← Back",
		groupConfig:  "tunlr › Config: Edit a mixin|Publish config|Config details|← Back",
		groupTraffic: "tunlr › Traffic: Turn TUN on|Turn gateway on|Open proxy port to LAN|DNS|TUN details|Proxy port details|← Back",
		groupCore:    "tunlr › mihomo: Start mihomo|Recent logs|Upgrade mihomo|mihomo details|← Back",
	} {
		if got := labels(group, stoppedStatus, 1, 0); got != want {
			t.Errorf("stopped %q:\n got %s\nwant %s", group, got, want)
		}
	}
	if got := labels(groupSubs, stoppedStatus, 0, 0); got != "tunlr › Subscriptions: Add a sub|List subs|← Back" {
		t.Errorf("no subs: %s", got)
	}

	// 每一项都有说明，执行的项都有命令；每个分类不超过 7 项，第一级 10 行。
	for _, group := range []string{"", groupSubs, groupConfig, groupTraffic, groupFilters, groupCore, groupPanel, groupMaintain} {
		_, _, items := a.menuItems(group, menuWith(runningStatus, 2, 3))
		if limit := 8; group != "" && len(items) > limit || group == "" && len(items) > maxListRows {
			t.Errorf("%q has %d items", group, len(items))
		}
		for _, it := range items {
			if it.back || it.quit {
				continue
			}
			if it.desc == "" || it.group == "" && it.args == nil || it.group != "" && len(it.nouns) == 0 {
				t.Errorf("%q › %q: desc %q, args %q, nouns %q", group, it.label, it.desc, it.args, it.nouns)
			}
		}
	}

	// 没安装时分类不存在（例如在分类里卸载了），菜单回到第一级。
	if _, _, items := a.menuItems(groupCore, menuData{status: &setup.Status{}}); items != nil {
		t.Errorf("not installed: %v", items)
	}
	// LAN devices 不是分类，直接执行 tunlr devices（在表格中选设备）；状态写在右侧。
	if k := slices.IndexFunc(top, func(it menuItem) bool { return it.label == groupDevices }); k < 0 || top[k].group != "" ||
		!slices.Equal(top[k].args, []string{"devices"}) {
		t.Errorf("LAN devices: %+v", top)
	}

	// 有可以换回的已知可用内核时才能回滚：当前的是已知可用版本时要有另一份，不是时有一份就行。
	for _, tt := range []struct {
		known   bool
		kernels int
		want    bool
	}{{true, 1, false}, {true, 2, true}, {false, 0, false}, {false, 1, true}} {
		s := *runningStatus
		s.Kernel = &mihomo.KernelStatus{Installed: true, KnownGood: tt.known, Kernels: make([]state.Kernel, tt.kernels)}
		got := strings.Contains(labels(groupCore, &s, 2, 3), "Roll back mihomo")
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
		_, _, items := a.menuItems("", menuWith(s, 2, revs))
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
		{Task: setup.TaskGeo},
	}
	want := "! Publish the changes not yet published = config apply|! Start mihomo (failed) = core start|! Fetch subs again (the last fetch failed) = sub update"
	if got := attention(&s, 3); got != want {
		t.Errorf("attention:\n got %s\nwant %s", got, want)
	}
	// 手工改过的配置不算没发布的改动（apply 会覆盖它，留给 Config 分类）。
	s.Drift.State = pipeline.DriftEdited
	if got := attention(&s, 3); strings.Contains(got, "Publish") {
		t.Errorf("edited: %q", got)
	}
}

// 列表下方两行：光标所在那一项的说明和等价命令；一级按终端高度尽量一屏放下。
func TestMenuHint(t *testing.T) {
	mt := newMenuTest(t, menuWith(runningStatus, 2, 3))
	if desc, cmd := mt.hintLines(); cmd != "$ tunlr status" || !strings.HasPrefix(desc, "The service, kernel") {
		t.Errorf("status: %q / %q", desc, cmd)
	}
	// 分类的命令行是它对应的顶层命令；退出两行都空。
	mt.keys("down")
	if desc, cmd := mt.hintLines(); cmd != "$ tunlr sub" || !strings.HasPrefix(desc, "Add, switch, change") {
		t.Errorf("Subscriptions: %q / %q", desc, cmd)
	}
	mt.keys("down", "down")
	if _, cmd := mt.hintLines(); cmd != "$ tunlr tun  ·  tunlr proxy  ·  tunlr dns" {
		t.Errorf("Traffic: %q", cmd)
	}
	mt.keys("up", "up")
	mt.keys("up", "up")
	if desc, cmd := mt.hintLines(); mt.hovered() != menuQuit || desc != "" || cmd != "" {
		t.Errorf("hovered %q, hint %q / %q", mt.hovered(), desc, cmd)
	}
	mt.keys("down", "down", "enter")
	if _, cmd := mt.hintLines(); cmd != "$ tunlr sub add" {
		t.Errorf("add: %q", cmd)
	}

	// 矮的终端至少显示 maxListRows 行，每个分类都放得下；光标不动。
	mt.send(tea.WindowSizeMsg{Width: 100, Height: 12})
	if mt.m.listRows() != maxListRows || !strings.Contains(mt.view(), menuBack) {
		t.Errorf("rows %d, view:\n%s", mt.m.listRows(), mt.view())
	}
	if mt.hovered() != "Add a sub" {
		t.Errorf("cursor moved on resize: %q", mt.hovered())
	}
	// 大小没变（huh 的表单在 Init 时会再请求一次）时不重建列表，否则会没完没了地重画。
	list := mt.m.list
	mt.send(tea.WindowSizeMsg{Width: 100, Height: 12})
	mt.send(tea.WindowSizeMsg{Width: 90, Height: 14})
	if mt.m.list != list {
		t.Error("the list was rebuilt although the number of rows did not change")
	}

	// 第一级比一屏长（有需要处理的事项）时滚动，终端够高时一屏放下。
	s := *stoppedStatus
	s.Drift = &pipeline.Drift{State: pipeline.DriftPending}
	mt = newMenuTest(t, menuWith(&s, 2, 3))
	if !strings.Contains(mt.view(), menuQuit) {
		t.Errorf("first level does not fit:\n%s", mt.view())
	}
	mt.send(tea.WindowSizeMsg{Width: 100, Height: 12})
	if strings.Contains(mt.view(), menuQuit) {
		t.Errorf("first level does not scroll:\n%s", mt.view())
	}
}
