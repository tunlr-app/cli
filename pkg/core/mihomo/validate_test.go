package mihomo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
)

// fakeMihomoEnv 置位时，测试二进制扮演 mihomo：只实现 -t -d <dir> -f <file>。
const fakeMihomoEnv = "TUNLR_FAKE_MIHOMO"

func TestMain(m *testing.M) {
	if os.Getenv(fakeMihomoEnv) != "" {
		os.Exit(fakeMihomo(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeMihomo 按配置内容模拟 mihomo -t 的输出：引用不存在的策略组时报配置错误；
// 有 GEOIP 规则而工作目录中没有 MMDB 时，配置含 DOWNLOAD 则像真实内核一样下载到工作目录，否则报下载失败；
// 含 HANG 时挂起。
func fakeMihomo(args []string) int {
	if len(args) != 5 || args[0] != "-t" || args[1] != "-d" || args[3] != "-f" {
		fmt.Println("unexpected args", args)
		return 2
	}
	cfg, err := os.ReadFile(args[4])
	if err != nil {
		fmt.Println(err)
		return 2
	}
	switch s := string(cfg); {
	case strings.Contains(s, "HANG"):
		time.Sleep(time.Hour)
	case strings.Contains(s, "MATCH,NOPE"):
		fmt.Println(`time="2026-10-26T17:00:00+08:00" level=error msg="rules[0] [MATCH,NOPE] error: proxy [NOPE] not found"`)
		fmt.Println("configuration file " + args[4] + " test failed")
		return 1
	case strings.Contains(s, "GEOIP"):
		// 与真实内核一样，按文件名不区分大小写查找。
		if p, _ := findGeo(args[2], GeoMMDB); p == "" && strings.Contains(s, "DOWNLOAD") {
			if err := os.WriteFile(filepath.Join(args[2], GeoMMDB.Name), []byte("downloaded"), 0o644); err != nil {
				fmt.Println(err)
				return 2
			}
		} else if p == "" {
			fmt.Println(`time="2026-10-26T17:00:00+08:00" level=error msg="can't initial GeoIP: can't download MMDB: Get \"https://github.com/x\": context deadline exceeded"`)
			return 1
		}
	case strings.Contains(s, "SILENT"):
		return 1
	}
	fmt.Println("configuration file " + args[4] + " test is successful")
	return 0
}

func fakeValidator(t *testing.T) *Validator {
	t.Setenv(fakeMihomoEnv, "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Validator{Binary: exe, HomeDir: t.TempDir()}
}

func TestValidate(t *testing.T) {
	v := fakeValidator(t)
	ctx := context.Background()

	if err := v.Validate(ctx, []byte("rules:\n  - MATCH,DIRECT\n"), nil); err != nil {
		t.Fatalf("valid config: %v", err)
	}

	err := v.Validate(ctx, []byte("rules:\n  - MATCH,NOPE\n"), nil)
	var ce *CheckError
	if !errors.As(err, &ce) || !errors.Is(err, core.ErrCheckFailed) || ce.MissingData {
		t.Fatalf("bad config: err = %v", err)
	}
	if len(ce.Messages) != 1 || ce.Messages[0] != "rules[0] [MATCH,NOPE] error: proxy [NOPE] not found" {
		t.Errorf("messages = %q", ce.Messages)
	}

	// 没有 MMDB 且内核下载失败：报告为数据缺失，而不是配置错误。
	err = v.Validate(ctx, []byte("rules:\n  - GEOIP,CN,DIRECT\n"), nil)
	if !errors.As(err, &ce) || !ce.MissingData || errors.Is(err, core.ErrCheckFailed) {
		t.Fatalf("missing geo: err = %v", err)
	}
	if !strings.Contains(err.Error(), `Get "https://github.com/x"`) {
		t.Errorf("escaped quotes not decoded: %v", err)
	}

	// 工作目录中已有 MMDB（大小写不同也算）时，以链接提供给校验。
	if err := os.WriteFile(filepath.Join(v.HomeDir, "GeoIP.metadb"), []byte("mmdb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(ctx, []byte("rules:\n  - GEOIP,CN,DIRECT\n"), nil); err != nil {
		t.Fatalf("with geo present: %v", err)
	}

	err = v.Validate(ctx, []byte("SILENT: true\n"), nil)
	if !errors.As(err, &ce) || ce.Messages[0] != "mihomo -t exited with status 1" {
		t.Errorf("silent failure: err = %v", err)
	}
}

func TestValidateTimeout(t *testing.T) {
	v := fakeValidator(t)
	v.Timeout = 300 * time.Millisecond
	err := v.Validate(context.Background(), []byte("HANG: true\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "did not finish within 300ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateMissingBinary(t *testing.T) {
	v := &Validator{Binary: filepath.Join(t.TempDir(), "no-mihomo")}
	err := v.Validate(context.Background(), []byte("rules: []\n"), nil)
	var ce *CheckError
	if !errors.Is(err, ErrNotInstalled) || errors.As(err, &ce) {
		t.Fatalf("err = %v, want ErrNotInstalled", err)
	}
}

// TestValidateKeepsGeo 验证 mihomo -t 下载的 geo 文件：Keep 时存进工作目录，否则丢弃；
// 来源变了的文件不提供旧的，下载到的存进 Staged，工作目录中的旧文件不动。
func TestValidateKeepsGeo(t *testing.T) {
	ctx := context.Background()
	cfg := []byte("DOWNLOAD: true\nrules:\n  - GEOIP,CN,DIRECT\n")
	mmdb := func(dir string) string {
		data, _ := os.ReadFile(filepath.Join(dir, GeoMMDB.Name))
		return string(data)
	}

	v := fakeValidator(t)
	if err := v.Validate(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if got := mmdb(v.HomeDir); got != "" {
		t.Fatalf("read-only check kept %q", got)
	}

	v.Keep = true
	if err := v.Validate(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if got := mmdb(v.HomeDir); got != "downloaded" {
		t.Fatalf("home = %q", got)
	}

	if err := os.WriteFile(filepath.Join(v.HomeDir, GeoMMDB.Name), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.Fresh, v.Staged = []GeoFile{GeoMMDB}, filepath.Join(t.TempDir(), "staged")
	if err := v.Validate(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if home, staged := mmdb(v.HomeDir), mmdb(v.Staged); home != "old" || staged != "downloaded" {
		t.Fatalf("home = %q, staged = %q", home, staged)
	}
}

// TestValidateTimeoutForDownloads 验证要现场下载时放宽时限：文件缺失或来源变了。
func TestValidateTimeoutForDownloads(t *testing.T) {
	v := &Validator{HomeDir: t.TempDir()}
	needs := config.GeoNeeds{GeoIP: true}
	if !v.downloads(needs) {
		t.Error("missing file: no download expected")
	}
	if err := os.WriteFile(filepath.Join(v.HomeDir, GeoMMDB.Name), []byte("mmdb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v.downloads(needs) || v.downloads(config.GeoNeeds{}) {
		t.Error("present file: download expected")
	}
	v.Fresh = []GeoFile{GeoMMDB}
	if !v.downloads(needs) {
		t.Error("changed source: no download expected")
	}
}

func TestGeoFilesFor(t *testing.T) {
	names := func(files []GeoFile) string {
		var s []string
		for _, f := range files {
			s = append(s, f.Name)
		}
		return strings.Join(s, ",")
	}
	cases := []struct {
		cfg, want string
	}{
		{"rules:\n  - MATCH,DIRECT\n", ""},
		{"rules:\n  - GEOIP,CN,DIRECT\n  - GEOSITE,cn,DIRECT\n  - IP-ASN,1,DIRECT\n", "geoip.metadb,GeoSite.dat,ASN.mmdb"},
		{"geodata-mode: true\nrules:\n  - GEOIP,CN,DIRECT\n", "GeoIP.dat"},
	}
	for _, c := range cases {
		needs, err := config.DetectGeo([]byte(c.cfg))
		if err != nil {
			t.Fatal(err)
		}
		if got := names(GeoFilesFor(needs)); got != c.want {
			t.Errorf("%q: got %s, want %s", c.cfg, got, c.want)
		}
	}
}

func TestGeoFileSource(t *testing.T) {
	for _, tt := range []struct {
		urls map[string]string
		want GeoSource
	}{
		{nil, GeoSource{URL: GeoMMDB.URL(), Repo: geoRepo, Tag: geoTag, Asset: "geoip.metadb"}},
		{map[string]string{"mmdb": "https://github.com/a/b/releases/latest/download/country.mmdb"},
			GeoSource{URL: "https://github.com/a/b/releases/latest/download/country.mmdb", Repo: "a/b", Asset: "country.mmdb"}},
		{map[string]string{"mmdb": "https://m.example/https://github.com/a/b/releases/download/v1/c.mmdb"},
			GeoSource{URL: "https://m.example/https://github.com/a/b/releases/download/v1/c.mmdb", Repo: "a/b", Tag: "v1", Asset: "c.mmdb"}},
		{map[string]string{"mmdb": "https://github.com/a/b/raw/main/c.mmdb"}, GeoSource{URL: "https://github.com/a/b/raw/main/c.mmdb"}},
		{map[string]string{"mmdb": "https://ghfast.example/https://github.com/a/b/releases/download/v2/c.mmdb"},
			GeoSource{URL: "https://ghfast.example/https://github.com/a/b/releases/download/v2/c.mmdb", Repo: "a/b", Tag: "v2", Asset: "c.mmdb"}},
		{map[string]string{"mmdb": "https://cdn.example/c.mmdb"}, GeoSource{URL: "https://cdn.example/c.mmdb"}},
	} {
		if got := GeoMMDB.Source(tt.urls); got != tt.want {
			t.Errorf("Source(%v) = %+v, want %+v", tt.urls, got, tt.want)
		}
	}
}

func TestGeoSourceSame(t *testing.T) {
	gh := "https://github.com/a/b/releases/download/v1/geosite.dat"
	for _, tt := range []struct {
		a, b string
		same bool
	}{
		{gh, gh, true},
		{gh, "https://m.example/" + gh, true},
		{gh, "https://github.com/a/b/releases/download/v2/geosite.dat", false},
		{gh, "https://cdn.example/geosite.dat", false},
		{"https://cdn.example/geosite.dat", "https://cdn.example/geosite.dat", true},
		{"https://github.com/a/b/raw/main/geosite.dat", "https://m.example/https://github.com/a/b/raw/main/geosite.dat", true},
	} {
		a, b := GeoSite.Source(map[string]string{"geosite": tt.a}), GeoSite.Source(map[string]string{"geosite": tt.b})
		if a.Same(b) != tt.same || b.Same(a) != tt.same {
			t.Errorf("%s vs %s: same = %v", tt.a, tt.b, a.Same(b))
		}
	}
}

// TestStageGeo 验证来源变化的 geo 文件：只列出已有、来源变了的；暂存的文件发布时换进工作目录，可以换回；
// 暂存的文件被改过时拒绝替换。
func TestStageGeo(t *testing.T) {
	home, stage := t.TempDir(), filepath.Join(t.TempDir(), "staged")
	for name, data := range map[string]string{"geosite.dat": "old geosite", "Country.mmdb": "old mmdb"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := map[string]string{"mmdb": "https://m.example/" + GeoMMDB.URL()}
	new := map[string]string{"mmdb": GeoMMDB.URL(), "geosite": "https://cdn.example/geosite.dat", "asn": "https://cdn.example/asn.mmdb"}
	changed, err := GeoSourceChanges(home, AllGeoFiles, old, new)
	if err != nil || len(changed) != 1 || changed[0].Name != GeoSite.Name {
		t.Fatalf("changes = %v, %v", changed, err)
	}

	// mihomo -t 没有下载成功的文件不列出。
	if sums, err := StagedGeo(stage, changed); err != nil || len(sums) != 0 {
		t.Fatalf("nothing staged: %v, %v", sums, err)
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, GeoSite.Name), []byte("new geosite"), 0o644); err != nil {
		t.Fatal(err)
	}
	sums, err := StagedGeo(stage, changed)
	if err != nil || len(sums) != 1 {
		t.Fatalf("StagedGeo = %v, %v", sums, err)
	}
	undo, err := InstallStagedGeo(stage, home, sums)
	if err != nil {
		t.Fatal(err)
	}
	// 替换的是已有的文件（文件名不区分大小写），不另建一份。
	if data, _ := os.ReadFile(filepath.Join(home, "geosite.dat")); string(data) != "new geosite" {
		t.Fatalf("not replaced: %q", data)
	}
	if err := undo(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(home, "geosite.dat")); string(data) != "old geosite" {
		t.Fatalf("not put back: %q", data)
	}

	if err := os.WriteFile(filepath.Join(stage, GeoSite.Name), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallStagedGeo(stage, home, sums); !errors.Is(err, ErrStagedGeoChanged) {
		t.Fatalf("changed staging: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(home, "geosite.dat")); string(data) != "old geosite" {
		t.Fatalf("replaced despite the change: %q", data)
	}
}

func TestLinkGeoSkips(t *testing.T) {
	home, dst := t.TempDir(), t.TempDir()
	for _, name := range []string{"GeoSite.dat", "geoip.metadb"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := linkGeo(home, dst, []GeoFile{GeoSite}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "GeoSite.dat")); err == nil {
		t.Error("skipped file linked")
	}
	if data, err := os.ReadFile(filepath.Join(dst, "geoip.metadb")); err != nil || string(data) != "geoip.metadb" {
		t.Errorf("geoip.metadb = %q, %v", data, err)
	}
}
