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

// menuItem 是主菜单的一项：选中后执行子命令 args。prepare 不为空时先由它补全参数（如询问订阅的
// 名称和地址）。args 与 prepare 都为空表示退出。
type menuItem struct {
	label   string
	args    []string
	prepare func(ctx context.Context) ([]string, error)
}

const menuQuit = "Quit"

// runMenu 是在终端里只输入 tunlr 时的主菜单：显示一行摘要，用箭头选择操作，执行对应的子命令后
// 回到菜单。子命令失败不退出菜单。
func (a *app) runMenu(cmd *cobra.Command) error {
	ctx := cmd.Context()
	opts := a.opts
	last := ""
	for ctx.Err() == nil {
		s, err := a.installer().Status(ctx)
		if err != nil {
			return err
		}
		subs, err := a.subscriptions().List()
		if err != nil {
			return err
		}
		revs, _, err := a.pipeline().Revisions()
		if err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, a.menuSummary(s))
		items := a.menuItems(s, subs, len(revs))
		labels := make([]string, len(items))
		for i, it := range items {
			labels[i] = it.label
		}
		i, err := a.prompt.Choose(ctx, "What do you want to do?", labels, max(0, slices.Index(labels, last)))
		if errors.Is(err, huh.ErrUserAborted) {
			return nil
		}
		if err != nil {
			return err
		}
		it := items[i]
		if it.args == nil && it.prepare == nil {
			return nil
		}
		last = it.label
		args := it.args
		if it.prepare != nil {
			if args, err = it.prepare(ctx); errors.Is(err, huh.ErrUserAborted) {
				fmt.Fprintln(a.stdout)
				continue
			} else if err != nil {
				return err
			}
		}
		// 子命令重新解析全局参数，执行完恢复；--verbose 带下去。
		if opts.Verbose {
			args = append(slices.Clone(args), "--verbose")
		}
		fmt.Fprintf(a.stdout, "%s\n\n", a.style.dim(commandLine(args)))
		a.execute(ctx, args)
		a.opts = opts
		fmt.Fprintln(a.stdout)
	}
	return nil
}

// menuSummary 是菜单上方的一行摘要：内核、启用的订阅、透明代理。
func (a *app) menuSummary(s *setup.Status) string {
	sep := a.style.dim(" · ")
	if !s.Installed {
		return a.style.warn("tunlr is not installed yet") + sep + "choose Install to set it up"
	}
	kernel := a.style.warn("mihomo not running")
	if s.Running != "" {
		kernel = a.style.good("mihomo " + s.Running + " running")
	}
	sub := a.style.warn("no subscription")
	if s.Subscription != "" {
		sub = "subscription " + s.Subscription
	}
	return kernel + sep + sub + sep + "TUN " + a.describeTun(s.Tun)
}

// menuItems 按当前状态给出菜单项：没安装时只能安装或查看状态；订阅、历史、透明代理和内核的
// 操作只在有意义时出现。
func (a *app) menuItems(s *setup.Status, subs []subscription.Entry, revisions int) []menuItem {
	status := menuItem{label: "Show status", args: []string{"status"}}
	quit := menuItem{label: menuQuit}
	if !s.Installed {
		return []menuItem{{label: "Install tunlr (init)", args: []string{"init"}}, status, quit}
	}
	items := []menuItem{status}
	if len(subs) > 0 {
		items = append(items,
			menuItem{label: "Switch subscription", args: []string{"sub", "use"}},
			menuItem{label: "Update subscriptions", args: []string{"sub", "update"}})
	}
	items = append(items, menuItem{label: "Add a subscription", prepare: a.askSubscription})
	if len(subs) > 1 {
		items = append(items, menuItem{label: "Remove subscriptions", args: []string{"sub", "remove"}})
	}
	items = append(items,
		menuItem{label: "Preview and apply the configuration", args: []string{"apply"}},
		menuItem{label: "Edit a mixin", args: []string{"edit"}})
	if revisions > 0 {
		items = append(items, menuItem{label: "Roll back the configuration", args: []string{"rollback"}})
	}
	if s.Tun != nil && s.Tun.Switch != "off" {
		items = append(items, menuItem{label: "Turn the transparent proxy (TUN) off", args: []string{"tun", "off"}})
	} else {
		items = append(items, menuItem{label: "Turn the transparent proxy (TUN) on", args: []string{"tun", "on"}})
	}
	if s.Service != nil && s.Service.Active() {
		items = append(items,
			menuItem{label: "Restart mihomo", args: []string{"restart"}},
			menuItem{label: "Stop mihomo", args: []string{"stop"}})
	} else {
		items = append(items, menuItem{label: "Start mihomo", args: []string{"start"}})
	}
	return append(items,
		menuItem{label: "Show recent logs", args: []string{"logs", "-n", "30"}},
		menuItem{label: "Upgrade mihomo to the latest release", args: []string{"upgrade", "core"}},
		quit)
}

// askSubscription 询问要添加的订阅的名称和地址。
func (a *app) askSubscription(ctx context.Context) ([]string, error) {
	name, err := a.prompt.Input(ctx, "Name of the subscription", "e.g. home or work; letters, digits, '.', '_' and '-'",
		subscription.ValidateName)
	if err != nil {
		return nil, err
	}
	url, err := a.prompt.Input(ctx, "Subscription URL", "The URL your provider gives for Clash/mihomo.",
		subscription.ValidateURL)
	if err != nil {
		return nil, err
	}
	return []string{"sub", "add", name, url}, nil
}

// commandLine 是给用户看的等价命令，订阅地址只给出缩写，不显示访问令牌。
func commandLine(args []string) string {
	shown := slices.Clone(args)
	if len(shown) == 4 && shown[0] == "sub" && shown[1] == "add" {
		shown[3] = subscription.Redact(shown[3])
	}
	return "$ tunlr " + strings.Join(shown, " ")
}
