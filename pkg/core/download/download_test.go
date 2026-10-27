package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeGitHub 同时模拟 API、github.com 下载和一个镜像：
// /repos/o/r/releases/tags/v1 和 /repos/o/r/releases/latest 返回资产列表，/dl/<name> 返回内容，
// /mirror/<完整地址> 是镜像，tampered 置位时镜像返回被篡改的内容。
type fakeGitHub struct {
	*httptest.Server
	files      map[string][]byte
	noDigest   bool
	tampered   atomic.Bool
	mirrorHits atomic.Int32
	directHits atomic.Int32
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{files: map[string][]byte{"geoip.metadb": []byte("mmdb-content")}}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/releases/tags/v1", r.URL.Path == "/repos/o/r/releases/latest":
			var assets []Asset
			for name, data := range g.files {
				sum := sha256.Sum256(data)
				a := Asset{Name: name, Size: int64(len(data)), URL: g.URL + "/dl/" + name}
				if !g.noDigest {
					a.Digest = "sha256:" + hex.EncodeToString(sum[:])
				}
				assets = append(assets, a)
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1", "assets": assets})
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			g.directHits.Add(1)
			w.Write(g.files[strings.TrimPrefix(r.URL.Path, "/dl/")])
		case strings.HasPrefix(r.URL.Path, "/mirror/"):
			g.mirrorHits.Add(1)
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			data := g.files[name]
			if g.tampered.Load() {
				data = []byte(strings.Repeat("x", len(data)))
			}
			w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func TestFetchAsset(t *testing.T) {
	g := newFakeGitHub(t)
	dst := filepath.Join(t.TempDir(), "geoip.metadb")
	ctx := context.Background()

	direct := &Client{APIBase: g.URL}
	if err := direct.FetchAsset(ctx, "o/r", "v1", "geoip.metadb", dst, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "mmdb-content" {
		t.Errorf("content = %q", got)
	}
	if g.directHits.Load() != 1 || g.mirrorHits.Load() != 0 {
		t.Errorf("hits: direct=%d mirror=%d", g.directHits.Load(), g.mirrorHits.Load())
	}

	// 镜像前缀不带末尾斜杠也可以。
	mirrored := &Client{APIBase: g.URL, Mirror: g.URL + "/mirror"}
	os.Remove(dst)
	if err := mirrored.FetchAsset(ctx, "o/r", "v1", "geoip.metadb", dst, 0o644); err != nil {
		t.Fatal(err)
	}
	if g.mirrorHits.Load() != 1 || g.directHits.Load() != 1 {
		t.Errorf("hits: direct=%d mirror=%d", g.directHits.Load(), g.mirrorHits.Load())
	}
}

func TestFetchProgress(t *testing.T) {
	g := newFakeGitHub(t)
	var last [2]int64
	calls := 0
	ctx := WithProgress(context.Background(), func(name string, done, total int64) {
		if name != "geoip.metadb" {
			t.Errorf("name = %q", name)
		}
		calls++
		last = [2]int64{done, total}
	})
	if _, err := (&Client{APIBase: g.URL}).Fetch(ctx, "o/r", "v1", "geoip.metadb"); err != nil {
		t.Fatal(err)
	}
	size := int64(len("mmdb-content"))
	if calls < 2 || last != [2]int64{size, size} {
		t.Errorf("calls = %d, last = %v", calls, last)
	}
}

func TestFetchAssetRejectsTampered(t *testing.T) {
	g := newFakeGitHub(t)
	g.tampered.Store(true)
	dst := filepath.Join(t.TempDir(), "geoip.metadb")
	os.WriteFile(dst, []byte("old"), 0o644)

	c := &Client{APIBase: g.URL, Mirror: g.URL + "/mirror/"}
	err := c.FetchAsset(context.Background(), "o/r", "v1", "geoip.metadb", dst, 0o644)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "old" {
		t.Errorf("existing file replaced by tampered download: %q", got)
	}
}

func TestFetchAssetErrors(t *testing.T) {
	g := newFakeGitHub(t)
	c := &Client{APIBase: g.URL}
	dst := filepath.Join(t.TempDir(), "x")
	ctx := context.Background()

	if err := c.FetchAsset(ctx, "o/r", "v1", "missing.dat", dst, 0o644); err == nil || !strings.Contains(err.Error(), "has no asset missing.dat") {
		t.Errorf("missing asset: err = %v", err)
	}
	if err := c.FetchAsset(ctx, "o/r", "v2", "geoip.metadb", dst, 0o644); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("missing release: err = %v", err)
	}
	g.noDigest = true
	if err := c.FetchAsset(ctx, "o/r", "v1", "geoip.metadb", dst, 0o644); err == nil || !strings.Contains(err.Error(), "refusing to download unverified") {
		t.Errorf("no digest: err = %v", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("failed downloads wrote %s", dst)
	}
}

func TestFetchLatest(t *testing.T) {
	g := newFakeGitHub(t)
	c := &Client{APIBase: g.URL, Mirror: g.URL + "/mirror/"}
	data, err := c.Fetch(context.Background(), "o/r", "", "geoip.metadb")
	if err != nil || string(data) != "mmdb-content" {
		t.Fatalf("Fetch latest = %q, %v", data, err)
	}
	if _, err := c.Fetch(context.Background(), "o/x", "", "geoip.metadb"); err == nil || !strings.Contains(err.Error(), "o/x latest release: HTTP 404") {
		t.Errorf("missing repo: err = %v", err)
	}
	rel, err := c.Release(context.Background(), "o/r", "")
	if err != nil || rel.Tag != "v1" {
		t.Fatalf("Release = %+v, %v", rel, err)
	}
	a, err := rel.Asset("geoip.metadb")
	if err != nil {
		t.Fatal(err)
	}
	if sum, err := a.SHA256(); err != nil || len(sum) != 64 {
		t.Errorf("SHA256 = %q, %v", sum, err)
	}
}
