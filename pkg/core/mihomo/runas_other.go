//go:build !unix

package mihomo

import (
	"errors"
	"os/exec"

	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

// runAs 让 cmd 以 owner 的身份运行；owner 为 nil 时不改变。只有 Unix 支持切换身份。
func runAs(_ *exec.Cmd, owner *fsutil.Owner) error {
	if owner == nil {
		return nil
	}
	return errors.New("running as another user is only supported on Unix")
}
