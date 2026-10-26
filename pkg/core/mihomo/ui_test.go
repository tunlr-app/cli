package mihomo

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/download"
)

// makeZip 按顺序打包 files（名称 → 内容；名称以 / 结尾的是目录）。
func makeZip(t *testing.T, files [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f[0])
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(f[1]))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeZashboard 模拟 zashboard 最新发布的 API 和下载，返回指向它的下载客户端。
func fakeZashboard(t *testing.T, archive []byte) *download.Client {
	sum := sha256.Sum256(archive)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + zashboardRepo + "/releases/latest":
			json.NewEncoder(w).Encode(map[string]any{"assets": []download.Asset{{
				Name: zashboardAsset, Size: int64(len(archive)), URL: srv.URL + "/dist.zip",
				Digest: "sha256:" + hex.EncodeToString(sum[:]),
			}}})
		case "/dist.zip":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &download.Client{APIBase: srv.URL}
}

func TestEnsureUI(t *testing.T) {
	archive := makeZip(t, [][2]string{{"dist/", ""}, {"dist/index.html", "<html>"}, {"dist/assets/app.js", "js"}})
	d := fakeZashboard(t, archive)
	dir := filepath.Join(t.TempDir(), "core", "ui", "zashboard")

	installed, err := EnsureUI(context.Background(), d, dir)
	if err != nil || !installed {
		t.Fatalf("EnsureUI = %v, %v", installed, err)
	}
	for name, want := range map[string]string{"index.html": "<html>", "assets/app.js": "js"} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(dir))
	if len(entries) != 1 {
		t.Errorf("leftovers next to the panel: %v", entries)
	}

	// 已存在时不再下载，也不改动。
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("mine"), 0o644)
	if installed, err := EnsureUI(context.Background(), d, dir); err != nil || installed {
		t.Fatalf("second EnsureUI = %v, %v", installed, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "index.html")); string(got) != "mine" {
		t.Error("existing panel replaced")
	}
}

func TestEnsureUIRejectsUnsafeArchives(t *testing.T) {
	for name, files := range map[string][][2]string{
		"parent path":   {{"dist/index.html", "x"}, {"dist/../../evil", "x"}},
		"absolute path": {{"/etc/evil", "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			d := fakeZashboard(t, makeZip(t, files))
			dir := filepath.Join(t.TempDir(), "ui", "zashboard")
			_, err := EnsureUI(context.Background(), d, dir)
			if err == nil || !strings.Contains(err.Error(), "points outside the target directory") {
				t.Fatalf("err = %v", err)
			}
			if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) != 0 {
				t.Errorf("left files behind: %v", entries)
			}
		})
	}
}

func TestEnsureUIDownloadFailure(t *testing.T) {
	d := &download.Client{APIBase: httptest.NewServer(http.NotFoundHandler()).URL}
	dir := filepath.Join(t.TempDir(), "ui", "zashboard")
	if installed, err := EnsureUI(context.Background(), d, dir); installed || err == nil || !strings.Contains(err.Error(), "install the panel") {
		t.Fatalf("EnsureUI = %v, %v", installed, err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("panel directory created after a failed download")
	}
}

func TestSingleRoot(t *testing.T) {
	files := func(names ...string) []*zip.File {
		var fs []*zip.File
		for _, n := range names {
			fs = append(fs, &zip.File{FileHeader: zip.FileHeader{Name: n}})
		}
		return fs
	}
	for _, tt := range []struct {
		names []string
		want  string
	}{
		{[]string{"dist/", "dist/index.html"}, "dist/"},
		{[]string{"dist/index.html", "other/x"}, ""},
		{[]string{"index.html", "assets/a.js"}, ""},
		{nil, ""},
	} {
		if got := singleRoot(files(tt.names...)); got != tt.want {
			t.Errorf("singleRoot(%v) = %q, want %q", tt.names, got, tt.want)
		}
	}
}
