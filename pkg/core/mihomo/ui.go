package mihomo

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

// zashboard 是默认的面板。模板中的 external-ui-url 指向它最新发布的 dist.zip。
const (
	zashboardRepo  = "Zephyruso/zashboard"
	zashboardAsset = "dist.zip"
	// ZashboardURL 是 zashboard 最新发布的官方下载地址。
	ZashboardURL = "https://github.com/" + zashboardRepo + "/releases/latest/download/" + zashboardAsset
)

// maxUISize 限制面板解压后的总大小，防止异常的压缩包耗尽磁盘。
const maxUISize = 256 << 20

// EnsureUI 确保 dir 中有面板。dir 已存在时什么都不做，与内核一致：实测内核启动时发现
// 面板目录已存在就跳过下载。否则经 d 下载 zashboard 的最新发布，按官方 sha256 校验，
// 解压后原子地放到 dir，属主设为 owner（内核在面板上手动更新时要能替换它）。返回是否新安装了面板。
//
// 内核自己下载面板时不做任何校验；面板在浏览器中运行并持有 API secret，
// 经镜像下载时尤其需要校验，因此由工具代为安装。
func EnsureUI(ctx context.Context, d *download.Client, dir string, owner *fsutil.Owner) (bool, error) {
	if _, err := os.Stat(dir); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	data, err := d.Fetch(ctx, zashboardRepo, "", zashboardAsset)
	if err != nil {
		return false, fmt.Errorf("install the panel: %w", err)
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, err
	}
	if err := owner.Chown(parent); err != nil {
		return false, err
	}
	tmp, err := os.MkdirTemp(parent, ".tunlr-ui-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	if err := extractZip(data, tmp); err != nil {
		return false, fmt.Errorf("install the panel: %w", err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return false, err
	}
	if err := owner.ChownTree(tmp); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		// 同时进行的另一次发布已经装好了面板。
		if _, serr := os.Stat(dir); serr == nil {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// extractZip 把压缩包解压到已存在的空目录 dst。所有条目位于同一个顶层目录（如 dist/）时
// 取该目录的内容，与内核的处理一致。拒绝指向 dst 之外的路径和非普通文件。
func extractZip(data []byte, dst string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	prefix := singleRoot(zr.File)
	var total uint64
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, prefix)
		if name == "" {
			continue
		}
		rel := filepath.FromSlash(strings.TrimSuffix(name, "/"))
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("archive entry %q points outside the target directory", f.Name)
		}
		p := filepath.Join(dst, rel)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if !f.Mode().IsRegular() {
			return fmt.Errorf("archive entry %q is not a regular file", f.Name)
		}
		if total += f.UncompressedSize64; total > maxUISize {
			return fmt.Errorf("archive expands to more than %d bytes", maxUISize)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := extractFile(f, p); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(f *zip.File, path string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	// 按声明的大小限制写入，声明与实际不符时 zip 包会报告校验错误。
	_, err = io.Copy(out, io.LimitReader(rc, int64(f.UncompressedSize64)))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// singleRoot 返回所有条目共同的顶层目录（带末尾斜杠），没有时返回空串。
func singleRoot(files []*zip.File) string {
	root := ""
	for _, f := range files {
		first, _, nested := strings.Cut(f.Name, "/")
		if !nested {
			return ""
		}
		if root == "" {
			root = first
		} else if first != root {
			return ""
		}
	}
	if root == "" {
		return ""
	}
	return root + "/"
}
