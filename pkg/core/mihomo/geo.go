// Package mihomo 负责内核的下载、升级、已知可用版本管理、配置校验，以及经 Unix socket
// 访问内核 REST API 的 client。
//
// 本包不 import mihomo 的任何 Go 包，只与独立运行的内核进程交互。
package mihomo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/download"
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

	allGeoFiles = []GeoFile{GeoMMDB, GeoIPDat, GeoSite, GeoASN}
)

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
// 返回新下载的文件名。已有的文件不更新，更新由内核的 geo-auto-update 负责。
func EnsureGeo(ctx context.Context, d *download.Client, home string, files []GeoFile) ([]string, error) {
	var downloaded []string
	for _, f := range files {
		if p, err := findGeo(home, f); err != nil {
			return downloaded, err
		} else if p != "" {
			continue
		}
		if err := os.MkdirAll(home, 0o755); err != nil {
			return downloaded, err
		}
		if err := d.FetchAsset(ctx, geoRepo, geoTag, f.Asset, filepath.Join(home, f.Name), 0o644); err != nil {
			return downloaded, fmt.Errorf("prepare %s: %w", f.Name, err)
		}
		downloaded = append(downloaded, f.Name)
	}
	return downloaded, nil
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
