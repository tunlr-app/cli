// Package mihomo 负责内核的下载、升级、已知可用版本管理、配置校验，以及经 Unix socket
// 访问内核 REST API 的 client。
//
// 本包不 import mihomo 的任何 Go 包，只与独立运行的内核进程交互。
package mihomo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	if m := releaseAsset.FindStringSubmatch(withoutMirror(s.URL)); m != nil {
		s.Repo, s.Tag, s.Asset = m[1], m[2], m[3]
	}
	return s
}

// withoutMirror 去掉经 GitHub 镜像的地址的镜像前缀，其他地址原样返回。
func withoutMirror(url string) string {
	if i := strings.Index(url, "/https://github.com/"); i >= 0 {
		return url[i+1:]
	}
	return url
}

// Same 报告 s 与 o 是否为同一份数据：同一个 GitHub release 资产，或去掉镜像前缀后地址相同。只换了镜像
// 不算来源变化。
func (s GeoSource) Same(o GeoSource) bool {
	if s.Verifiable() || o.Verifiable() {
		return s.Repo == o.Repo && s.Tag == o.Tag && s.Asset == o.Asset
	}
	return withoutMirror(s.URL) == withoutMirror(o.URL)
}

// GeoSourceChanges 返回 home 中已有的 files 里，geox-url 从 old 变为 new 后来源不同的文件。缺失的文件
// 不在其中：它们本来就按 new 下载（见 EnsureGeo）。
func GeoSourceChanges(home string, files []GeoFile, old, new map[string]string) ([]GeoFile, error) {
	var changed []GeoFile
	for _, f := range files {
		if f.Source(old).Same(f.Source(new)) {
			continue
		}
		if p, err := findGeo(home, f); err != nil {
			return nil, err
		} else if p != "" {
			changed = append(changed, f)
		}
	}
	return changed, nil
}

// fetchGeo 按 geox-url（urls）下载 f：来源是 GitHub release 资产的按官方 sha256 校验，其他来源原样下载。
func fetchGeo(ctx context.Context, d *download.Client, f GeoFile, urls map[string]string) ([]byte, error) {
	if src := f.Source(urls); src.Verifiable() {
		return d.Fetch(ctx, src.Repo, src.Tag, src.Asset)
	}
	return d.GetUnverified(ctx, f.Name, f.Source(urls).URL)
}

// StageGeo 清空 dir，把 files 按 geox-url（urls）下载到其中（文件名为 GeoFile.Name，属主为 owner），
// 返回各文件的 sha256。用于来源变化的 geo 文件：校验时代替工作目录中的旧文件（见 Validator.Staged），
// 发布时换进工作目录（见 InstallStagedGeo）。
func StageGeo(ctx context.Context, d *download.Client, dir string, files []GeoFile, urls map[string]string, owner *fsutil.Owner) (map[string]string, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for _, f := range files {
		data, err := fetchGeo(ctx, d, f, urls)
		if err != nil {
			return nil, fmt.Errorf("download %s from its new source: %w", f.Name, err)
		}
		if err := fsutil.WriteFileOwned(filepath.Join(dir, f.Name), data, 0o644, owner); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		sums[f.Name] = hex.EncodeToString(sum[:])
	}
	return sums, nil
}

// ErrStagedGeoChanged 表示暂存的 geo 文件与计划时下载的不同（期间又生成了别的计划）。
var ErrStagedGeoChanged = errors.New("the geo data downloaded for this plan has changed; plan again")

// InstallStagedGeo 把 StageGeo 暂存在 dir 中的文件（sums 是计划时记下的 sha256）换进 home：替换已有的
// 同类文件（文件名可能是别名，原样保留），原来的文件移到 dir 中。返回的 undo 把原来的文件换回去。
// 先核对全部暂存文件，有不一致时什么都不动，返回 ErrStagedGeoChanged。
func InstallStagedGeo(dir, home string, sums map[string]string) (undo func() error, err error) {
	type move struct{ staged, target, old string }
	var moves []move
	for _, f := range AllGeoFiles {
		want, ok := sums[f.Name]
		if !ok {
			continue
		}
		staged := filepath.Join(dir, f.Name)
		if have, err := FileChecksum(staged); err != nil || have != want {
			return nil, ErrStagedGeoChanged
		}
		target, err := findGeo(home, f)
		if err != nil {
			return nil, err
		}
		if target == "" {
			target = filepath.Join(home, f.Name)
		}
		moves = append(moves, move{staged, target, staged + ".old"})
	}
	var done []move
	undo = func() error {
		var errs []error
		for _, m := range slices.Backward(done) {
			if err := os.Rename(m.old, m.target); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	for _, m := range moves {
		if err := os.Rename(m.target, m.old); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, errors.Join(err, undo())
		}
		done = append(done, m)
		if err := os.Rename(m.staged, m.target); err != nil {
			return nil, errors.Join(err, undo())
		}
	}
	return undo, nil
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
		data, err := fetchGeo(ctx, d, f, urls)
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
