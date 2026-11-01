package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/service"
)

// toolArchive 打包一个假的 tunlr 二进制，和 goreleaser 的 tar.gz 一样带 LICENSE。
func toolArchive(t *testing.T, bin string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"LICENSE": "MIT", "tunlr": bin} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeReleases 模拟 GitHub 的 release API：tunlr 的 tags 各有一个本平台的 tar.gz，mihomo 只有最新版本号。
// tool 为空时 tunlr 的查询返回 404。
func fakeReleases(t *testing.T, tool map[string][]byte, latestTool, latestKernel string) *download.Client {
	t.Helper()
	var srv *httptest.Server
	release := func(tag string, assets map[string][]byte) map[string]any {
		var list []map[string]any
		for name, data := range assets {
			sum := sha256.Sum256(data)
			list = append(list, map[string]any{
				"name": name, "size": len(data), "digest": "sha256:" + hex.EncodeToString(sum[:]),
				"browser_download_url": srv.URL + "/download/" + tag + "/" + name,
			})
		}
		return map[string]any{"tag_name": tag, "assets": list}
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		toolTags := "/repos/" + ToolRepo + "/releases/tags/"
		switch {
		case r.URL.Path == "/repos/"+ToolRepo+"/releases/latest" && latestTool != "":
			json.NewEncoder(w).Encode(release(latestTool, map[string][]byte{ToolAsset(latestTool, runtime.GOARCH): tool[latestTool]}))
		case strings.HasPrefix(r.URL.Path, toolTags):
			tag := strings.TrimPrefix(r.URL.Path, toolTags)
			if _, ok := tool[tag]; !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(release(tag, map[string][]byte{ToolAsset(tag, runtime.GOARCH): tool[tag]}))
		case r.URL.Path == "/repos/MetaCubeX/mihomo/releases/latest" && latestKernel != "":
			json.NewEncoder(w).Encode(release(latestKernel, nil))
		case strings.HasPrefix(r.URL.Path, "/download/"):
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/download/"), "/", 2)
			w.Write(tool[parts[0]])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &download.Client{APIBase: srv.URL}
}

// fakeTool 记录对下载好的二进制的调用，--version 报告 version。
type fakeTool struct {
	version string
	calls   []string
	initErr error
}

func (f *fakeTool) run(_ context.Context, path string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, filepath.Base(path)+" "+strings.Join(args, " "))
	if args[0] == "init" {
		return []byte("Checking the environment\n"), f.initErr
	}
	data, _ := os.ReadFile(path)
	return []byte(`{"version":1,"data":{"version":"` + f.version + `","commit":"` + string(data) + `"}}` + "\n"), nil
}

func TestUpgrade(t *testing.T) {
	ctx := context.Background()
	i, _, _ := newTunInstaller(t)
	i.Version = "0.1.0"
	archives := map[string][]byte{"v0.2.0": toolArchive(t, "bin-0.2.0"), "v0.1.0": toolArchive(t, "bin-0.1.0")}
	i.Download = fakeReleases(t, archives, "v0.2.0", "v1.19.34")
	tool := &fakeTool{version: "0.2.0"}
	i.RunTool = tool.run
	if err := os.MkdirAll(i.Layout.UnitDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(i.Layout.UnitDir(), service.CoreUnit), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if target, err := i.UpgradeTarget(ctx, ""); err != nil || target != "0.2.0" {
		t.Fatalf("target = %q, %v", target, err)
	}
	// 查到的最新版本记入状态文件，菜单的提示随之更新；查询时间不变。
	if uc := tunSwitch(t, i).UpdateCheck; uc == nil || uc.Tool != "0.2.0" || !uc.At.IsZero() {
		t.Errorf("latest version not recorded: %+v", uc)
	}
	res, err := i.Upgrade(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "0.1.0" || res.To != "0.2.0" || !res.Changed || !res.Reinstalled || res.Asset != ToolAsset("v0.2.0", runtime.GOARCH) || res.Path != i.Layout.ToolBinary() {
		t.Errorf("result = %+v", res)
	}
	if data, err := os.ReadFile(i.Layout.ToolBinary()); err != nil || string(data) != "bin-0.2.0" {
		t.Errorf("installed binary = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(i.Layout.ToolBinary()), ".tunlr-new")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temporary file left behind: %v", err)
	}
	// 先用临时文件确认版本，替换后由新版本执行 init。
	if want := []string{".tunlr-new --version --json", "tunlr init --yes"}; strings.Join(tool.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q", tool.calls)
	}

	// 已经是目标版本时什么都不做。
	i.Version = "0.2.0"
	tool.calls = nil
	if res, err := i.Upgrade(ctx, "v0.2.0"); err != nil || res.Changed || res.To != "0.2.0" || len(tool.calls) != 0 {
		t.Errorf("same version: %+v, %v, calls %q", res, err, tool.calls)
	}

	// 下载的二进制报告的版本不对时不替换。
	tool.version = "0.3.0"
	if _, err := i.Upgrade(ctx, "v0.1.0"); err == nil || !strings.Contains(err.Error(), `reports version "0.3.0", expected 0.1.0`) {
		t.Errorf("wrong version: %v", err)
	}
	if data, _ := os.ReadFile(i.Layout.ToolBinary()); string(data) != "bin-0.2.0" {
		t.Errorf("binary replaced by an unverified one: %q", data)
	}
	// 用 --version 指明的不是最新版本，不记。
	if uc := tunSwitch(t, i).UpdateCheck; uc.Tool != "0.2.0" {
		t.Errorf("a version given with --version recorded: %+v", uc)
	}

	// init 失败时二进制已经换好，错误说明怎么补救。
	tool.version, tool.initErr = "0.1.0", errors.New("exit status 1")
	res, err = i.Upgrade(ctx, "v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "its init failed (run it again: tunlr init)") || !strings.Contains(err.Error(), "Checking the environment") {
		t.Errorf("init failed: %v", err)
	}
	if res == nil || !res.Changed || res.Reinstalled {
		t.Errorf("result after failed init = %+v", res)
	}

	// 没有这个版本。
	if _, err := i.Upgrade(ctx, "v9.9.9"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("missing release: %v", err)
	}
}

func TestUpgradeDevBuildNeedsVersion(t *testing.T) {
	ctx := context.Background()
	i, _, _ := newTunInstaller(t)
	i.Version = DevVersion
	i.Download = fakeReleases(t, map[string][]byte{"v0.2.0": toolArchive(t, "bin")}, "v0.2.0", "")
	tool := &fakeTool{version: "0.2.0"}
	i.RunTool = tool.run
	if _, err := i.UpgradeTarget(ctx, ""); !errors.Is(err, core.ErrInvalidArgument) || !strings.Contains(err.Error(), "development build") {
		t.Errorf("target: %v", err)
	}
	if _, err := i.Upgrade(ctx, ""); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("upgrade: %v", err)
	}
	// 指明版本就可以装；服务没有安装时不执行 init。
	res, err := i.Upgrade(ctx, "v0.2.0")
	if err != nil || !res.Changed || res.Reinstalled {
		t.Fatalf("with --version: %+v, %v", res, err)
	}
	if len(tool.calls) != 1 || !strings.HasSuffix(tool.calls[0], "--version --json") {
		t.Errorf("calls = %q", tool.calls)
	}
}

func TestNormalizeToolVersion(t *testing.T) {
	for in, want := range map[string]string{"0.2.0": "v0.2.0", "v0.2.0": "v0.2.0", "0.2": "", "latest": "", "v1.2.3-rc1": ""} {
		got, err := NormalizeToolVersion(in)
		if (err == nil) != (want != "") || got != want {
			t.Errorf("NormalizeToolVersion(%q) = %q, %v", in, got, err)
		}
		if err != nil && !errors.Is(err, core.ErrInvalidArgument) {
			t.Errorf("%q: not a usage error: %v", in, err)
		}
	}
	if got := ToolAsset("v0.2.0", "arm64"); got != "tunlr_0.2.0_linux_arm64.tar.gz" {
		t.Errorf("asset = %q", got)
	}
}

func TestNewerVersion(t *testing.T) {
	tests := []struct {
		latest, current string
		want            bool
	}{
		{"0.2.0", "0.1.0", true},
		{"0.2.0", "v0.1.9", true},
		{"v1.19.34", "v1.19.33", true},
		{"1.0.0", "0.99.99", true},
		{"0.1.0", "0.1.0", false},
		{"0.1.0", "0.2.0", false},
		{"0.2.0", "dev", false},
		{"0.2.0", "v0.0.0-test", false},
		{"", "0.1.0", false},
		{"0.2.0", "", false},
		{"0.2.0", "alpha-g1234567", false},
		{"01.2.0", "0.1.0", false},
	}
	for _, tt := range tests {
		if got := newerVersion(tt.latest, tt.current); got != tt.want {
			t.Errorf("newerVersion(%q, %q) = %v", tt.latest, tt.current, got)
		}
	}
}

func TestCheckUpdates(t *testing.T) {
	ctx := context.Background()
	i, _, _ := newTunInstaller(t)
	i.Version = "0.1.0"
	now := testNow
	i.Now = func() time.Time { return now }
	i.Download = fakeReleases(t, nil, "v0.2.0", "v1.19.34")
	// 还没有状态文件（没有安装过）时不查。
	fresh := *i
	fresh.Layout.Root = t.TempDir()
	if uc, checked, err := fresh.CheckUpdates(ctx); err != nil || checked || uc != nil {
		t.Fatalf("without state: %+v, %v, %v", uc, checked, err)
	}

	uc, checked, err := i.CheckUpdates(ctx)
	if err != nil || !checked || uc.Tool != "0.2.0" || uc.Kernel != "v1.19.34" || !uc.At.Equal(now) {
		t.Fatalf("first check: %+v, %v, %v", uc, checked, err)
	}
	if st := tunSwitch(t, i); st.UpdateCheck == nil || st.UpdateCheck.Tool != "0.2.0" {
		t.Errorf("not saved: %+v", st.UpdateCheck)
	}

	// 一天之内不再查。
	i.Download = fakeReleases(t, nil, "v0.3.0", "v1.19.35")
	now = now.Add(23 * time.Hour)
	if uc, checked, err := i.CheckUpdates(ctx); err != nil || checked || uc.Tool != "0.2.0" {
		t.Errorf("within a day: %+v, %v, %v", uc, checked, err)
	}
	// 过了一天再查；一个查询失败时保留它上次的结果，另一个照常更新，错误说明失败的那个。
	now = now.Add(2 * time.Hour)
	i.Download = fakeReleases(t, nil, "", "v1.19.35")
	uc, checked, err = i.CheckUpdates(ctx)
	if err == nil || !checked || uc.Tool != "0.2.0" || uc.Kernel != "v1.19.35" || !uc.At.Equal(now) {
		t.Errorf("partial failure: %+v, %v, %v", uc, checked, err)
	}
	// 都失败时不写状态。
	now = now.Add(25 * time.Hour)
	i.Download = fakeReleases(t, nil, "", "")
	if _, checked, err := i.CheckUpdates(ctx); err == nil || !checked {
		t.Errorf("both failed: %v, %v", checked, err)
	}
	if st := tunSwitch(t, i); !st.UpdateCheck.At.Equal(now.Add(-25 * time.Hour)) {
		t.Errorf("state written after a failed check: %+v", st.UpdateCheck)
	}

	// status 据此算出可用的更新：tunlr 比 0.1.0 新；内核与运行中的（没有运行时与已知可用的当前）版本比较。
	s, err := i.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.UpdateCheck == nil || s.UpdateCheck.Tool != "0.2.0" || s.UpdateCheck.ToolLatest != "0.2.0" || s.UpdateCheck.Kernel != "" {
		t.Errorf("status = %+v", s.UpdateCheck)
	}
	cs := &CoreStatus{Running: "v1.19.33"}
	if c := updateCheck(tunSwitch(t, i).UpdateCheck, "0.2.0", cs); c.Tool != "" || c.Kernel != "v1.19.35" {
		t.Errorf("running v1.19.33 on 0.2.0: %+v", c)
	}
	cs = &CoreStatus{Running: "v1.19.35"}
	if c := updateCheck(tunSwitch(t, i).UpdateCheck, DevVersion, cs); c.Tool != "" || c.Kernel != "" {
		t.Errorf("up to date: %+v", c)
	}
}
