// Package mihomo 负责内核的下载、升级、已知可用版本管理、配置校验，以及经 Unix socket
// 访问内核 REST API 的 client。
//
// 本包不 import mihomo 的任何 Go 包，只与独立运行的内核进程交互。
package mihomo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

// geo 数据来自 MetaCubeX/meta-rules-dat 的 latest 发布，与 mihomo 默认的 geox-url 一致。
const (
	geoRepo = "MetaCubeX/meta-rules-dat"
	geoTag  = "latest"
)

// GeoFile 是内核使用的一种 geo 数据文件。
type GeoFile struct {
	// Name 是工作目录中的文件名，内核按名称（不区分大小写）查找。
	Name string
	// Aliases 是内核同样接受的其他文件名。
	Aliases []string
	// Asset 是 meta-rules-dat 发布中对应的资产名。
	Asset string
}

// 文件名取自 mihomo 的 constant/path.go。
var (
	GeoMMDB  = GeoFile{Name: "geoip.metadb", Aliases: []string{"Country.mmdb", "geoip.db"}, Asset: "geoip.metadb"}
	GeoIPDat = GeoFile{Name: "GeoIP.dat", Asset: "geoip.dat"}
	GeoSite  = GeoFile{Name: "GeoSite.dat", Asset: "geosite.dat"}
	GeoASN   = GeoFile{Name: "ASN.mmdb", Asset: "GeoLite2-ASN.mmdb"}

	// AllGeoFiles 是内核使用的全部 geo 文件。
	AllGeoFiles = []GeoFile{GeoMMDB, GeoIPDat, GeoSite, GeoASN}
)

// URL 返回 f 在 GitHub 上的官方下载地址，与 mihomo 默认的 geox-url 相同。
func (f GeoFile) URL() string {
	return "https://github.com/" + geoRepo + "/releases/download/" + geoTag + "/" + f.Asset
}

// GeoxURL 返回配置中 geox-url 的各项（geoip、mmdb、asn、geosite），地址加上 mirror 前缀，
// 让内核自己定时更新 geo 数据时也经过镜像。mirror 为空时返回 nil，使用配置模板中的官方地址。
func GeoxURL(mirror string) map[string]string {
	if mirror == "" {
		return nil
	}
	return map[string]string{
		"geoip":   download.MirrorURL(mirror, GeoIPDat.URL()),
		"mmdb":    download.MirrorURL(mirror, GeoMMDB.URL()),
		"asn":     download.MirrorURL(mirror, GeoASN.URL()),
		"geosite": download.MirrorURL(mirror, GeoSite.URL()),
	}
}

// GeoFilesFor 返回满足 needs 所需的 geo 文件。
func GeoFilesFor(needs config.GeoNeeds) []GeoFile {
	var files []GeoFile
	if needs.GeoIP {
		if needs.GeodataMode {
			files = append(files, GeoIPDat)
		} else {
			files = append(files, GeoMMDB)
		}
	}
	if needs.GeoSite {
		files = append(files, GeoSite)
	}
	if needs.ASN {
		files = append(files, GeoASN)
	}
	return files
}

// EnsureGeo 确保 home 中有 files 列出的 geo 文件，缺失的经 d 下载并按官方 sha256 校验，
// 属主设为 owner（内核要能原地更新它们），返回新下载的文件名。已有的文件不在这里更新，见 VerifyGeo。
func EnsureGeo(ctx context.Context, d *download.Client, home string, files []GeoFile, owner *fsutil.Owner) ([]string, error) {
	var downloaded []string
	for _, f := range files {
		if p, err := findGeo(home, f); err != nil {
			return downloaded, err
		} else if p != "" {
			continue
		}
		if err := EnsureHome(home, owner); err != nil {
			return downloaded, err
		}
		data, err := d.Fetch(ctx, geoRepo, geoTag, f.Asset)
		if err != nil {
			return downloaded, fmt.Errorf("prepare %s: %w", f.Name, err)
		}
		if err := fsutil.WriteFileOwned(filepath.Join(home, f.Name), data, 0o644, owner); err != nil {
			return downloaded, err
		}
		downloaded = append(downloaded, f.Name)
	}
	return downloaded, nil
}

// VerifyGeo 按 meta-rules-dat 最新发布的官方 sha256 核对 home 中已有的 files。不一致的下载校验过的
// 文件替换（经镜像下载校验不通过时改为直连），返回被替换的文件名。替换用 rename，运行中的内核
// 仍使用原来的文件，重启后才加载新文件：内核的 geo 数据只能由它自己的更新器重新加载。
func VerifyGeo(ctx context.Context, d *download.Client, home string, files []GeoFile, owner *fsutil.Owner) ([]string, error) {
	rel, err := d.Release(ctx, geoRepo, geoTag)
	if err != nil {
		return nil, err
	}
	var replaced []string
	for _, f := range files {
		path, err := findGeo(home, f)
		if err != nil {
			return replaced, err
		}
		if path == "" {
			continue
		}
		asset, err := rel.Asset(f.Asset)
		if err != nil {
			return replaced, err
		}
		want, err := asset.SHA256()
		if err != nil {
			return replaced, err
		}
		have, err := os.ReadFile(path)
		if err != nil {
			return replaced, err
		}
		if Checksum(have) == want {
			continue
		}
		data, err := d.Get(ctx, asset)
		if errors.Is(err, download.ErrChecksumMismatch) && d.Mirror != "" {
			direct := *d
			direct.Mirror = ""
			data, err = direct.Get(ctx, asset)
		}
		if err != nil {
			return replaced, fmt.Errorf("replace %s: %w", filepath.Base(path), err)
		}
		if err := fsutil.WriteFileOwned(path, data, 0o644, owner); err != nil {
			return replaced, err
		}
		replaced = append(replaced, filepath.Base(path))
	}
	return replaced, nil
}

// EnsureHome 创建内核的工作目录，属主设为 owner，权限收紧到 0750：其中有内核的 API socket
// （0666、免 secret），只能由内核用户和 root 进入。
func EnsureHome(home string, owner *fsutil.Owner) error {
	if err := os.MkdirAll(home, 0o750); err != nil {
		return err
	}
	if err := os.Chmod(home, 0o750); err != nil {
		return err
	}
	return owner.Chown(home)
}

// findGeo 按内核的规则（文件名不区分大小写，含别名）在 dir 中查找 f，返回其路径；不存在时返回空串。
func findGeo(dir string, f GeoFile) (string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		for _, name := range append([]string{f.Name}, f.Aliases...) {
			if strings.EqualFold(e.Name(), name) {
				return filepath.Join(dir, e.Name()), nil
			}
		}
	}
	return "", nil
}
