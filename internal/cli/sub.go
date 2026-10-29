package cli

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// subscriptionView 是订阅在 --json 输出中的形式。URL 只给出缩写，不输出访问令牌。
type subscriptionView struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
	// Path 是本地文件的路径，"-" 为从标准输入导入。
	Path        string                     `json:"path,omitempty"`
	Source      string                     `json:"source"`
	Active      bool                       `json:"active"`
	UserAgent   string                     `json:"user_agent,omitempty"`
	Update      string                     `json:"update"`
	AddedAt     time.Time                  `json:"added_at"`
	FetchedAt   time.Time                  `json:"fetched_at,omitzero"`
	LastError   string                     `json:"last_error,omitempty"`
	LastErrorAt time.Time                  `json:"last_error_at,omitzero"`
	UserInfo    *state.UserInfo            `json:"userinfo,omitempty"`
	Summary     config.SubscriptionSummary `json:"summary"`
	// Mixins 是订阅自己的 mixin 文件（mixin.d/<名称>/），按应用顺序。
	Mixins []string `json:"mixins"`
}

type fetchedView struct {
	Name      string                     `json:"name"`
	Activated bool                       `json:"activated"`
	Summary   config.SubscriptionSummary `json:"summary"`
}

type updateView struct {
	Name    string                     `json:"name"`
	OK      bool                       `json:"ok"`
	Error   string                     `json:"error,omitempty"`
	Summary config.SubscriptionSummary `json:"summary"`
}

func (a *app) subscriptions() *subscription.Manager {
	return &subscription.Manager{Layout: a.layout, Version: a.info.Version}
}

func (a *app) newSubCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sub",
		Short: "List subscriptions (several can be saved, one is active); add, remove, update, use",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.listSubscriptions() },
	}
	cmd.AddCommand(
		a.newSubAddCommand(),
		a.newSubRemoveCommand(),
		a.newSubUpdateCommand(),
		a.newSubUseCommand(),
	)
	return cmd
}

// updateFlagHelp 是 sub add 的 --update 参数的说明。
const updateFlagHelp = "how often it is fetched automatically: hours or days such as 6h, 1d or 7d, or off"

// updatePeriodArg 把 --update 的值转换为订阅的周期设置：default 和空字符串都是默认值。
func updatePeriodArg(v string) (string, error) {
	if v == "" || v == "default" {
		return "", nil
	}
	_, err := settings.ParsePeriod(v)
	return v, err
}

func (a *app) newSubAddCommand() *cobra.Command {
	var userAgent, update string
	var include, exclude []string
	cmd := &cobra.Command{
		Use:   "add <name> <url|file|->",
		Short: "Fetch and save a subscription; the first one becomes active",
		Long: "Fetch and save a subscription; the first one becomes active. The source is a URL\n" +
			"(http or https), a local file, or - to read it from standard input, e.g.\n" +
			"ssh server tunlr sub add home - < airport.yaml. A URL is fetched again automatically\n" +
			"every day and a file is read again only by sub update, unless --update says otherwise\n" +
			"(interactively tunlr asks); a copy from standard input is never updated automatically.\n" +
			"The active subscription's configuration is published after each fetch that changed it.\n\n" +
			"--include and --exclude write a proxy filter for this subscription to\n" +
			"mixin.d/<name>/10-filter.yaml (tunlr.proxy-filter, the same as mihomo's filter and\n" +
			"exclude-filter): keep only the proxies whose names match --include, then drop those\n" +
			"matching --exclude. Repeat a flag for several regular expressions. Change it later with\n" +
			"tunlr config edit <name>/10-filter.",
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			period, err := updatePeriodArg(update)
			if err != nil {
				return err
			}
			stdin := args[1] == subscription.PathStdin
			if stdin && a.terminal() {
				return usageErrorf("- reads the subscription from standard input; redirect a file into it: tunlr sub add %s - < file.yaml", args[0])
			}
			if !cmd.Flags().Changed("update") && a.interactive() {
				initial := subscription.DefaultUpdate
				if !strings.Contains(args[1], "://") {
					initial = settings.PeriodOff
				}
				if period, err = a.choosePeriod(cmd, fmt.Sprintf("subscription %q", args[0]), initial); err != nil || period == "" {
					_, err := a.cancelled(err)
					return err
				}
			}
			opts := subscription.AddOptions{UserAgent: userAgent, Update: period,
				Filter: strings.Join(include, "`"), Exclude: strings.Join(exclude, "`")}
			if !cmd.Flags().Changed("include") && !cmd.Flags().Changed("exclude") && a.interactive() {
				if opts.Filter, opts.Exclude, err = a.askFilter(cmd, args[0]); err != nil {
					_, err := a.cancelled(err)
					return err
				}
			}
			if stdin {
				opts.Stdin = a.stdin
			}
			step := "Fetching the subscription"
			if !strings.Contains(args[1], "://") {
				step = "Reading the subscription"
			}
			var res *subscription.Fetched
			err = a.spin(step, func() (err error) {
				res, err = a.subscriptions().Add(cmd.Context(), args[0], args[1], opts)
				return err
			})
			if res == nil {
				return err
			}
			if serr := a.syncTimers(cmd); err == nil {
				err = serr
			}
			rerr := a.render(fetchedView{res.Name, res.Activated, res.Summary}, func() error {
				fmt.Fprintf(a.stdout, "Added subscription %q: %s; %s.\n", res.Name, describeSummary(res.Summary), describeFetching(period, args[1]))
				if opts.Filter != "" || opts.Exclude != "" {
					fmt.Fprintf(a.stdout, "Its proxy filter is in %s (tunlr config edit %s/10-filter).\n",
						config.MixinPath(a.layout.MixinDir(), res.Name+"/"+config.FilterMixinFile), res.Name)
				}
				if res.Activated {
					fmt.Fprintf(a.stdout, "%q is now the active subscription.\n", res.Name)
				}
				return nil
			})
			if err != nil {
				return renderedError{err}
			}
			return rerr
		},
	}
	cmd.Flags().StringVar(&userAgent, "user-agent", "",
		"User-Agent sent when fetching a URL (default: user-agent in "+a.layout.SettingsFile()+", or "+
			fmt.Sprintf("%q", subscription.DefaultUserAgent(a.info.Version))+")")
	cmd.Flags().StringVar(&update, "update", "", updateFlagHelp+" (default "+subscription.DefaultUpdate+" for a URL, off for a file)")
	cmd.Flags().StringArrayVar(&include, "include", nil, "keep only proxies whose names match this regular expression (repeatable)")
	cmd.Flags().StringArrayVar(&exclude, "exclude", nil, "drop proxies whose names match this regular expression (repeatable)")
	return cmd
}

// filterFields 是添加订阅时询问的节点筛选，两项都可以留空（留空表示不筛选）。菜单的表单、sub add 和 init
// 在终端里都问这两项；对应的参数是 --include、--exclude。
func filterFields() []inputField {
	check := func(s string) error { return config.CheckPatterns(s) }
	return []inputField{
		{title: "Keep only proxies matching (optional)",
			description: "A regular expression on proxy names, e.g. 香港|HK; separate several with `. Leave empty to keep all.",
			validate:    check},
		{title: "Drop proxies matching (optional)",
			description: "e.g. 倍率|剩余流量|官网. Leave empty to drop none. Change both later with: tunlr config edit <name>/10-filter",
			validate:    check},
	}
}

// askFilter 在终端里询问订阅 name 的节点筛选（见 filterFields）。
func (a *app) askFilter(cmd *cobra.Command, name string) (filter, exclude string, err error) {
	fields := filterFields()
	values := make([]string, len(fields))
	for i, f := range fields {
		title := f.title
		if i == 0 {
			title = fmt.Sprintf("Proxy filter for %q: %s", name, strings.ToLower(title[:1])+title[1:])
		}
		if values[i], err = a.prompt.Input(cmd.Context(), title, f.description, "", f.validate); err != nil {
			return "", "", err
		}
	}
	return values[0], values[1], nil
}

// describeFetching 是订阅自动更新周期的说法，如 "fetched again daily"；period 为空时是 source 的默认值。
func describeFetching(period, source string) string {
	file := !strings.Contains(source, "://")
	if period == "" {
		period = subscription.DefaultUpdate
		if file {
			period = settings.PeriodOff
		}
	}
	switch {
	case source == subscription.PathStdin:
		return "not updated automatically (it was read from standard input)"
	case file && period == settings.PeriodOff:
		return "read again by tunlr sub update"
	case file:
		return "read again " + settings.DescribePeriod(period)
	case period == settings.PeriodOff:
		return "not fetched again automatically"
	}
	return "fetched again " + settings.DescribePeriod(period)
}

// syncTimers 在订阅的增删改之后按新的情况启用或停用 tunlr-update.timer（还没有安装时什么都不做）。
func (a *app) syncTimers(cmd *cobra.Command) error {
	return a.installer().SyncTimers(cmd.Context())
}

// listSubscriptions 列出已保存的订阅（tunlr sub）。
func (a *app) listSubscriptions() error {
	entries, err := a.subscriptions().List()
	if err != nil {
		return err
	}
	views := make([]subscriptionView, 0, len(entries))
	for _, e := range entries {
		url := ""
		if e.URL != "" {
			url = subscription.Redact(e.URL)
		}
		views = append(views, subscriptionView{
			Name: e.Name, URL: url, Path: e.Path, Source: subscription.Describe(&e.Def), Active: e.Active, UserAgent: e.UserAgent,
			Update: e.UpdatePeriod(), AddedAt: e.AddedAt, FetchedAt: e.FetchedAt, LastError: e.LastError, LastErrorAt: e.LastErrorAt,
			UserInfo: e.UserInfo, Summary: e.Summary, Mixins: append([]string{}, e.Mixins...),
		})
	}
	return a.render(views, func() error {
		if len(views) == 0 {
			_, err := fmt.Fprintln(a.stdout, "No subscriptions. Add one with: tunlr sub add <name> <url|file>")
			return err
		}
		tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "\tNAME\tPROXIES\tUPDATED\tEVERY\tUSAGE\tEXPIRES\tMIXINS\tSOURCE")
		withMixins := false
		for _, v := range views {
			mark := ""
			if v.Active {
				mark = "*"
			}
			updated := formatTime(v.FetchedAt)
			if v.LastError != "" {
				updated += " (last update failed)"
			}
			mixins := "-"
			if len(v.Mixins) > 0 {
				withMixins = true
				mixins = strings.Join(trimYAML(v.Mixins), ",")
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				mark, v.Name, v.Summary.Proxies, updated, settings.DescribePeriod(v.Update), formatUsage(v.UserInfo), formatExpire(v.UserInfo), mixins, v.Source)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		if withMixins {
			_, err := fmt.Fprintln(a.stdout, "MIXINS are the subscription's own mixin files, applied only while it is active; edit one with: tunlr config edit <name>/<file>")
			return err
		}
		return nil
	})
}

// trimYAML 去掉 mixin 文件名的 .yaml，与 config edit 接受的写法相同。
func trimYAML(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strings.TrimSuffix(n, ".yaml")
	}
	return out
}

func (a *app) newSubRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [name...]",
		Short: "Remove saved subscriptions (not the active one)",
		Long: "Remove saved subscriptions. The active one cannot be removed; switch to another first.\n" +
			"Without names, choose them from a list (needs a terminal).",
		Args: usageArgs(cobra.ArbitraryArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			names := args
			if len(names) == 0 && !a.interactive() {
				return usageErrorf("sub remove needs the names of the subscriptions to remove (see: tunlr sub)")
			}
			if err := a.requireYes("sub remove"); err != nil {
				return err
			}
			if len(names) == 0 {
				var err error
				if names, err = a.chooseToRemove(cmd); err != nil || names == nil {
					_, err := a.cancelled(err)
					return err
				}
			}
			if ok, err := a.confirmRemove(cmd, names); !ok {
				return err
			}
			var removed []string
			for _, name := range names {
				if err := a.subscriptions().Remove(cmd.Context(), name); err != nil {
					if len(removed) > 0 && !a.opts.JSON {
						fmt.Fprintf(a.stdout, "Removed %s.\n", strings.Join(quoteAll(removed), ", "))
					}
					return err
				}
				removed = append(removed, name)
			}
			if err := a.syncTimers(cmd); err != nil {
				return err
			}
			return a.render(map[string][]string{"removed": removed}, func() error {
				for _, name := range removed {
					fmt.Fprintf(a.stdout, "Removed subscription %q.\n", name)
				}
				return nil
			})
		},
	}
}

// chooseToRemove 让用户勾选要删除的订阅（启用的那个不在列表中）。没有可删除的、或什么都没勾选时
// 告诉用户并返回 nil。
func (a *app) chooseToRemove(cmd *cobra.Command) ([]string, error) {
	entries, err := a.subscriptions().List()
	if err != nil {
		return nil, err
	}
	var candidates []subscription.Entry
	for _, e := range entries {
		if !e.Active {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		fmt.Fprintln(a.stdout, "Nothing to remove: the active subscription cannot be removed, and there is no other.")
		return nil, nil
	}
	picked, err := a.prompt.ChooseMany(cmd.Context(), "Remove which subscriptions?", subscriptionOptions(candidates), nil)
	if err != nil {
		return nil, err
	}
	if len(picked) == 0 {
		fmt.Fprintln(a.stdout, "Nothing selected; nothing removed.")
		return nil, nil
	}
	names := make([]string, len(picked))
	for i, p := range picked {
		names[i] = candidates[p].Name
	}
	return names, nil
}

// confirmRemove 在删除订阅前确认，说明中列出它们的地址（缩写，不显示令牌）。
func (a *app) confirmRemove(cmd *cobra.Command, names []string) (bool, error) {
	if a.opts.Yes {
		return true, nil
	}
	entries, err := a.subscriptions().List()
	if err != nil {
		return false, err
	}
	var lines []string
	for _, e := range entries {
		if slices.Contains(names, e.Name) {
			line := e.Name + "  " + subscription.Describe(&e.Def)
			if n, err := a.subscriptions().MixinFiles(e.Name); err == nil && n > 0 {
				line += fmt.Sprintf("  (and its %s in %s)", plural(n, "mixin file", "mixin files"), a.layout.SubscriptionMixinDir(e.Name))
			}
			lines = append(lines, line)
		}
	}
	desc := "Their saved copies are deleted; to get one back, add it again with its full URL."
	if len(lines) > 0 {
		desc = bullets(lines) + "\n" + desc
	}
	question := fmt.Sprintf("Remove the subscription %s?", strings.Join(quoteAll(names), ", "))
	if len(names) > 1 {
		question = fmt.Sprintf("Remove the subscriptions %s?", strings.Join(quoteAll(names), ", "))
	}
	return a.ask(cmd.Context(), question, desc, "Nothing removed.")
}

// chooseSubscription 让用户从全部订阅中选一个，光标起始于启用的那个。
func (a *app) chooseSubscription(cmd *cobra.Command, question string) (string, error) {
	entries, err := a.subscriptions().List()
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", errors.New("no subscriptions yet; add one with: tunlr sub add <name> <url|file>")
	}
	initial := 0
	for i, e := range entries {
		if e.Active {
			initial = i
		}
	}
	i, err := a.prompt.Choose(cmd.Context(), question, subscriptionOptions(entries), initial)
	if err != nil {
		return "", err
	}
	return entries[i].Name, nil
}

func quoteAll(names []string) []string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = strconv.Quote(n)
	}
	return q
}

func (a *app) newSubUpdateCommand() *cobra.Command {
	var userAgent, source string
	cmd := &cobra.Command{
		Use:   "update [name...]",
		Short: "Fetch subscriptions again (all of them by default)",
		Long: "Fetch subscriptions again (all of them by default); a local file is read again.\n" +
			"A subscription that fails to update keeps its last good copy. One read from standard\n" +
			"input is skipped unless named with --source -.\n\n" +
			"--user-agent changes the User-Agent of the named subscriptions; it is saved only\n" +
			"if the fetch succeeds. --user-agent \"\" goes back to the default, which is the\n" +
			"user-agent in " + a.layout.SettingsFile() + " if set, or the built-in one.\n\n" +
			"--source gives one subscription a new source: a URL, a file, or - for standard input\n" +
			"(e.g. ssh server tunlr sub update home --source - < airport.yaml). It is saved only if\n" +
			"the new source can be read.",
		Args: usageArgs(cobra.ArbitraryArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			var opts subscription.UpdateOptions
			if cmd.Flags().Changed("user-agent") {
				opts.UserAgent = &userAgent
			}
			if cmd.Flags().Changed("source") {
				if source == subscription.PathStdin && a.terminal() {
					return usageErrorf("--source - reads the subscription from standard input; redirect a file into it")
				}
				opts.Source = &source
				if source == subscription.PathStdin {
					opts.Stdin = a.stdin
				}
			}
			var results []subscription.UpdateResult
			err := a.spin("Fetching subscriptions", func() (err error) {
				results, err = a.subscriptions().Update(cmd.Context(), args, opts)
				return err
			})
			if results == nil {
				return err
			}
			views := make([]updateView, 0, len(results))
			for _, r := range results {
				v := updateView{Name: r.Name, OK: r.Err == nil, Summary: r.Summary}
				if r.Err != nil {
					v.Error = r.Err.Error()
				}
				views = append(views, v)
			}
			rerr := a.render(views, func() error {
				for _, v := range views {
					switch {
					case !a.style.on && v.OK:
						fmt.Fprintf(a.stdout, "ok      %s: %s\n", v.Name, describeSummary(v.Summary))
					case !a.style.on:
						fmt.Fprintf(a.stdout, "FAILED  %s: %s (keeping the last good copy)\n", v.Name, v.Error)
					case v.OK:
						a.printCheck("ok", v.Name+":", 0, describeSummary(v.Summary))
					default:
						a.printCheck("failed", v.Name+":", 0, v.Error+" (keeping the last good copy)")
					}
				}
				return nil
			})
			if err != nil {
				return renderedError{err}
			}
			return rerr
		},
	}
	cmd.Flags().StringVar(&userAgent, "user-agent", "", "change the User-Agent of the named subscriptions (\"\" restores the default)")
	cmd.Flags().StringVar(&source, "source", "", "read the named subscription from a new source: a URL, a file, or - (standard input)")
	return cmd
}

func (a *app) newSubUseCommand() *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "use [name]",
		Short: "Make a saved subscription the active one",
		Long: "Make a saved subscription the active one. Without a name, choose it from a list\n" +
			"(needs a terminal). With --apply, publish the configuration right away like\n" +
			"tunlr config apply (pass --yes too when not interactive); interactively tunlr asks whether\n" +
			"to publish it now.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if apply {
				if a.opts.JSON {
					return usageErrorf("--apply cannot be combined with --json; run tunlr config apply --json afterwards")
				}
				if err := a.requireConfirmation("sub use --apply"); err != nil {
					return err
				}
			}
			var name string
			if len(args) == 1 {
				name = args[0]
			} else {
				if !a.interactive() {
					return usageErrorf("sub use needs the name of a subscription (see: tunlr sub)")
				}
				var err error
				if name, err = a.chooseSubscription(cmd, "Use which subscription?"); err != nil {
					_, err := a.cancelled(err)
					return err
				}
			}
			if a.opts.DryRun {
				// 不切换订阅，只按它被启用时的样子预览配置。
				plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().PlanSubscription(cmd.Context(), name) })
				if err != nil {
					return err
				}
				return a.publish(cmd, plan, "")
			}
			res, err := a.subscriptions().Use(cmd.Context(), name)
			if err != nil {
				return err
			}
			err = a.render(fetchedView{res.Name, res.Activated, res.Summary}, func() error {
				var err error
				if res.Activated {
					_, err = fmt.Fprintf(a.stdout, "%s: %s.\n", a.style.good(fmt.Sprintf("Switched to %q", res.Name)), describeSummary(res.Summary))
				} else {
					_, err = fmt.Fprintf(a.stdout, "%q is already the active subscription.\n", res.Name)
				}
				return err
			})
			if err != nil || a.opts.JSON {
				return err
			}
			if !apply {
				if !res.Activated {
					return nil
				}
				if !a.interactive() {
					_, err := fmt.Fprintln(a.stdout, "Publish the new configuration with: tunlr config apply")
					return err
				}
				if apply, err = a.confirm(cmd.Context(), "Publish the new configuration now?", ""); err != nil || !apply {
					if err == nil {
						fmt.Fprintln(a.stdout, "Not published; run tunlr config apply when ready.")
					}
					return err
				}
			}
			fmt.Fprintln(a.stdout)
			plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().Plan(cmd.Context()) })
			if err != nil {
				return err
			}
			err = a.publish(cmd, plan, "Apply this configuration?")
			a.aborted = false // 订阅已经切换，不发布也不是什么都没做。
			return err
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "publish the configuration right away (like tunlr config apply)")
	return cmd
}

func describeSummary(s config.SubscriptionSummary) string {
	parts := []string{fmt.Sprintf("%d proxies", s.Proxies)}
	if s.ProxyProviders > 0 {
		parts = append(parts, fmt.Sprintf("%d proxy providers", s.ProxyProviders))
	}
	parts = append(parts, fmt.Sprintf("%d groups", s.ProxyGroups), fmt.Sprintf("%d rules", s.Rules))
	return strings.Join(parts, ", ")
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func formatUsage(info *state.UserInfo) string {
	if info == nil || info.Total == 0 {
		return "-"
	}
	return formatBytes(info.Upload+info.Download) + " / " + formatBytes(info.Total)
}

func formatExpire(info *state.UserInfo) string {
	if info == nil || info.Expire.IsZero() {
		return "-"
	}
	return info.Expire.Local().Format("2006-01-02")
}

// formatBytes 用二进制单位输出字节数，例如 12.3 GiB。
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
