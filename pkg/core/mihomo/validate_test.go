package mihomo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/download"
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
// 有 GEOIP 规则而工作目录中没有 MMDB 时报下载失败；含 HANG 时挂起。
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
		if p, _ := findGeo(args[2], GeoMMDB); p == "" {
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

	// 没有 MMDB 且不下载：报告为数据缺失，而不是配置错误。
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

// TestValidateDownloadsGeo 验证缺失的 geo 文件会先下载到工作目录，再交给校验。
func TestValidateDownloadsGeo(t *testing.T) {
	content := []byte("fake mmdb")
	sum := sha256.Sum256(content)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + geoRepo + "/releases/tags/" + geoTag:
			json.NewEncoder(w).Encode(map[string]any{"assets": []download.Asset{{
				Name: GeoMMDB.Asset, Size: int64(len(content)),
				Digest: "sha256:" + hex.EncodeToString(sum[:]), URL: srv.URL + "/dl/" + GeoMMDB.Asset,
			}}})
		case "/dl/" + GeoMMDB.Asset:
			w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	v := fakeValidator(t)
	v.Download = &download.Client{APIBase: srv.URL}
	// 调用方给出 geo 需求时以它为准，不再解析配置。
	if err := v.Validate(context.Background(), []byte("rules:\n  - MATCH,DIRECT\n"), &config.GeoNeeds{GeoIP: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(v.HomeDir, "geoip.metadb")); err != nil {
		t.Fatalf("geo given by the caller not downloaded: %v", err)
	}
	if err := os.Remove(filepath.Join(v.HomeDir, "geoip.metadb")); err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(context.Background(), []byte("rules:\n  - GEOIP,CN,DIRECT\n"), nil); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(v.HomeDir, "geoip.metadb")); err != nil || string(got) != "fake mmdb" {
		t.Fatalf("geo not downloaded into home: %q, %v", got, err)
	}
	// 已有的文件不重复下载。
	srv.Close()
	if err := v.Validate(context.Background(), []byte("rules:\n  - GEOIP,CN,DIRECT\n"), nil); err != nil {
		t.Fatalf("second validate should not download: %v", err)
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
