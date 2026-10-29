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
	"regexp"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

// 内核默认的 geo 数据来自 MetaCubeX/meta-rules-dat 的 latest 发布（配置中没有写 geox-url 时）。
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
	// Key 是它在 geox-url 中的键。
	Key string
	// Asset 是 meta-rules-dat 发布中对应的资产名，用于内核的默认地址。
	Asset string
}

// 文件名取自 mihomo 的 constant/path.go。
var (
	GeoMMDB  = GeoFile{Name: "geoip.metadb", Aliases: []string{"Country.mmdb", "geoip.db"}, Key: "mmdb", Asset: "geoip.metadb"}
	GeoIPDat = GeoFile{Name: "GeoIP.dat", Key: "geoip", Asset: "geoip.dat"}
	GeoSite  = GeoFile{Name: "GeoSite.dat", Key: "geosite", Asset: "geosite.dat"}
	GeoASN   = GeoFile{Name: "ASN.mmdb", Key: "asn", Asset: "GeoLite2-ASN.mmdb"}

	// AllGeoFiles 是内核使用的全部 geo 文件。
	AllGeoFiles = []GeoFile{GeoMMDB, GeoIPDat, GeoSite, GeoASN}
)

// URL 返回 f 的默认下载地址，与 mihomo 默认的 geox-url 相同。
func (f GeoFile) URL() string {
	return "https://github.com/" + geoRepo + "/releases/download/" + geoTag + "/" + f.Asset
}

// GeoSource 是一个 geo 文件的下载来源。
type GeoSource struct {
	// URL 是配置中的地址（可能已经加上了镜像前缀），没有写时为默认地址。
	URL string
	// Repo、Tag、Asset 是去掉镜像前缀后，地址所指的 GitHub release 资产（Tag 为空表示最新的正式发布）；
	// 不是 GitHub release 资产时 Repo 为空。
	Repo, Tag, Asset string
}

// Verifiable 报告来源能否按 GitHub 提供的 sha256 校验：只有 GitHub release 的资产有官方校验值。
func (s GeoSource) Verifiable() bool { return s.Repo != "" }

// releaseAsset 匹配 GitHub release 资产的下载地址：releases/download/<tag>/<名称> 或
// releases/latest/download/<名称>。
var releaseAsset = regexp.MustCompile(`^https://github\.com/([^/]+/[^/]+)/releases/(?:download/([^/]+)|latest/download)/([^/?#]+)$`)

// Source 返回 f 在 geox-url 为 urls 时的来源。经 GitHub 镜像的地址（镜像前缀后接完整的 github.com 地址，
// 不论是 tunlr 设置的镜像还是订阅自己写的）去掉前缀后再判断。
func (f GeoFile) Source(urls map[string]string) GeoSource {
	s := GeoSource{URL: urls[f.Key]}
	if s.URL == "" {
		s.URL = f.URL()
	}
	raw := s.URL
	if i := strings.Index(raw, "/https://github.com/"); i >= 0 {
		raw = raw[i+1:]
	}
	if m := releaseAsset.FindStringSubmatch(raw); m != nil {
		s.Repo, s.Tag, s.Asset = m[1], m[2], m[3]
	}
	return s
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

// EnsureGeo 确保 home 中有 files 列出的 geo 文件，缺失的按 geox-url（urls，见 GeoFile.Source）经 d
// 下载：来源是 GitHub release 资产的按官方 sha256 校验，其他来源原样下载、不校验。属主设为 owner
// （内核要能原地更新它们），返回新下载的文件名。已有的文件不在这里更新，见 VerifyGeo。
func EnsureGeo(ctx context.Context, d *download.Client, home string, files []GeoFile, urls map[string]string, owner *fsutil.Owner) ([]string, error) {
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
		var data []byte
		var err error
		if src := f.Source(urls); src.Verifiable() {
			data, err = d.Fetch(ctx, src.Repo, src.Tag, src.Asset)
		} else {
			data, err = d.GetUnverified(ctx, f.Name, src.URL)
		}
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

// GeoCheck 是 VerifyGeo 的结果。
type GeoCheck struct {
	// Verified 是与官方 sha256 一致的文件，Replaced 是不一致、已换成校验过的文件的，Unverified 是
	// 来源不是 GitHub release 资产、无从校验的。都是工作目录中的文件名。
	Verified, Replaced, Unverified []string
}

// VerifyGeo 按 geox-url（urls）所指的 GitHub release 资产的官方 sha256 核对 home 中已有的 files。
// 不一致的下载校验过的文件替换（经镜像下载校验不通过时改为直连）。替换用 rename，运行中的内核仍使用
// 原来的文件，重启后才加载新文件：内核的 geo 数据只能由它自己的更新器重新加载。来源不是 GitHub
// release 资产的不核对，原样保留。
func VerifyGeo(ctx context.Context, d *download.Client, home string, files []GeoFile, urls map[string]string, owner *fsutil.Owner) (*GeoCheck, error) {
	res := &GeoCheck{}
	releases := map[string]*download.Release{}
	for _, f := range files {
		path, err := findGeo(home, f)
		if err != nil {
			return res, err
		}
		if path == "" {
			continue
		}
		name := filepath.Base(path)
		src := f.Source(urls)
		if !src.Verifiable() {
			res.Unverified = append(res.Unverified, name)
			continue
		}
		rel := releases[src.Repo+"@"+src.Tag]
		if rel == nil {
			if rel, err = d.Release(ctx, src.Repo, src.Tag); err != nil {
				return res, err
			}
			releases[src.Repo+"@"+src.Tag] = rel
		}
		asset, err := rel.Asset(src.Asset)
		if err != nil {
			return res, err
		}
		want, err := asset.SHA256()
		if err != nil {
			return res, err
		}
		have, err := FileChecksum(path)
		if err != nil {
			return res, err
		}
		if have == want {
			res.Verified = append(res.Verified, name)
			continue
		}
		data, err := d.Get(ctx, asset)
		if errors.Is(err, download.ErrChecksumMismatch) && d.Mirror != "" {
			direct := *d
			direct.Mirror = ""
			data, err = direct.Get(ctx, asset)
		}
		if err != nil {
			return res, fmt.Errorf("replace %s: %w", name, err)
		}
		if err := fsutil.WriteFileOwned(path, data, 0o644, owner); err != nil {
			return res, err
		}
		res.Replaced = append(res.Replaced, name)
	}
	return res, nil
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

// GeoModTime 返回 home 中 files 里最新的那个 geo 文件的修改时间，一个都没有时返回零值。
// 内核自己更新 geo 数据时不报告结果，只能由文件的修改时间知道它最近一次更新。
func GeoModTime(home string, files []GeoFile) (time.Time, error) {
	var newest time.Time
	for _, f := range files {
		path, err := findGeo(home, f)
		if err != nil {
			return time.Time{}, err
		}
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return time.Time{}, err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest, nil
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
