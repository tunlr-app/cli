package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
)

// 退出码，含义见 CLAUDE.md。
const (
	exitOK          = 0
	exitError       = 1
	exitUsage       = 2
	exitCheckFailed = 3
	exitRolledBack  = 4
)

// usageError 表示参数用法错误。
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// renderedError 表示命令已经输出了包含失败信息的结果（例如部分订阅更新失败），
// 只需按内部错误设置退出码，不再重复报告。
type renderedError struct{ err error }

func (e renderedError) Error() string { return e.err.Error() }
func (e renderedError) Unwrap() error { return e.err }

func usageErrorf(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// usageArgs 把 cobra 的参数校验错误包装为用法错误。
// 叶子命令必须显式设置 Args，否则 cobra 会静默接受多余参数。
func usageArgs(fn cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := fn(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

func exitCode(err error) int {
	var ue usageError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ue), errors.Is(err, core.ErrInvalidArgument):
		return exitUsage
	case errors.Is(err, core.ErrCheckFailed):
		return exitCheckFailed
	case errors.Is(err, core.ErrRolledBack):
		return exitRolledBack
	default:
		return exitError
	}
}
