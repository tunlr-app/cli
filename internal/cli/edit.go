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
	"slices"
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
	// Choose 让用户用上下箭头从 options 中选一项，光标起始于下标 initial，返回所选的下标。
	Choose(ctx context.Context, title string, options []string, initial int) (int, error)
	// ChooseMany 让用户勾选 options 中的若干项，返回所选的下标（按列表顺序）。
	ChooseMany(ctx context.Context, title string, options []string) ([]int, error)
	// Input 让用户输入一行文字，description 是标题下的说明，validate 不通过时要求重新输入。
	Input(ctx context.Context, title, description string, validate func(string) error) (string, error)
}

type huhPrompter struct{ a *app }

// maxListRows 是列表一次显示的最多行数，更多时滚动。
const maxListRows = 10

func intOptions(options []string) []huh.Option[int] {
	opts := make([]huh.Option[int], len(options))
	for i, o := range options {
		opts[i] = huh.NewOption(o, i)
	}
	return opts
}

// listHeight 是选择列表的高度：最多 maxListRows 行选项，加上标题等 header 行。总是显式设置：
// 不设置时 huh 按选项行数定高，再减去标题和说明，选项少时一行都显示不出来。
func listHeight(options []string, header int) int {
	return min(len(options), maxListRows) + header
}

func (p huhPrompter) Choose(ctx context.Context, title string, options []string, initial int) (int, error) {
	v := initial
	s := huh.NewSelect[int]().Title(title).Options(intOptions(options)...).Value(&v).
		Height(listHeight(options, 1))
	err := p.run(ctx, s)
	return v, err
}

func (p huhPrompter) ChooseMany(ctx context.Context, title string, options []string) ([]int, error) {
	var v []int
	// 按键（x 或空格勾选、ctrl+a 全选）显示在底部的帮助行中。
	s := huh.NewMultiSelect[int]().Title(title).Options(intOptions(options)...).Value(&v).
		Height(listHeight(options, 1))
	err := p.run(ctx, s)
	slices.Sort(v)
	return v, err
}

func (p huhPrompter) Input(ctx context.Context, title, description string, validate func(string) error) (string, error) {
	var v string
	err := p.run(ctx, huh.NewInput().Title(title).Description(description).Validate(validate).Value(&v))
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

// chooseMixin 让用户选择已有的 mixin 文件，或输入新文件的名称。输入名称时列出已有文件的
// 应用顺序，方便选择数字前缀。
func (a *app) chooseMixin(ctx context.Context) (string, error) {
	mixins, err := config.LoadMixins(a.layout.MixinDir())
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(mixins))
	for _, m := range mixins {
		names = append(names, m.Name)
	}
	if len(names) > 0 {
		i, err := a.prompt.Choose(ctx, "Which mixin file?", append(slices.Clone(names), "New file..."), 0)
		if err != nil {
			return "", err
		}
		if i < len(names) {
			return names[i], nil
		}
	}
	desc := "Files are applied in name order and later files win; start the name with a number to place it."
	if len(names) > 0 {
		desc += "\nCurrent order: " + strings.Join(names, " → ")
	}
	name, err := a.prompt.Input(ctx, "Name of the new mixin file (e.g. 20-rules)", desc, func(s string) error {
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
			i, err := a.prompt.Choose(ctx, "The mixin was not saved. What now?", choices, 0)
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
