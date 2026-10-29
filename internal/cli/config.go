package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// managedView 是受管 mixin 的展示形式。独占键的只列出键（其中有 API secret）；提供默认值的给出内容。
type managedView struct {
	Name    string `json:"name"`
	Command string `json:"command,omitempty"`
	// Default 为 true 时它只提供默认值，用户的 mixin 可以覆盖；否则 Keys 中的键用户的 mixin 不能改动。
	Default bool     `json:"default"`
	Keys    []string `json:"keys,omitempty"`
	Content string   `json:"content,omitempty"`
	// Rewrites 是它在合成的最后改写的键，用户的 mixin 不能取消（目前只有 geox-url 经镜像）。
	Rewrites []string `json:"rewrites,omitempty"`
}

func newManagedViews(ms []config.Managed) []managedView {
	vs := make([]managedView, 0, len(ms))
	for _, m := range ms {
		v := managedView{Name: m.Name, Command: m.Command, Default: m.Default, Keys: m.Keys()}
		if m.Default {
			v.Content = string(m.Data)
		}
		if m.RewriteGeoxURL != nil {
			v.Rewrites = []string{"geox-url"}
		}
		vs = append(vs, v)
	}
	return vs
}

// writeManaged 输出受管 mixin：独占的每个一行，列出键和修改它们的命令；提供默认值的给出内容。
func writeManaged(w io.Writer, vs []managedView) {
	fmt.Fprintln(w, "Managed by tunlr (mixins cannot change these keys; use the command shown):")
	for _, v := range vs {
		if v.Default {
			continue
		}
		how := ""
		if v.Command != "" {
			how = " (" + v.Command + ")"
		}
		fmt.Fprintf(w, "  %s%s: %s\n", v.Name, how, strings.Join(v.Keys, ", "))
	}
	writeDefaults(w, vs)
}

// empty 报告提供默认值的受管 mixin 既没有内容也没有改写，例如没有配置 GitHub 镜像时的 tunlr:mirror。
func (v managedView) empty() bool {
	return strings.TrimSpace(v.Content) == "{}" && len(v.Rewrites) == 0
}

// writeDefaults 输出提供默认值的受管 mixin 的内容和改写，都没有的省略。
func writeDefaults(w io.Writer, vs []managedView) {
	for _, v := range vs {
		if !v.Default || v.empty() {
			continue
		}
		empty := strings.TrimSpace(v.Content) == "{}"
		how := "a mixin can override these"
		if strings.Contains("\n"+v.Content, "\ntunlr:") {
			how += ", remove rules with tunlr.rules.delete"
		}
		if v.Command != "" {
			how = v.Command + "; " + how
		}
		fmt.Fprintf(w, "Defaults from %s (%s):\n", v.Name, how)
		if !empty {
			for l := range strings.SplitSeq(strings.TrimRight(v.Content, "\n"), "\n") {
				fmt.Fprintf(w, "  %s\n", l)
			}
		}
		if len(v.Rewrites) > 0 {
			fmt.Fprintf(w, "  # then, after the mixins, https://github.com/ addresses in %s go through the mirror\n", strings.Join(v.Rewrites, ", "))
		}
	}
}

// writeMixinFiles 输出 mixin 文件：先是共用的，再按订阅分组，标出启用的订阅（只有它的在应用）。
func writeMixinFiles(w io.Writer, st styles, dir string, files []string, active string) {
	if len(files) == 0 {
		fmt.Fprintf(w, "No mixin files in %s (create one with: tunlr config edit).\n", dir)
		return
	}
	fmt.Fprintf(w, "Mixin files in %s (in order):\n", dir)
	group := ""
	for _, f := range files {
		sub, file := splitMixin(f)
		if sub != group {
			group = sub
			note := st.dim(" (not active, not applied)")
			if sub == active {
				note = " (active, applied after the shared files)"
			}
			fmt.Fprintf(w, "Mixins of subscription %s%s:\n", sub, note)
		}
		fmt.Fprintf(w, "  %s\n", file)
	}
}

// configView 是 tunlr config 在 --json 输出中的形式。
type configView struct {
	*setup.ConfigStatus
	History []revisionView `json:"history"`
	Dir     string         `json:"dir"`
	Files   []string       `json:"files"`
	Managed []managedView  `json:"managed"`
}

func (a *app) newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show the published configuration, its history and the mixins; plan, apply, rollback, edit",
		Long: "The configuration mihomo runs is composed from the template, the active subscription and\n" +
			"the mixin files in /etc/tunlr/mixin.d, checked with mihomo -t and published with apply.\n\n" +
			"Without a subcommand, show the published revision and whether there are changes not yet\n" +
			"published, the history rollback can go back to, your mixin files in the order they apply,\n" +
			"and the mixins tunlr generates for its own settings (TUN, the panel, the LAN proxy).\n" +
			"Your mixins may not set, change or delete the keys tunlr manages; plan, apply and edit\n" +
			"refuse such a mixin and name the command that changes the key instead. tunlr:dns,\n" +
			"tunlr:mirror and tunlr:filter only give defaults you can override.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error { return a.showConfig(cmd.Context()) },
	}
	cmd.AddCommand(
		a.withFullDiff(a.newPlanCommand()),
		a.withFullDiff(a.newApplyCommand()),
		a.withFullDiff(a.newRollbackCommand()),
		a.withFullDiff(a.newConfigEditCommand()),
	)
	return cmd
}

func (a *app) showConfig(ctx context.Context) error {
	cs, err := a.installer().ConfigStatus(ctx)
	if err != nil {
		return err
	}
	revs, current, err := a.pipeline().Revisions()
	if err != nil {
		return err
	}
	mixins, err := config.LoadAllMixins(a.layout.MixinDir())
	if err != nil {
		return err
	}
	managed, err := a.pipeline().Managed()
	if err != nil {
		return err
	}
	list, err := subscription.LoadList(a.layout.SubscriptionList())
	if err != nil {
		return err
	}
	active := list.Active
	v := configView{ConfigStatus: cs, History: newRevisionViews(revs, current), Dir: a.layout.MixinDir(), Files: []string{}, Managed: newManagedViews(managed)}
	for _, m := range mixins {
		v.Files = append(v.Files, m.Name)
	}
	return a.render(v, func() error {
		if cs.Revision == nil && cs.Drift == nil {
			fmt.Fprintf(a.stdout, "Configuration: %s (tunlr config apply publishes it)\n", a.style.warn("not published"))
		}
		a.printRevision(cs.Revision, cs.Drift)
		if len(v.History) > 0 {
			fmt.Fprintln(a.stdout, "\nHistory (tunlr config rollback publishes an earlier one again):")
			writeRevisions(a.stdout, v.History)
		}
		fmt.Fprintln(a.stdout)
		writeMixinFiles(a.stdout, a.style, v.Dir, v.Files, active)
		fmt.Fprintln(a.stdout)
		writeManaged(a.stdout, v.Managed)
		return nil
	})
}
