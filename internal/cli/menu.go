package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// 主菜单的分类（第二级）。
const (
	groupSubscriptions = "Subscriptions"
	groupConfiguration = "Configuration"
	groupTun           = "Transparent proxy"
	groupService       = "mihomo service"
)

const (
	menuQuit = "Quit"
	menuBack = "← Back"
)

// menuItem 是主菜单的一项，按以下顺序判断：quit 退出菜单，back 回到第一级，group 进入这一分类，
// 否则执行子命令 args；prepare 不为空时先由它补全参数（如询问订阅的名称和地址）。
type menuItem struct {
	label string
	// note 是分类右侧的当前状态。
	note    string
	group   string
	args    []string
	prepare func(ctx context.Context) ([]string, error)
	back    bool
	quit    bool
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

// runMenu 是在终端里只输入 tunlr 时的主菜单：两级，第一级是“查看状态”和各分类，第二级是分类里的
// 操作。选中操作后清屏执行对应的子命令，结束后显示结果，按回车回到菜单。任何提问按 Esc 回到上一级，
// 在第一级按 Esc 退出。操作执行中按 Ctrl-C 只中断这个操作。
func (a *app) runMenu(cmd *cobra.Command) error {
	// 菜单在不随 Ctrl-C 取消的 context 中循环；cmd.Context() 会随 Ctrl-C 取消。
	ctx := a.session
	if ctx == nil {
		ctx = cmd.Context()
	}
	opts := a.opts
	group := ""
	last := map[string]string{}
	for ctx.Err() == nil {
		d, err := a.loadMenu(ctx)
		if err != nil {
			return err
		}
		title, items := a.menuItems(group, d)
		if items == nil { // 状态变了，这一分类已不存在（如未安装）
			group = ""
			continue
		}
		labels := a.menuLabels(items)
		initial := max(0, slices.IndexFunc(items, func(it menuItem) bool { return it.key() == last[group] }))
		a.clearScreen()
		i, err := a.prompt.Choose(ctx, title, labels, initial)
		if errors.Is(err, huh.ErrUserAborted) {
			if group == "" {
				return nil
			}
			group = ""
			continue
		}
		if err != nil {
			return err
		}
		it := items[i]
		last[group] = it.key()
		switch {
		case it.quit:
			return nil
		case it.back:
			group = ""
			continue
		case it.group != "":
			group = it.group
			continue
		}
		if err := a.runMenuItem(ctx, it, opts); err != nil {
			return err
		}
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

// runMenuItem 清屏执行一个操作：顶部是操作名和等价命令，结束后在底部显示结果，等用户按回车。
// 用户在操作开头的提问中取消（什么都没做）时直接返回。
func (a *app) runMenuItem(ctx context.Context, it menuItem, opts globalOptions) error {
	args := it.args
	if it.prepare != nil {
		var err error
		if args, err = it.prepare(ctx); errors.Is(err, huh.ErrUserAborted) {
			return nil
		} else if err != nil {
			return err
		}
	}
	// 子命令重新解析全局参数，执行完恢复；--verbose 带下去。
	if opts.Verbose {
		args = append(slices.Clone(args), "--verbose")
	}
	a.clearScreen()
	fmt.Fprintf(a.stdout, "%s  %s\n\n", a.style.title(it.label), a.style.dim(commandLine(args)+"  ·  Ctrl-C to interrupt"))
	code, interrupted := a.run(ctx, args)
	a.opts = opts
	if a.aborted && code == exitOK && !interrupted {
		return nil
	}
	fmt.Fprintln(a.stdout)
	return a.prompt.Pause(ctx, a.resultTitle(code, interrupted), "Back to menu")
}

// resultTitle 是操作结束后的结果。
func (a *app) resultTitle(code int, interrupted bool) string {
	switch {
	case interrupted:
		return a.style.warn("– Interrupted")
	case code == exitOK:
		return a.style.good("✓ Done")
	case code == exitRolledBack:
		return a.style.bad("✗ Failed; the change was rolled back")
	case code == exitCheckFailed:
		return a.style.bad("✗ Check failed")
	}
	return a.style.bad("✗ Failed")
}

// menuItems 返回分类 group（"" 为第一级）的标题和菜单项。没安装时第一级只能安装或查看状态，
// 分类不存在；订阅、历史、透明代理和内核的操作只在有意义时出现。
func (a *app) menuItems(group string, d menuData) (string, []menuItem) {
	s := d.status
	status := menuItem{label: "Show status", args: []string{"status"}}
	if !s.Installed {
		if group != "" {
			return "", nil
		}
		return "tunlr · not installed yet", []menuItem{
			{label: "Install tunlr (init)", args: []string{"init"}}, status, {label: menuQuit, quit: true}}
	}
	back := menuItem{label: menuBack, back: true}
	var items []menuItem
	switch group {
	case "":
		return "tunlr", []menuItem{
			status,
			{label: groupSubscriptions, group: groupSubscriptions, note: a.describeSubscriptions(s, d.subs)},
			{label: groupConfiguration, group: groupConfiguration, note: a.describeRevision(s)},
			{label: groupTun, group: groupTun, note: a.describeTun(s.Tun)},
			{label: groupService, group: groupService, note: a.describeService(s)},
			{label: menuQuit, quit: true},
		}
	case groupSubscriptions:
		items = append(items, menuItem{label: "List subscriptions", args: []string{"sub", "list"}})
		if len(d.subs) > 0 {
			items = append(items,
				menuItem{label: "Switch subscription", args: []string{"sub", "use"}},
				menuItem{label: "Update subscriptions", args: []string{"sub", "update"}})
		}
		items = append(items, menuItem{label: "Add a subscription", prepare: a.askSubscription})
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
		items = append(items, menuItem{label: "Show details", args: []string{"tun"}})
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

func (a *app) describeService(s *setup.Status) string {
	if s.Running != "" {
		return a.style.good("running") + a.style.dim(" · "+s.Running)
	}
	if s.Service != nil && s.Service.ActiveState == "failed" {
		return a.style.bad("failed")
	}
	return a.style.warn("not running")
}

// askSubscription 在同一个表单中询问要添加的订阅的名称和地址。
func (a *app) askSubscription(ctx context.Context) ([]string, error) {
	v, err := a.prompt.Inputs(ctx, []inputField{
		{"Name of the subscription", "e.g. home or work; letters, digits, '.', '_' and '-'", subscription.ValidateName},
		{"Subscription URL", "The URL your provider gives for Clash/mihomo.", subscription.ValidateURL},
	})
	if err != nil {
		return nil, err
	}
	return []string{"sub", "add", v[0], v[1]}, nil
}

// commandLine 是给用户看的等价命令，订阅地址只给出缩写，不显示访问令牌。
func commandLine(args []string) string {
	shown := slices.Clone(args)
	if len(shown) >= 4 && shown[0] == "sub" && shown[1] == "add" {
		shown[3] = subscription.Redact(shown[3])
	}
	return "$ tunlr " + strings.Join(shown, " ")
}
