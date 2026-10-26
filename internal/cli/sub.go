package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/config"
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
			res, err := a.subscriptions().Add(cmd.Context(), args[0], args[1], userAgent)
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
		"User-Agent sent when fetching (default "+fmt.Sprintf("%q", subscription.DefaultUserAgent(a.info.Version))+")")
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
		Use:   "remove <name>",
		Short: "Remove a saved subscription (not the active one)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.subscriptions().Remove(cmd.Context(), args[0]); err != nil {
				return err
			}
			return a.render(map[string]string{"name": args[0]}, func() error {
				_, err := fmt.Fprintf(a.stdout, "Removed subscription %q.\n", args[0])
				return err
			})
		},
	}
}

func (a *app) newSubUpdateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "update [name...]",
		Short: "Fetch subscriptions again (all of them by default)",
		Long: "Fetch subscriptions again (all of them by default). A subscription that fails\n" +
			"to update keeps its last good copy.",
		Args: usageArgs(cobra.ArbitraryArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			results, err := a.subscriptions().Update(cmd.Context(), args)
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
					if v.OK {
						fmt.Fprintf(a.stdout, "ok      %s: %s\n", v.Name, describeSummary(v.Summary))
					} else {
						fmt.Fprintf(a.stdout, "FAILED  %s: %s (keeping the last good copy)\n", v.Name, v.Error)
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
}

func (a *app) newSubUseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Make a saved subscription the active one",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := a.subscriptions().Use(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.render(fetchedView{res.Name, res.Activated, res.Summary}, func() error {
				var err error
				if res.Activated {
					_, err = fmt.Fprintf(a.stdout, "Switched to %q: %s.\n", res.Name, describeSummary(res.Summary))
				} else {
					_, err = fmt.Fprintf(a.stdout, "%q is already the active subscription.\n", res.Name)
				}
				return err
			})
		},
	}
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
