package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Owner 是文件的属主，也用作降权运行进程的身份。方法的接收者为 nil 时什么都不做，
// 表示保持当前进程的身份（如尚未创建内核用户，或在测试中）。
type Owner struct {
	UID, GID int
}

// Chown 把 path 的属主设为 o，不跟随符号链接。
func (o *Owner) Chown(path string) error {
	if o == nil {
		return nil
	}
	return os.Lchown(path, o.UID, o.GID)
}

// ChownTree 把 root 及其下所有文件和目录的属主设为 o，不跟随符号链接。
func (o *Owner) ChownTree(root string) error {
	if o == nil {
		return nil
	}
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, o.UID, o.GID)
	})
}
