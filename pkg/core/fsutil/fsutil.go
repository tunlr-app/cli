// Package fsutil 提供文件写入的公共操作。
package fsutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile 原子地写入文件：先在同目录写临时文件并刷盘，再用 os.Rename 替换目标，
// 最后刷新目录。中途失败不会留下写了一半的目标文件。目标所在目录必须已存在。
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	return WriteFileOwned(path, data, perm, nil)
}

// WriteFileOwned 同 WriteFile，替换前把文件的属主设为 owner（为 nil 时不改变）。
func WriteFileOwned(path string, data []byte, perm fs.FileMode, owner *Owner) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
			err = fmt.Errorf("write %s: %w", path, err)
		}
	}()

	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = owner.Chown(tmp); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}
