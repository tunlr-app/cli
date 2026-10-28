package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/templates"
)

// settingsView 是 tunlr settings 在 --json 输出中的形式。
type settingsView struct {
	File     string           `json:"file"`
	Settings []settings.Entry `json:"settings"`
}

func (a *app) newSettingsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Show tunlr's own settings; edit changes them",
		Long: "tunlr's own settings live in /etc/tunlr/settings.yaml: the GitHub mirror, the default\n" +
			"User-Agent for subscriptions, and how the geo data is updated. They are not part of the\n" +
			"mihomo configuration (change that with tunlr config edit). tunlr settings edit opens the file in an\n" +
			"editor, checks it, and applies the configuration when a change affects it.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := a.installer().SettingsEntries()
			if err != nil {
				return err
			}
			v := settingsView{File: a.layout.SettingsFile(), Settings: entries}
			return a.render(v, func() error {
				for _, e := range entries {
					value := e.Value
					switch {
					case e.Value == "":
						value = a.style.dim("none (direct)")
					case e.Default:
						value += a.style.dim(" (default)")
					}
					fmt.Fprintf(a.stdout, "%-13s %s\n", e.Key, value)
					fmt.Fprintf(a.stdout, "%-13s %s\n", "", a.style.dim(e.Help))
				}
				fmt.Fprintf(a.stdout, "\n(file %s; tunlr settings edit changes it)\n", v.File)
				return nil
			})
		},
	}
	cmd.AddCommand(a.withFullDiff(a.newSettingsEditCommand()))
	return cmd
}

func (a *app) newSettingsEditCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Edit tunlr's settings, check them, then apply",
		Long: "Edit /etc/tunlr/settings.yaml with $VISUAL, $EDITOR, or else vim, nano or vi. A new file\n" +
			"starts from commented defaults that explain each setting. A mistake takes you back to the\n" +
			"editor, and nothing is saved until the file is valid. When the change affects the\n" +
			"configuration (the GitHub mirror, geo data updated by mihomo), the diff is shown and the\n" +
			"configuration is applied after confirmation.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !a.interactive() {
				return usageErrorf("settings edit needs a terminal; edit %s directly, then run tunlr config apply", a.layout.SettingsFile())
			}
			return a.editSettings(cmd)
		},
	}
}

// editSettings 在临时副本上编辑设置文件，检查通过后才写回，然后发布。与 editMixin 一样，有错的编辑
// 不会写到设置文件中。
func (a *app) editSettings(cmd *cobra.Command) error {
	ctx := cmd.Context()
	path := a.layout.SettingsFile()
	original, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	start := original
	if !existed {
		start = templates.Settings
	}

	tmp, err := os.CreateTemp("", "tunlr-settings-*.yaml")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			os.Remove(tmp.Name())
		}
	}()
	_, err = tmp.Write(start)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}

	fmt.Fprintln(a.stdout, a.style.dim("After you save and quit, the settings are checked; changes to the configuration\n"+
		"are shown and nothing is applied until you confirm. Quit without saving to change nothing."))
	for {
		if err := a.editor(tmp.Name()); err != nil {
			return err
		}
		data, err := os.ReadFile(tmp.Name())
		if err != nil {
			return err
		}
		if bytes.Equal(data, start) {
			fmt.Fprintln(a.stdout, "No changes.")
			return nil
		}
		set, err := settings.Parse(data)
		if err != nil {
			fmt.Fprintf(a.stdout, "\nsettings: %v\n\n", err)
			choice, err := a.chooseAfterCheck(ctx, []string{choiceEdit, choiceDiscard},
				"The settings were not saved.", "The settings file stays as it was.")
			if err != nil {
				return err
			}
			if choice == choiceEdit {
				continue
			}
			a.aborted = true
			fmt.Fprintln(a.stdout, "Discarded; nothing saved.")
			return nil
		}

		if a.opts.DryRun {
			plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().PlanWithSettings(ctx, set) })
			if err != nil {
				return err
			}
			if plan.Current != nil && plan.Changed() {
				a.printPlan(newPlanView(plan, false))
				fmt.Fprintln(a.stdout)
			}
			fmt.Fprintln(a.stdout, "Dry run: the settings were not saved.")
			return nil
		}

		err = a.installer().SaveSettings(ctx, data, original, existed)
		if errors.Is(err, settings.ErrChanged) {
			keep = true
			return fmt.Errorf("%w; your version is kept in %s", err, tmp.Name())
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Saved %s.\n", path)

		plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().Plan(ctx) })
		switch {
		case err != nil:
			return fmt.Errorf("the settings are saved, but the configuration could not be composed: %w", err)
		case plan.Current == nil:
			fmt.Fprintln(a.stdout, "No configuration has been published yet; the settings take effect with tunlr config apply.")
			return nil
		case !plan.Changed():
			fmt.Fprintln(a.stdout, "The configuration stays the same.")
			return nil
		}
		fmt.Fprintln(a.stdout)
		err = a.publish(cmd, plan, "Apply this configuration?")
		if err == nil && a.aborted {
			fmt.Fprintln(a.stdout, "The settings stay saved; tunlr config apply publishes them later.")
		}
		a.aborted = false // 设置已经保存，不发布也不是什么都没做。
		return err
	}
}
