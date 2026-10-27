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
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// subscriptionView 是订阅在 --json 输出中的形式。URL 只给出缩写，不输出访问令牌。
type subscriptionView struct {
	Name        string                     `json:"name"`
	URL         string                     `json:"url"`
	Active      bool                       `json:"active"`
	UserAgent   string                     `json:"user_agent,omitempty"`
	AddedAt     time.Time                  `json:"added_at"`
	FetchedAt   time.Time                  `json:"fetched_at,omitzero"`
	LastError   string                     `json:"last_error,omitempty"`
	LastErrorAt time.Time                  `json:"last_error_at,omitzero"`
	UserInfo    *state.UserInfo            `json:"userinfo,omitempty"`
	Summary     config.SubscriptionSummary `json:"summary"`
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
		Short: "Manage subscriptions (several can be saved, one is active)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		a.newSubAddCommand(),
		a.newSubListCommand(),
		a.newSubRemoveCommand(),
		a.newSubUpdateCommand(),
		a.newSubUseCommand(),
	)
	return cmd
}

func (a *app) newSubAddCommand() *cobra.Command {
	var userAgent string
	cmd := &cobra.Command{
		Use:   "add <name> <url>",
		Short: "Fetch and save a subscription; the first one becomes active",
		Args:  usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res *subscription.Fetched
			err := a.spin("Fetching the subscription", func() (err error) {
				res, err = a.subscriptions().Add(cmd.Context(), args[0], args[1], userAgent)
				return err
			})
			if err != nil {
				return err
			}
			return a.render(fetchedView{res.Name, res.Activated, res.Summary}, func() error {
				fmt.Fprintf(a.stdout, "Added subscription %q: %s.\n", res.Name, describeSummary(res.Summary))
				if res.Activated {
					fmt.Fprintf(a.stdout, "%q is now the active subscription.\n", res.Name)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&userAgent, "user-agent", "",
		"User-Agent sent when fetching (default: user-agent in "+a.layout.SettingsFile()+", or "+
			fmt.Sprintf("%q", subscription.DefaultUserAgent(a.info.Version))+")")
	return cmd
}

func (a *app) newSubListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved subscriptions",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := a.subscriptions().List()
			if err != nil {
				return err
			}
			views := make([]subscriptionView, 0, len(entries))
			for _, e := range entries {
				views = append(views, subscriptionView{
					Name: e.Name, URL: subscription.Redact(e.URL), Active: e.Active, UserAgent: e.UserAgent,
					AddedAt: e.AddedAt, FetchedAt: e.FetchedAt, LastError: e.LastError, LastErrorAt: e.LastErrorAt,
					UserInfo: e.UserInfo, Summary: e.Summary,
				})
			}
			return a.render(views, func() error {
				if len(views) == 0 {
					_, err := fmt.Fprintln(a.stdout, "No subscriptions. Add one with: tunlr sub add <name> <url>")
					return err
				}
				tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "\tNAME\tPROXIES\tUPDATED\tUSAGE\tEXPIRES\tURL")
				for _, v := range views {
					mark := ""
					if v.Active {
						mark = "*"
					}
					updated := formatTime(v.FetchedAt)
					if v.LastError != "" {
						updated += " (last update failed)"
					}
					fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
						mark, v.Name, v.Summary.Proxies, updated, formatUsage(v.UserInfo), formatExpire(v.UserInfo), v.URL)
				}
				return tw.Flush()
			})
		},
	}
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
				return usageErrorf("sub remove needs the names of the subscriptions to remove (see: tunlr sub list)")
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
	picked, err := a.prompt.ChooseMany(cmd.Context(), "Remove which subscriptions?", subscriptionOptions(candidates))
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
			lines = append(lines, e.Name+"  "+subscription.Redact(e.URL))
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
func (a *app) chooseSubscription(cmd *cobra.Command) (string, error) {
	entries, err := a.subscriptions().List()
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", errors.New("no subscriptions yet; add one with: tunlr sub add <name> <url>")
	}
	initial := 0
	for i, e := range entries {
		if e.Active {
			initial = i
		}
	}
	i, err := a.prompt.Choose(cmd.Context(), "Use which subscription?", subscriptionOptions(entries), initial)
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
	var userAgent string
	cmd := &cobra.Command{
		Use:   "update [name...]",
		Short: "Fetch subscriptions again (all of them by default)",
		Long: "Fetch subscriptions again (all of them by default). A subscription that fails\n" +
			"to update keeps its last good copy.\n\n" +
			"--user-agent changes the User-Agent of the named subscriptions; it is saved only\n" +
			"if the fetch succeeds. --user-agent \"\" goes back to the default, which is the\n" +
			"user-agent in " + a.layout.SettingsFile() + " if set, or the built-in one.",
		Args: usageArgs(cobra.ArbitraryArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			var opts subscription.UpdateOptions
			if cmd.Flags().Changed("user-agent") {
				opts.UserAgent = &userAgent
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
	return cmd
}

func (a *app) newSubUseCommand() *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "use [name]",
		Short: "Make a saved subscription the active one",
		Long: "Make a saved subscription the active one. Without a name, choose it from a list\n" +
			"(needs a terminal). With --apply, publish the configuration right away like\n" +
			"tunlr apply (pass --yes too when not interactive); interactively tunlr asks whether\n" +
			"to publish it now.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if apply {
				if a.opts.JSON {
					return usageErrorf("--apply cannot be combined with --json; run tunlr apply --json afterwards")
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
					return usageErrorf("sub use needs the name of a subscription (see: tunlr sub list)")
				}
				var err error
				if name, err = a.chooseSubscription(cmd); err != nil {
					_, err := a.cancelled(err)
					return err
				}
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
					_, err := fmt.Fprintln(a.stdout, "Publish the new configuration with: tunlr apply")
					return err
				}
				if apply, err = a.confirm(cmd.Context(), "Publish the new configuration now?", ""); err != nil || !apply {
					if err == nil {
						fmt.Fprintln(a.stdout, "Not published; run tunlr apply when ready.")
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
	cmd.Flags().BoolVar(&apply, "apply", false, "publish the configuration right away (like tunlr apply)")
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
