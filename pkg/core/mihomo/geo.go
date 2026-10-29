// Package mihomo 负责内核的下载、升级、已知可用版本管理、配置校验，以及经 Unix socket
// 访问内核 REST API 的 client。
//
// 本包不 import mihomo 的任何 Go 包，只与独立运行的内核进程交互。
package mihomo

import (
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

// release 报告来源是不是 GitHub release 的资产。
func (s GeoSource) release() bool { return s.Repo != "" }

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
	if s.release() || o.release() {
		return s.Repo == o.Repo && s.Tag == o.Tag && s.Asset == o.Asset
	}
	return withoutMirror(s.URL) == withoutMirror(o.URL)
}

// GeoSourceChanges 返回 home 中已有的 files 里，geox-url 从 old 变为 new 后来源不同的文件。缺失的文件
// 不在其中：mihomo -t 本来就按 new 下载它们（见 Validator）。
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

// ErrStagedGeoChanged 表示暂存的 geo 文件与计划时下载的不同（期间又生成了别的计划）。
var ErrStagedGeoChanged = errors.New("the geo data downloaded for this plan has changed; plan again")

// StagedGeo 返回 dir 中暂存的 files（mihomo -t 按新来源下载的，见 Validator.Staged）的 sha256，文件名为键；
// 没有暂存的不在其中。
func StagedGeo(dir string, files []GeoFile) (map[string]string, error) {
	sums := map[string]string{}
	for _, f := range files {
		sum, err := FileChecksum(filepath.Join(dir, f.Name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sums[f.Name] = sum
	}
	return sums, nil
}

// InstallStagedGeo 把暂存在 dir 中的文件（sums 是计划时记下的 sha256，见 StagedGeo）换进 home：替换已有的
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

// GeoStamps 返回 home 中 files 里已有的每个 geo 文件的修改时间和大小（键为 GeoFile.Name），用于比较内核
// 更新前后哪些文件换了：内容与上游相同时内核不写文件。
func GeoStamps(home string, files []GeoFile) (map[string]string, error) {
	stamps := map[string]string{}
	for _, f := range files {
		path, err := findGeo(home, f)
		if err != nil {
			return nil, err
		}
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		stamps[f.Name] = fmt.Sprintf("%d/%d", info.ModTime().UnixNano(), info.Size())
	}
	return stamps, nil
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
