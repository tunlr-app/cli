//go:build unix

package fsutil

import "os"

// syncDir 刷新目录，确保重命名本身已落盘，断电后不会回到旧文件。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
