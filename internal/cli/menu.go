package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// 主菜单的分类（第二级），按用户要做的事划分，不按命令树。
const (
	groupConfig   = "Subs & config"
	groupTraffic  = "Traffic"
	groupDevices  = "LAN devices"
	groupCore     = "mihomo & panel"
	groupMaintain = "Maintenance"
)

const (
	menuQuit = "Quit"
	menuBack = "← Back"
)

// menuRefresh 是菜单空闲时刷新状态行的间隔。
const menuRefresh = 10 * time.Second

// menuChrome 是菜单中选项以外的行数：状态行、空行、列表标题、分类的说明、空行、按键说明、
// 光标所在项的说明和等价命令，再留一行。
const menuChrome = 9

// menuItem 是主菜单的一项，按以下顺序判断：quit 退出菜单，back 回到第一级，group 进入这一分类，
// 否则执行子命令 args。菜单只负责选择，需要提问的由子命令自己问。
type menuItem struct {
	label string
	// desc 是光标停在这一项时列表下方的一行说明：会发生什么、有什么前提。
	desc string
	// note 是分类右侧的当前状态，也是进入分类后标题下的说明。
	note  string
	group string
	args  []string
	back  bool
	quit  bool
}

// key 标识菜单项，用来在回到菜单时把光标放回上次选的那一项。
func (it menuItem) key() string {
	if it.group != "" {
		return "group:" + it.group
	}
	return it.label
}

// menuData 是画一次菜单所需的状态。
type menuData struct {
	status    *setup.Status
	subs      []subscription.Entry
	revisions int
	// backups 是自动备份的份数。
	backups int
}

// runMenu 是在终端里只输入 tunlr 时的主菜单。整个菜单期间只运行一个内联的 Bubble Tea 程序（不进入
// 备用屏幕）：顶部是一行状态，下面是两级菜单。选中操作后把终端交给对应的子命令，输出留在滚动记录里，
// 结束后输出一行结果，菜单接着出现在下面。任何提问按 Esc 回到上一级，在第一级按 Esc 退出。
// 操作执行中按 Ctrl-C 只中断这个操作。
func (a *app) runMenu(cmd *cobra.Command) error {
	// 菜单在不随 Ctrl-C 取消的 context 中运行；cmd.Context() 会随 Ctrl-C 取消。
	ctx := a.session
	if ctx == nil {
		ctx = cmd.Context()
	}
	d, err := a.loadMenu(ctx)
	if err != nil {
		return err
	}
	p := tea.NewProgram(a.newMenu(ctx, d), tea.WithContext(ctx), tea.WithInput(a.stdin), tea.WithOutput(a.stderr))
	if _, err := p.Run(); err != nil && !errors.Is(err, tea.ErrInterrupted) && ctx.Err() == nil {
		return err
	}
	return nil
}

func (a *app) loadMenu(ctx context.Context) (menuData, error) {
	s, err := a.installer().Status(ctx)
	if err != nil {
		return menuData{}, err
	}
	subs, err := a.subscriptions().List()
	if err != nil {
		return menuData{}, err
	}
	revs, _, err := a.pipeline().Revisions()
	if err != nil {
		return menuData{}, err
	}
	saved, err := a.installer().SavedBackups()
	if err != nil {
		return menuData{}, err
	}
	return menuData{status: s, subs: subs, revisions: len(revs), backups: len(saved)}, nil
}

// menuModel 是主菜单的 Bubble Tea 模型。列表是嵌在里面的 huh 表单；执行操作时经 tea.Exec 交出
// 终端（见 menuRun）。
type menuModel struct {
	a    *app
	ctx  context.Context
	opts globalOptions
	// load 读取菜单需要的状态；refresh 为 0 时不定时刷新（供测试替换）。
	load    func(context.Context) (menuData, error)
	refresh time.Duration

	data menuData
	// stale 是最近一次刷新状态失败的原因，状态行据此提示。
	stale error
	size  tea.WindowSizeMsg

	group string
	// last 记录每一级上次选的项。
	last   map[string]string
	title  string
	note   string
	items  []menuItem
	labels []string
	list   *huh.Select[int]
	choice int
	form   *huh.Form
	init   tea.Cmd

	loading bool
	// tick 是定时刷新的代数，过时的 menuTickMsg 作废。
	tick int
	// pending 是刷新状态期间选中的操作，刷新完再执行，避免与读取状态的协程同时访问 app。
	pending  *menuRun
	running  *menuRun
	quitting bool
}

type (
	menuTickMsg int
	menuDataMsg struct {
		data menuData
		err  error
	}
	menuDoneMsg struct{}
)

func (a *app) newMenu(ctx context.Context, d menuData) *menuModel {
	m := &menuModel{a: a, ctx: ctx, opts: a.opts, load: a.loadMenu, refresh: menuRefresh, data: d,
		last: map[string]string{}}
	m.init = m.showList("")
	return m
}

func (m *menuModel) Init() tea.Cmd {
	return tea.Batch(m.init, m.nextTick())
}

func (m *menuModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		rows := m.listRows()
		m.size = msg
		// 列表的高度随终端高度变化。只在行数变了时重建：huh 的表单在 Init 时总会再请求一次终端大小，
		// 每次都重建会没完没了地重画。
		if m.listRows() != rows {
			return m, m.showList(m.hoveredKey())
		}
	case menuTickMsg:
		if int(msg) != m.tick || m.loading || m.running != nil {
			return m, nil
		}
		return m, m.reload()
	case menuDataMsg:
		return m, m.loaded(msg)
	case menuDoneMsg:
		m.running = nil
		return m, m.reload()
	}
	if m.running != nil || m.pending != nil || m.quitting {
		return m, nil
	}
	f, cmd := m.form.Update(msg)
	m.form = f.(*huh.Form)
	switch m.form.State {
	case huh.StateAborted:
		return m, m.back()
	case huh.StateCompleted:
		return m, m.choose()
	}
	return m, cmd
}

func (m *menuModel) View() tea.View {
	if m.quitting || m.running != nil || m.pending != nil {
		return tea.NewView("")
	}
	return tea.NewView(m.statusLine() + "\n\n" + m.form.View() + "\n" + m.hint())
}

// hint 是列表下方的两行：光标所在那一项的说明和等价命令。没有时留空行，列表不跳动。
func (m *menuModel) hint() string {
	var desc, line string
	if i, ok := m.list.Hovered(); ok && i < len(m.items) {
		it := m.items[i]
		desc = it.desc
		if it.args != nil {
			line = commandLine(it.args)
		}
	}
	if m.size.Width > 1 {
		desc = ansi.Truncate(desc, m.size.Width-1, "…")
		line = ansi.Truncate(line, m.size.Width-1, "…")
	}
	return m.a.style.dim(desc) + "\n" + m.a.style.dim(line)
}

// hoveredKey 是列表中光标所在那一项的 key，没有时为空。
func (m *menuModel) hoveredKey() string {
	if m.list == nil {
		return ""
	}
	if i, ok := m.list.Hovered(); ok && i < len(m.items) {
		return m.items[i].key()
	}
	return ""
}

// listRows 是列表一次显示的选项行数：按终端高度尽量让一级一屏放下，至少 maxListRows 行，更多时滚动。
func (m *menuModel) listRows() int {
	return max(maxListRows, m.size.Height-menuChrome)
}

// showList 显示当前分类的列表，光标停在 hover（为空时为上次在这一级选的那一项）。
// 状态变了、这一分类已不存在（如未安装）时回到第一级。
func (m *menuModel) showList(hover string) tea.Cmd {
	title, note, items := m.a.menuItems(m.group, m.data)
	if items == nil {
		m.group = ""
		title, note, items = m.a.menuItems("", m.data)
	}
	if hover == "" {
		hover = m.last[m.group]
	}
	m.title, m.note, m.items, m.labels = title, note, items, m.a.menuLabels(items)
	m.choice = max(0, slices.IndexFunc(items, func(it menuItem) bool { return it.key() == hover }))
	// 高度包括标题，有说明时再加一行。
	height := min(len(m.labels), m.listRows()) + 1
	if note != "" {
		height++
	}
	m.list = huh.NewSelect[int]().Title(title).Description(note).Options(intOptions(m.labels)...).Value(&m.choice).
		Height(height)
	return m.setForm(promptKeys("q"), m.list)
}

func (m *menuModel) setForm(km *huh.KeyMap, fields ...huh.Field) tea.Cmd {
	m.form = huh.NewForm(huh.NewGroup(fields...)).WithKeyMap(km).WithTheme(huh.ThemeFunc(huh.ThemeBase16))
	cmd := m.form.Init()
	if m.size.Width > 0 {
		f, c := m.form.Update(m.size)
		m.form = f.(*huh.Form)
		cmd = tea.Batch(cmd, c)
	}
	return cmd
}

// back 处理取消：分类回到第一级，第一级退出菜单。
func (m *menuModel) back() tea.Cmd {
	if m.group != "" {
		m.group = ""
		return m.showList("")
	}
	m.quitting = true
	return tea.Quit
}

// choose 处理列表的提交。
func (m *menuModel) choose() tea.Cmd {
	it := m.items[m.choice]
	m.last[m.group] = it.key()
	switch {
	case it.quit:
		m.quitting = true
		return tea.Quit
	case it.back:
		m.group = ""
		return m.showList("")
	case it.group != "":
		m.group = it.group
		return m.showList("")
	}
	return tea.Batch(m.showList(""), m.start(it))
}

// start 执行一个操作；正在刷新状态时等刷新完再执行。
func (m *menuModel) start(it menuItem) tea.Cmd {
	args := it.args
	// 子命令重新解析全局参数，执行完恢复；--verbose 带下去。
	if m.opts.Verbose {
		args = append(slices.Clone(args), "--verbose")
	}
	r := &menuRun{a: m.a, ctx: m.ctx, opts: m.opts, label: it.label, args: args}
	if m.loading {
		m.pending = r
		return nil
	}
	return m.exec(r)
}

func (m *menuModel) exec(r *menuRun) tea.Cmd {
	m.running = r
	return tea.Exec(r, func(error) tea.Msg { return menuDoneMsg{} })
}

func (m *menuModel) reload() tea.Cmd {
	m.loading = true
	load, ctx := m.load, m.ctx
	return func() tea.Msg {
		d, err := load(ctx)
		return menuDataMsg{d, err}
	}
}

// loaded 用刷新到的状态更新状态行和列表（光标留在原来那一项），再执行等待中的操作或安排下次刷新。
func (m *menuModel) loaded(msg menuDataMsg) tea.Cmd {
	m.loading = false
	var cmds []tea.Cmd
	if m.stale = msg.err; msg.err == nil {
		m.data = msg.data
		cmds = append(cmds, m.refreshList())
	}
	if r := m.pending; r != nil {
		m.pending = nil
		return tea.Batch(append(cmds, m.exec(r))...)
	}
	return tea.Batch(append(cmds, m.nextTick())...)
}

// refreshList 在列表内容随状态变化时重建列表，光标留在原来那一项。
func (m *menuModel) refreshList() tea.Cmd {
	title, note, items := m.a.menuItems(m.group, m.data)
	if title == m.title && note == m.note && slices.Equal(m.a.menuLabels(items), m.labels) {
		return nil
	}
	return m.showList(m.hoveredKey())
}

func (m *menuModel) nextTick() tea.Cmd {
	if m.refresh == 0 {
		return nil
	}
	m.tick++
	t := m.tick
	return tea.Tick(m.refresh, func(time.Time) tea.Msg { return menuTickMsg(t) })
}

// statusLine 是菜单顶部的一行状态，超出终端宽度的部分截掉。
func (m *menuModel) statusLine() string {
	a, s := m.a, m.data.status
	parts := []string{a.style.title("tunlr")}
	if !s.Installed {
		parts = append(parts, a.style.warn("not installed"))
	} else {
		sub := s.Subscription
		if sub == "" {
			sub = a.style.warn("no subscription")
		}
		parts = append(parts, "mihomo "+a.describeService(s), sub, a.describeRevision(s))
		if s.Tun != nil {
			parts = append(parts, "TUN "+a.describeTun(s.Tun))
		}
	}
	if m.stale != nil {
		parts = append(parts, a.style.warn("not refreshed: "+m.stale.Error()))
	}
	line := strings.Join(parts, a.style.dim(" · "))
	if m.size.Width > 1 {
		line = ansi.Truncate(line, m.size.Width-1, "…")
	}
	return line
}

// menuRun 是菜单中执行的一个操作。它实现 tea.ExecCommand：经 tea.Exec 执行时 Bubble Tea 先交出
// 终端（等读取输入的协程退出），子命令照常输出和提问，结束后再收回终端。
type menuRun struct {
	a     *app
	ctx   context.Context
	opts  globalOptions
	label string
	args  []string
}

func (r *menuRun) SetStdin(io.Reader)  {}
func (r *menuRun) SetStdout(io.Writer) {}
func (r *menuRun) SetStderr(io.Writer) {}

// Run 先输出操作名和等价命令，再执行子命令，最后输出一行结果。
func (r *menuRun) Run() error {
	a := r.a
	fmt.Fprintf(a.stdout, "%s  %s\n\n", a.style.title("▶ "+r.label), a.style.dim(commandLine(r.args)+"  ·  Ctrl-C to interrupt"))
	start := time.Now()
	code, interrupted := a.run(r.ctx, r.args)
	a.opts = r.opts
	fmt.Fprintf(a.stdout, "\n%s\n\n", a.result(code, interrupted, time.Since(start)))
	return nil
}

// result 是操作结束后的一行结果。用户取消或在确认时回答否时什么都没做，不算失败。
func (a *app) result(code int, interrupted bool, took time.Duration) string {
	elapsed := " " + a.style.dim("("+formatElapsed(took)+")")
	switch {
	case interrupted:
		return a.style.warn("– Interrupted") + elapsed
	case code == exitOK && a.aborted:
		return a.style.dim("– Cancelled")
	case code == exitOK:
		return a.style.good("✓ Done") + elapsed
	case code == exitRolledBack:
		return a.style.bad("✗ Failed; the change was rolled back") + elapsed
	case code == exitCheckFailed:
		return a.style.bad("✗ Check failed") + elapsed
	}
	return a.style.bad("✗ Failed") + elapsed
}

// op 是执行子命令的菜单项。
func op(label, desc string, args ...string) menuItem {
	return menuItem{label: label, desc: desc, args: args}
}

// toggle 是开关类的菜单项：只给出与当前状态相反的那个动作。
func toggle(on bool, offItem, onItem menuItem) menuItem {
	if on {
		return offItem
	}
	return onItem
}

// menuItems 返回分类 group（"" 为第一级）的标题、标题下的说明和菜单项。没安装时第一级只能安装、
// 查看状态或诊断，分类不存在。菜单项只在有意义时出现；开关类的只出现与当前状态相反的动作。
// 第一级最上面是需要处理的事项（见 attention）。
func (a *app) menuItems(group string, d menuData) (title, note string, items []menuItem) {
	s := d.status
	status := op("Show status", "The service, kernel, sub, config, TUN and updates at a glance", "status")
	diagnose := op("Diagnose", "Check this server and the installation, with hints for what fails", "doctor")
	if !s.Installed {
		if group != "" {
			return "", "", nil
		}
		return "tunlr · not installed yet", "", []menuItem{
			op("Install tunlr", "Download mihomo, create its service and the scheduled updates; asks a few questions first", "init"),
			status, diagnose, {label: menuQuit, quit: true}}
	}
	groups := []menuItem{
		{label: groupConfig, group: groupConfig, note: a.configNote(s, d.subs),
			desc: "Add, switch and change subs; geo data; mixins; publish or roll back the config"},
		{label: groupTraffic, group: groupTraffic, note: a.trafficNote(s),
			desc: "What goes through mihomo: TUN, gateway mode, the proxy port and filters"},
		{label: groupDevices, group: groupDevices, note: a.describeDevicesNote(s.Devices),
			desc: "Devices on the LAN that use this server: names and how each one goes"},
		{label: groupCore, group: groupCore, note: a.coreNote(s),
			desc: "Start, stop, logs and upgrades; how to open the panel"},
		{label: groupMaintain, group: groupMaintain, note: a.describeBackups(d.backups),
			desc: "Backups, tool settings, diagnostics and repair"},
	}
	if group == "" {
		items = append(a.attention(d), status)
		items = append(items, groups...)
		return "tunlr", "", append(items, menuItem{label: menuQuit, quit: true})
	}
	i := slices.IndexFunc(groups, func(it menuItem) bool { return it.group == group })
	if i < 0 {
		return "", "", nil
	}
	title, note = "tunlr › "+group, groups[i].note
	switch group {
	case groupConfig:
		items = a.configItems(d)
	case groupTraffic:
		items = a.trafficItems(s)
	case groupDevices:
		items = []menuItem{
			op("List devices", "Devices that use this server, their names and how they go", "devices"),
			op("List all LAN devices", "Including those that do not use this server", "devices", "--all"),
			op("Name a device", "Choose a device, then enter a name; an empty name removes it", "devices", "name"),
			op("Route a device", "Through the proxies (it follows the rules) or direct for everything", "filter", "device"),
			op("Forget a device", "Remove its name and setting; it follows the rules again", "devices", "forget"),
		}
	case groupCore:
		items = a.coreItems(s)
	case groupMaintain:
		items = []menuItem{
			op("Back up to a file", "Settings, mixins and subs, to restore here or on another server", "backup", "create"),
			op("Restore a backup", "An automatic backup or a file; the diff is shown before anything changes", "backup", "restore"),
			op("Edit settings", "tunlr's own settings, such as the download mirror; checked, then applied", "settings", "edit"),
			diagnose,
			op("Repair install", "Set up the service, kernel and timers again; your config is kept", "init"),
		}
	}
	return title, note, append(items, menuItem{label: menuBack, back: true})
}

func (a *app) configItems(d menuData) []menuItem {
	var items []menuItem
	items = append(items, op("Add a sub", "Name, URL or file, fetch period and an optional proxy filter; the first one becomes active", "sub", "add"))
	if len(d.subs) > 1 {
		items = append(items, op("Switch sub", "Make another saved sub active and publish the config", "sub", "use"))
	}
	if len(d.subs) > 0 {
		items = append(items,
			op("Change a sub", "Its URL or file, User-Agent, fetch period or proxy filter", "sub", "set"),
			op("Fetch subs now", "Fetch every sub again; the config is published if the active one changed", "sub", "update"))
	}
	if len(d.subs) > 1 {
		items = append(items, op("Remove a sub", "Any but the active one", "sub", "remove"))
	}
	if len(d.subs) > 0 {
		items = append(items, op("Geo data settings", "Per sub: how often mihomo updates the geo data and where from; the rest comes from tunlr's fallbacks", "update", "set", "geo"))
	}
	items = append(items, op("Update geo data now", "Have mihomo download the geo data from geox-url now (it does so by itself on its period)", "update", "run", "geo"))
	items = append(items,
		op("Edit a mixin", "Your own changes on top of the sub; checked with mihomo -t, then published", "config", "edit"),
		op("Publish config", "Compose and check the config, show the diff, publish after you confirm", "config", "apply"))
	if d.revisions > 0 {
		items = append(items, op("Roll back config", "Publish an earlier revision again", "config", "rollback"))
	}
	return append(items,
		op("List subs", "Proxies, last fetch, period, usage and expiry of each sub", "sub"),
		op("Config details", "The published config, its history and the mixins", "config"))
}

func (a *app) trafficItems(s *setup.Status) []menuItem {
	tun := s.Tun != nil && s.Tun.Switch != "off"
	f := s.Filter
	if f == nil {
		f = &setup.FilterStatus{}
	}
	items := []menuItem{
		toggle(tun,
			op("Turn TUN off", "This server's own traffic no longer goes through mihomo", "tun", "off"),
			op("Turn TUN on", "This server's own traffic goes through mihomo; rolled back if the network breaks", "tun", "on")),
		toggle(s.Tun != nil && s.Tun.Gateway != "",
			op("Turn gateway off", "LAN devices that use this server as their gateway lose the proxy; TUN stays on", "tun", "gateway", "off"),
			op("Turn gateway on", "LAN devices that use this server as their gateway go through mihomo (turns TUN on)", "tun", "gateway", "on")),
		toggle(s.ProxyLAN,
			op("Close proxy port to LAN", "Only this server can use the HTTP/SOCKS5 proxy port again", "proxy", "lan", "off"),
			op("Open proxy port to LAN", "For devices that set this server as their HTTP/SOCKS5 proxy", "proxy", "lan", "on")),
		toggle(f.QUIC,
			op("Allow QUIC", "Browsers may use QUIC (HTTP/3) again", "filter", "quic", "off"),
			op("Block QUIC", "Browsers fall back to TCP, which proxies better", "filter", "quic", "on")),
	}
	if f.Ports {
		items = append(items,
			op("Change proxied ports", "Now: "+strings.Join(f.PortList, ","), "filter", "ports", "set"),
			op("Proxy all ports", "Every port goes through mihomo again", "filter", "ports", "off"))
	} else {
		items = append(items, op("Proxy some ports only", "Asks which ports; others go direct", "filter", "ports", "on"))
	}
	items = append(items, op("Send a network direct", "All traffic from an address range skips mihomo", "filter", "source", "add"))
	if len(f.Sources) > 0 {
		items = append(items, op("Stop sending a network direct", "Traffic from that range follows the rules again", "filter", "source", "remove"))
	}
	return append(items,
		op("TUN details", "TUN, gateway mode and the checks behind them", "tun"),
		op("Proxy port details", "The address other devices enter as their proxy", "proxy"),
		op("Filter details", "Devices and networks sent direct, QUIC and ports", "filter"))
}

func (a *app) coreItems(s *setup.Status) []menuItem {
	var items []menuItem
	if s.Service != nil && s.Service.Active() {
		items = append(items,
			op("Restart mihomo", "Then check that it is healthy", "core", "restart"),
			op("Stop mihomo", "Proxying stops until it is started again", "core", "stop"))
	} else {
		items = append(items, op("Start mihomo", "Then check that it is healthy", "core", "start"))
	}
	items = append(items,
		op("Recent logs", "The last 50 lines; follow them with tunlr core logs -f", "core", "logs", "-n", "50"),
		op("Upgrade mihomo", "To the latest release; checked against the config first, put back if unhealthy", "core", "upgrade"))
	if canRollBackKernel(s.Kernel) {
		items = append(items, op("Roll back mihomo", "To the previous known-good version kept on disk", "core", "rollback"))
	}
	return append(items,
		op("Open the panel", "How to reach it: the SSH tunnel and the link", "panel"),
		toggle(s.PanelLAN,
			op("Close panel to LAN", "Only this server can reach the panel again; mihomo restarts", "panel", "lan", "off"),
			op("Open panel to LAN", "Devices on the LAN can open the panel; mihomo restarts", "panel", "lan", "on")),
		op("New API secret", "Panels that are open must connect again with the new link", "panel", "reset-secret"),
		op("mihomo details", "Kernel versions kept on disk and the service", "core"))
}

// attention 是第一级最上面需要处理的事项，每项对应一个命令：有还没发布的改动、内核服务失败或没有运行
// （发布过配置时）、订阅最近一次抓取失败。
func (a *app) attention(d menuData) []menuItem {
	s := d.status
	mark := a.style.warn("!") + " "
	var items []menuItem
	if s.Drift != nil && s.Drift.State == pipeline.DriftPending {
		items = append(items, op(mark+"Publish the changes not yet published", "Shows the diff first", "config", "apply"))
	}
	if s.Service != nil && !s.Service.Active() && d.revisions > 0 {
		why := "not running"
		if s.Service.ActiveState == "failed" {
			why = "failed"
		}
		items = append(items, op(mark+"Start mihomo ("+why+")", "Then check that it is healthy", "core", "start"))
	}
	// geo 数据由内核更新，失败只记在内核的日志中；这里只看订阅。
	if slices.ContainsFunc(s.Updates, func(sc setup.Schedule) bool {
		return sc.Task == setup.TaskSubscription && sc.Last != nil && sc.Last.Status == setup.StepFailed
	}) {
		items = append(items, op(mark+"Fetch subs again (the last fetch failed)", "Fetch every sub; the last good copy is kept if it fails again", "update", "run", "sub"))
	}
	return items
}

// canRollBackKernel 报告 tunlr core rollback 有没有可以换回的已知可用版本（与当前二进制不同的那一份）。
func canRollBackKernel(k *mihomo.KernelStatus) bool {
	if k == nil {
		return false
	}
	if k.KnownGood {
		return len(k.Kernels) > 1
	}
	return len(k.Kernels) > 0
}

// menuLabels 是菜单项显示的文字：分类右侧对齐显示当前状态（暗色）。
func (a *app) menuLabels(items []menuItem) []string {
	width := 0
	for _, it := range items {
		if it.note != "" {
			width = max(width, len(it.label))
		}
	}
	labels := make([]string, len(items))
	for i, it := range items {
		labels[i] = it.label
		if it.note != "" {
			labels[i] += strings.Repeat(" ", width-len(it.label)+3) + it.note
		}
	}
	return labels
}

// configNote 是“Subs & config”的状态：启用的订阅和发布的修订。
func (a *app) configNote(s *setup.Status, subs []subscription.Entry) string {
	return a.describeSubscriptions(s, subs) + a.style.dim(" · ") + a.describeRevision(s)
}

// trafficNote 是“Traffic”的状态：TUN 与网关、代理端口、过滤。
func (a *app) trafficNote(s *setup.Status) string {
	parts := []string{}
	if s.Tun != nil {
		parts = append(parts, "TUN "+a.describeTun(s.Tun))
	}
	parts = append(parts, "proxy port "+a.describeProxyLAN(s.ProxyLAN), "filters "+a.describeFilterNote(s.Filter))
	return strings.Join(parts, a.style.dim(" · "))
}

// coreNote 是“mihomo & panel”的状态：内核服务和面板的局域网访问。
func (a *app) coreNote(s *setup.Status) string {
	return a.describeService(s) + a.style.dim(" · ") + "panel " + a.describePanelLAN(s.PanelLAN)
}

func (a *app) describeSubscriptions(s *setup.Status, subs []subscription.Entry) string {
	switch {
	case len(subs) == 0:
		return a.style.warn("no subs")
	case s.Subscription == "":
		return a.style.warn("none active") + a.style.dim(fmt.Sprintf(" · %d saved", len(subs)))
	}
	return s.Subscription + a.style.dim(fmt.Sprintf(" · %d saved", len(subs)))
}

func (a *app) describeRevision(s *setup.Status) string {
	var note string
	if d := s.Drift; d != nil {
		switch d.State {
		case pipeline.DriftPending:
			note = " " + a.style.warn("(unpublished changes)")
		case pipeline.DriftError:
			note = " " + a.style.bad("(cannot compose)")
		case pipeline.DriftEdited:
			return a.style.warn("edited outside tunlr")
		}
	}
	if s.Revision == nil {
		return a.style.warn("not published")
	}
	return fmt.Sprintf("revision %d", s.Revision.Number) + note
}

// describeDevicesNote 是局域网设备分类右侧的状态；网关和代理端口的局域网访问都关闭时没有设备会使用本机。
func (a *app) describeDevicesNote(d *setup.DeviceSummary) string {
	if d == nil {
		return a.style.dim("none: gateway and proxy port LAN access are off")
	}
	return a.describeDevices(d)
}

// describeFilterNote 是过滤的状态。
func (a *app) describeFilterNote(f *setup.FilterStatus) string {
	if f == nil {
		return a.style.dim("none")
	}
	return a.describeFilter(f)
}

func (a *app) describeService(s *setup.Status) string {
	if s.Running != "" {
		return a.style.good("running") + a.style.dim(" · "+s.Running)
	}
	if s.Service != nil && s.Service.ActiveState == "failed" {
		return a.style.bad("failed")
	}
	return a.style.warn("not running")
}

// commandLine 是给用户看的等价命令。含空白或 shell 特殊字符的参数加上单引号。
func commandLine(args []string) string {
	shown := slices.Clone(args)
	for i, s := range shown {
		if strings.ContainsAny(s, " \t'\"`$|&;<>()*?!#~\\") {
			shown[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return "$ tunlr " + strings.Join(shown, " ")
}
