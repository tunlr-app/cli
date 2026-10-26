//go:build unix

package mihomo

import (
	"os/exec"
	"syscall"

	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

// runAs 让 cmd 以 owner 的身份运行；owner 为 nil 时不改变。
func runAs(cmd *exec.Cmd, owner *fsutil.Owner) error {
	if owner == nil {
		return nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(owner.UID), Gid: uint32(owner.GID)}
	return nil
}
