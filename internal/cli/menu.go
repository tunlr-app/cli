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

	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// 主菜单的分类（第二级）。
const (
	groupSubscriptions = "Subscriptions"
	groupConfiguration = "Configuration"
	groupTun           = "Transparent proxy"
	groupService       = "mihomo service"
	groupUpdates       = "Scheduled updates"
)

const (
	menuQuit = "Quit"
	menuBack = "← Back"
)

// menuRefresh 是菜单空闲时刷新状态行的间隔。
const menuRefresh = 10 * time.Second

// menuItem 是主菜单的一项，按以下顺序判断：quit 退出菜单，back 回到第一级，group 进入这一分类，
// ask 不为空时先在表单中询问这些项、由 build 得到参数，否则执行子命令 args。
type menuItem struct {
	label string
	// note 是分类右侧的当前状态。
	note  string
	group string
	args  []string
	ask   []inputField
	build func(values []string) []string
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
	return menuData{s, subs, len(revs)}, nil
}

// menuModel 是主菜单的 Bubble Tea 模型。列表和添加订阅的表单都是嵌在里面的 huh 表单；
// 执行操作时经 tea.Exec 交出终端（见 menuRun）。
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
	items  []menuItem
	labels []string
	list   *huh.Select[int]
	choice int
	// adding 是正在填写表单的菜单项，values 是表单的值。
	adding *menuItem
	values []string
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
		m.size = msg
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
	return tea.NewView(m.statusLine() + "\n\n" + m.form.View())
}

// showList 显示当前分类的列表，光标停在 hover（为空时为上次在这一级选的那一项）。
// 状态变了、这一分类已不存在（如未安装）时回到第一级。
func (m *menuModel) showList(hover string) tea.Cmd {
	title, items := m.a.menuItems(m.group, m.data)
	if items == nil {
		m.group = ""
		title, items = m.a.menuItems("", m.data)
	}
	if hover == "" {
		hover = m.last[m.group]
	}
	m.title, m.items, m.labels = title, items, m.a.menuLabels(items)
	m.choice = max(0, slices.IndexFunc(items, func(it menuItem) bool { return it.key() == hover }))
	m.list = huh.NewSelect[int]().Title(title).Options(intOptions(m.labels)...).Value(&m.choice).
		Height(listHeight(m.labels, 1))
	m.adding = nil
	return m.setForm(promptKeys("q"), m.list)
}

// showAsk 显示菜单项 it 的表单。
func (m *menuModel) showAsk(it menuItem) tea.Cmd {
	m.values = make([]string, len(it.ask))
	fields := make([]huh.Field, len(it.ask))
	for i, f := range it.ask {
		if f.options != nil {
			m.values[i] = f.initial
			fields[i] = huh.NewSelect[string]().Title(f.title).Description(f.description).Options(f.options...).
				Value(&m.values[i]).Height(listHeight(make([]string, len(f.options)), 2))
			continue
		}
		fields[i] = huh.NewInput().Title(f.title).Description(f.description).Validate(f.validate).Value(&m.values[i])
	}
	m.adding = &it
	return m.setForm(promptKeys(), fields...)
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

// back 处理取消：表单回到列表，分类回到第一级，第一级退出菜单。
func (m *menuModel) back() tea.Cmd {
	switch {
	case m.adding != nil:
		return m.showList("")
	case m.group != "":
		m.group = ""
		return m.showList("")
	}
	m.quitting = true
	return tea.Quit
}

// choose 处理列表或表单的提交。
func (m *menuModel) choose() tea.Cmd {
	if it := m.adding; it != nil {
		args := it.build(m.values)
		return tea.Batch(m.showList(""), m.start(*it, args))
	}
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
	case it.ask != nil:
		return m.showAsk(it)
	}
	return tea.Batch(m.showList(""), m.start(it, it.args))
}

// start 执行一个操作；正在刷新状态时等刷新完再执行。
func (m *menuModel) start(it menuItem, args []string) tea.Cmd {
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
	if m.adding != nil {
		return nil
	}
	title, items := m.a.menuItems(m.group, m.data)
	if title == m.title && slices.Equal(m.a.menuLabels(items), m.labels) {
		return nil
	}
	hover := ""
	if i, ok := m.list.Hovered(); ok && i < len(m.items) {
		hover = m.items[i].key()
	}
	return m.showList(hover)
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

// menuItems 返回分类 group（"" 为第一级）的标题和菜单项。没安装时第一级只能安装或查看状态，
// 分类不存在；订阅、历史、透明代理和内核的操作只在有意义时出现。
func (a *app) menuItems(group string, d menuData) (string, []menuItem) {
	s := d.status
	status := menuItem{label: "Show status", args: []string{"status"}}
	diagnose := menuItem{label: "Run diagnostics", args: []string{"doctor"}}
	if !s.Installed {
		if group != "" {
			return "", nil
		}
		return "tunlr · not installed yet", []menuItem{
			{label: "Install tunlr (init)", args: []string{"init"}}, status, diagnose, {label: menuQuit, quit: true}}
	}
	back := menuItem{label: menuBack, back: true}
	var items []menuItem
	switch group {
	case "":
		return "tunlr", []menuItem{
			status,
			{label: "Open the panel", args: []string{"panel"}},
			{label: groupSubscriptions, group: groupSubscriptions, note: a.describeSubscriptions(s, d.subs)},
			{label: groupConfiguration, group: groupConfiguration, note: a.describeRevision(s)},
			{label: groupTun, group: groupTun, note: a.describeTun(s.Tun)},
			{label: groupService, group: groupService, note: a.describeService(s)},
			{label: groupUpdates, group: groupUpdates, note: a.describeUpdates(s.Updates)},
			diagnose,
			{label: menuQuit, quit: true},
		}
	case groupSubscriptions:
		items = append(items, menuItem{label: "List subscriptions", args: []string{"sub", "list"}})
		if len(d.subs) > 0 {
			items = append(items,
				menuItem{label: "Switch subscription", args: []string{"sub", "use"}},
				menuItem{label: "Update subscriptions", args: []string{"sub", "update"}})
		}
		items = append(items, menuItem{label: "Add a subscription", ask: subscriptionFields(),
			build: func(v []string) []string { return []string{"sub", "add", v[0], v[1], "--update", v[2]} }})
		if len(d.subs) > 0 {
			items = append(items, menuItem{label: "Change how often one is fetched", args: []string{"sub", "set"}})
		}
		if len(d.subs) > 1 {
			items = append(items, menuItem{label: "Remove subscriptions", args: []string{"sub", "remove"}})
		}
	case groupConfiguration:
		items = append(items,
			menuItem{label: "Preview changes", args: []string{"plan"}},
			menuItem{label: "Preview and apply", args: []string{"apply"}},
			menuItem{label: "Edit a mixin", args: []string{"edit"}})
		if d.revisions > 0 {
			items = append(items,
				menuItem{label: "Roll back", args: []string{"rollback"}},
				menuItem{label: "List revisions", args: []string{"rollback", "--list"}})
		}
	case groupTun:
		if s.Tun != nil && s.Tun.Switch != "off" {
			items = append(items, menuItem{label: "Turn TUN off", args: []string{"tun", "off"}})
		} else {
			items = append(items, menuItem{label: "Turn TUN on", args: []string{"tun", "on"}})
		}
		if s.Tun != nil && s.Tun.Gateway != "" {
			items = append(items, menuItem{label: "Turn gateway mode off", args: []string{"gateway", "off"}})
		} else {
			items = append(items, menuItem{label: "Turn gateway mode on (LAN devices route through mihomo)", args: []string{"gateway", "on"}})
		}
		items = append(items,
			menuItem{label: "Show details", args: []string{"tun"}},
			menuItem{label: "Show gateway details", args: []string{"gateway"}})
	case groupService:
		if s.Service != nil && s.Service.Active() {
			items = append(items,
				menuItem{label: "Restart mihomo", args: []string{"restart"}},
				menuItem{label: "Stop mihomo", args: []string{"stop"}})
		} else {
			items = append(items, menuItem{label: "Start mihomo", args: []string{"start"}})
		}
		items = append(items,
			menuItem{label: "Show recent logs", args: []string{"logs", "-n", "50"}},
			menuItem{label: "Upgrade mihomo to the latest release", args: []string{"upgrade", "core"}})
	case groupUpdates:
		items = append(items,
			menuItem{label: "Show the schedule", args: []string{"update", "show"}},
			menuItem{label: "Change the geo data update", args: []string{"update", "set", "geo"}},
			menuItem{label: "Update everything now", args: []string{"update"}})
	default:
		return "", nil
	}
	return "tunlr › " + group, append(items, back)
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

func (a *app) describeSubscriptions(s *setup.Status, subs []subscription.Entry) string {
	switch {
	case len(subs) == 0:
		return a.style.warn("none")
	case s.Subscription == "":
		return a.style.warn("none active") + a.style.dim(fmt.Sprintf(" · %d saved", len(subs)))
	}
	return s.Subscription + a.style.dim(fmt.Sprintf(" · %d saved", len(subs)))
}

func (a *app) describeRevision(s *setup.Status) string {
	if s.Revision == nil {
		return a.style.warn("not published")
	}
	return fmt.Sprintf("revision %d", s.Revision.Number)
}

// describeUpdates 是定时更新分类右侧的状态，如 "2 subscriptions · geo weekly by tunlr"；最近一次
// 失败的标红。
func (a *app) describeUpdates(scheds []setup.Schedule) string {
	subs, failed := 0, 0
	geo := ""
	for _, s := range scheds {
		if s.Task == setup.TaskGeo {
			geo = "geo " + settings.DescribePeriod(s.Period)
			if s.Period != settings.PeriodOff {
				geo += " by " + s.Updater
			}
			if s.Last != nil && s.Last.Status == setup.StepFailed {
				geo = a.style.bad(geo + " (failed)")
			}
			continue
		}
		subs++
		if s.Last != nil && s.Last.Status == setup.StepFailed {
			failed++
		}
	}
	part := fmt.Sprintf("%d subscriptions", subs)
	if subs == 1 {
		part = "1 subscription"
	}
	if failed > 0 {
		part = a.style.bad(fmt.Sprintf("%s (%d failed)", part, failed))
	}
	return part + a.style.dim(" · ") + geo
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

// subscriptionFields 是添加订阅时在同一个表单中询问的名称、地址和自动更新周期。
func subscriptionFields() []inputField {
	periods := make([]huh.Option[string], len(periodChoices))
	for i, p := range periodChoices {
		periods[i] = huh.NewOption(describePeriod(p), p)
	}
	return []inputField{
		{title: "Name of the subscription", description: "e.g. home or work; letters, digits, '.', '_' and '-'", validate: subscription.ValidateName},
		{title: "Subscription URL", description: "The URL your provider gives for Clash/mihomo.", validate: subscription.ValidateURL},
		{title: "Fetch it again how often?", description: "Change later with tunlr sub set.", options: periods, initial: state.DefaultUpdate},
	}
}

// commandLine 是给用户看的等价命令，订阅地址只给出缩写，不显示访问令牌。
func commandLine(args []string) string {
	shown := slices.Clone(args)
	if len(shown) >= 4 && shown[0] == "sub" && shown[1] == "add" {
		shown[3] = subscription.Redact(shown[3])
	}
	return "$ tunlr " + strings.Join(shown, " ")
}
