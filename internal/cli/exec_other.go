//go:build !unix

package cli

import "errors"

// execBinary 只在 Linux 上可用；其他系统只用于编译和单元测试。
func execBinary(string, []string, []string) error {
	return errors.New("replacing the process is not supported on this system")
}
