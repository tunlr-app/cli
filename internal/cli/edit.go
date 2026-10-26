package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/templates"
)

// prompter 向用户提问。用户取消时返回 huh.ErrUserAborted。
type prompter interface {
	// Choose 让用户从 options 中选一项，返回其下标。
	Choose(ctx context.Context, title string, options []string) (int, error)
	// Input 让用户输入一行文字，validate 不通过时要求重新输入。
	Input(ctx context.Context, title string, validate func(string) error) (string, error)
}

type huhPrompter struct{ a *app }

func (p huhPrompter) Choose(ctx context.Context, title string, options []string) (int, error) {
	opts := make([]huh.Option[int], len(options))
	for i, o := range options {
		opts[i] = huh.NewOption(o, i)
	}
	var v int
	err := p.run(ctx, huh.NewSelect[int]().Title(title).Options(opts...).Value(&v))
	return v, err
}

func (p huhPrompter) Input(ctx context.Context, title string, validate func(string) error) (string, error) {
	var v string
	err := p.run(ctx, huh.NewInput().Title(title).Validate(validate).Value(&v))
	return v, err
}

func (p huhPrompter) run(ctx context.Context, f huh.Field) error {
	return huh.NewForm(huh.NewGroup(f)).WithInput(p.a.stdin).WithOutput(p.a.stderr).RunWithContext(ctx)
}

// 检查未通过时的选项。
const (
	choiceEdit    = "Edit again"
	choiceSave    = "Save without applying"
	choiceDiscard = "Discard my changes"
)

func (a *app) newEditCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "edit [name]",
		Short: "Edit a mixin file, check it, then apply",
		Long: "Edit a mixin file in /etc/tunlr/mixin.d with $VISUAL or $EDITOR; a new file starts\n" +
			"from a commented example that explains the syntax. After you save, the configuration\n" +
			"is composed and checked with mihomo -t. A mistake takes you back to the editor, and\n" +
			"nothing is saved until the check passes. Then the diff is shown and the configuration\n" +
			"is applied after confirmation. Without a name, choose a file or create a new one.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !a.interactive() {
				return usageErrorf("edit needs a terminal; edit the files in %s directly, then run tunlr apply", a.layout.MixinDir())
			}
			var name string
			var err error
			if len(args) == 1 {
				name, err = config.MixinFileName(args[0])
			} else {
				name, err = a.chooseMixin(cmd.Context())
			}
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			if err != nil {
				return err
			}
			return a.editMixin(cmd, name)
		},
	}
}

// chooseMixin 让用户选择已有的 mixin 文件，或输入新文件的名称。
func (a *app) chooseMixin(ctx context.Context) (string, error) {
	mixins, err := config.LoadMixins(a.layout.MixinDir())
	if err != nil {
		return "", err
	}
	if len(mixins) > 0 {
		options := make([]string, 0, len(mixins)+1)
		for _, m := range mixins {
			options = append(options, m.Name)
		}
		i, err := a.prompt.Choose(ctx, "Which mixin file?", append(options, "New file..."))
		if err != nil {
			return "", err
		}
		if i < len(mixins) {
			return mixins[i].Name, nil
		}
	}
	name, err := a.prompt.Input(ctx, "Name of the new mixin file (e.g. 20-rules)", func(s string) error {
		_, err := config.MixinFileName(s)
		return err
	})
	if err != nil {
		return "", err
	}
	return config.MixinFileName(name)
}

// editMixin 在临时副本上编辑 mixin 文件 name，检查通过后才写回，然后发布。
// 有错的编辑因此不会出现在 mixin.d 中，定时任务也不会读到。
func (a *app) editMixin(cmd *cobra.Command, name string) error {
	ctx := cmd.Context()
	dir := a.layout.MixinDir()
	path := filepath.Join(dir, name)
	original, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	start := original
	if !existed {
		start = templates.Mixin
	}

	tmp, err := os.CreateTemp("", "tunlr-"+strings.TrimSuffix(name, ".yaml")+"-*.yaml")
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
		edited := config.Mixin{Name: name, Data: data}

		plan, err := a.pipeline().PlanWithMixin(ctx, edited)
		var me *config.MixinError
		var choices []string
		switch {
		case errors.As(err, &me):
			fmt.Fprintf(a.stdout, "\n%v\n\n", err)
			choices = []string{choiceEdit, choiceDiscard}
		case err != nil:
			return err
		case errors.Is(plan.Check, core.ErrCheckFailed):
			a.printPlan(newPlanView(plan, false))
			fmt.Fprintln(a.stdout)
			choices = []string{choiceEdit, choiceDiscard}
		case plan.Check != nil && !errors.Is(plan.Check, mihomo.ErrNotInstalled):
			// 校验未能完成（如下载 geo 数据失败），不能说明配置有误。
			a.printPlan(newPlanView(plan, false))
			fmt.Fprintln(a.stdout)
			choices = []string{choiceEdit, choiceSave, choiceDiscard}
		}

		if choices != nil {
			i, err := a.prompt.Choose(ctx, "The mixin was not saved. What now?", choices)
			if err != nil && !errors.Is(err, huh.ErrUserAborted) {
				return err
			}
			choice := choiceDiscard
			if err == nil {
				choice = choices[i]
			}
			switch choice {
			case choiceEdit:
				continue
			case choiceSave:
				if err := a.saveMixin(edited, original, existed, &keep, tmp.Name()); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Saved %s; not applied. Run tunlr apply once the check can complete.\n", path)
				return nil
			}
			fmt.Fprintln(a.stdout, "Discarded; nothing saved.")
			return nil
		}

		if a.opts.DryRun {
			a.printPlan(newPlanView(plan, false))
			fmt.Fprintln(a.stdout, "\nDry run: the mixin was not saved.")
			return nil
		}
		if err := a.saveMixin(edited, original, existed, &keep, tmp.Name()); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Saved %s.\n\n", path)
		return a.publish(cmd, plan, "Apply this configuration?")
	}
}

// saveMixin 写回编辑结果。文件在编辑期间被别人改动时不覆盖，保留临时副本并告诉用户位置。
func (a *app) saveMixin(m config.Mixin, original []byte, existed bool, keep *bool, tmp string) error {
	err := config.SaveMixin(a.layout.MixinDir(), m, original, existed)
	if errors.Is(err, config.ErrMixinChanged) {
		*keep = true
		return fmt.Errorf("%w; your version is kept in %s", err, tmp)
	}
	return err
}

// runEditor 用 $VISUAL、$EDITOR，或系统中找到的编辑器打开文件。编辑器的值可以带参数（如 "code -w"）。
// 不随命令的 context 结束编辑器：终端里的 Ctrl-C 同时发给编辑器，由它自己处理。
func (a *app) runEditor(path string) error {
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	if editor == "" {
		for _, e := range []string{"editor", "nano", "vi"} {
			if _, err := exec.LookPath(e); err == nil {
				editor = e
				break
			}
		}
	}
	fields := strings.Fields(editor)
	if len(fields) == 0 {
		return errors.New("no editor found; set the EDITOR environment variable")
	}
	c := exec.Command(fields[0], append(fields[1:], path)...)
	c.Stdin, c.Stdout, c.Stderr = a.stdin, a.stdout, a.stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("editor %s: %w; nothing saved", fields[0], err)
	}
	return nil
}
