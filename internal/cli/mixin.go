package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/config"
)

// managedView 是受管 mixin 的展示形式。独占键的只列出键（其中有 API secret）；提供默认值的给出内容。
type managedView struct {
	Name    string `json:"name"`
	Command string `json:"command,omitempty"`
	// Default 为 true 时它只提供默认值，用户的 mixin 可以覆盖；否则 Keys 中的键用户的 mixin 不能改动。
	Default bool     `json:"default"`
	Keys    []string `json:"keys,omitempty"`
	Content string   `json:"content,omitempty"`
}

func newManagedViews(ms []config.Managed) []managedView {
	vs := make([]managedView, 0, len(ms))
	for _, m := range ms {
		v := managedView{Name: m.Name, Command: m.Command, Default: m.Default, Keys: m.Keys()}
		if m.Default {
			v.Content = string(m.Data)
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

// writeDefaults 输出提供默认值的受管 mixin 的内容，没有内容的省略。
func writeDefaults(w io.Writer, vs []managedView) {
	for _, v := range vs {
		if !v.Default || strings.TrimSpace(v.Content) == "{}" {
			continue
		}
		fmt.Fprintf(w, "Defaults from %s (%s; a mixin can override these, remove rules with delete-rules):\n", v.Name, v.Command)
		for l := range strings.SplitSeq(strings.TrimRight(v.Content, "\n"), "\n") {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
}

type mixinListView struct {
	Dir     string        `json:"dir"`
	Files   []string      `json:"files"`
	Managed []managedView `json:"managed"`
}

func (a *app) newMixinCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mixin",
		Short: "List the mixin files and the mixins tunlr manages; edit changes a file",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.listMixins() },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the mixin files and the mixins tunlr manages",
		Long: "List your mixin files in /etc/tunlr/mixin.d in the order they apply, and the mixins tunlr\n" +
			"generates for its own settings (TUN, the panel, geo data updates). Your mixins may not set,\n" +
			"change or delete the keys tunlr manages; plan, apply and edit refuse such a mixin and name\n" +
			"the command that changes the key instead. tunlr:mirror only gives defaults you can override.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error { return a.listMixins() },
	}, a.withFullDiff(a.newMixinEditCommand()))
	return cmd
}

func (a *app) listMixins() error {
	mixins, err := config.LoadMixins(a.layout.MixinDir())
	if err != nil {
		return err
	}
	managed, err := a.pipeline().Managed()
	if err != nil {
		return err
	}
	v := mixinListView{Dir: a.layout.MixinDir(), Files: []string{}, Managed: newManagedViews(managed)}
	for _, m := range mixins {
		v.Files = append(v.Files, m.Name)
	}
	return a.render(v, func() error {
		if len(v.Files) == 0 {
			fmt.Fprintf(a.stdout, "No mixin files in %s (create one with: tunlr mixin edit).\n", v.Dir)
		} else {
			fmt.Fprintf(a.stdout, "Mixin files in %s (in order):\n", v.Dir)
			for _, f := range v.Files {
				fmt.Fprintf(a.stdout, "  %s\n", f)
			}
		}
		fmt.Fprintln(a.stdout)
		writeManaged(a.stdout, v.Managed)
		return nil
	})
}
