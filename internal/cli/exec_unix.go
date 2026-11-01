//go:build unix

package cli

import "syscall"

// execBinary 用 path 处的二进制替换本进程。
func execBinary(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
