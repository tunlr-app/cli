package download

import (
	"bytes"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	// apiStatus 不为 0 时直连 API 返回这个状态码；apiMirrorHits 计经镜像的 API 查询。
	apiStatus     atomic.Int32
	apiMirrorHits atomic.Int32
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{files: map[string][]byte{"geoip.metadb": []byte("mmdb-content")}}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 经镜像的 API 查询：/mirror/<API 地址>。
		apiViaMirror := strings.HasPrefix(r.URL.Path, "/mirror/") && strings.Contains(r.URL.Path, "/repos/o/r/releases/")
		if apiViaMirror {
			g.apiMirrorHits.Add(1)
			r.URL.Path = r.URL.Path[strings.Index(r.URL.Path, "/repos/"):]
		}
		switch {
		case r.URL.Path == "/repos/o/r/releases/tags/v1", r.URL.Path == "/repos/o/r/releases/latest":
			if code := g.apiStatus.Load(); code != 0 && !apiViaMirror {
				if code == http.StatusForbidden {
					// GitHub 限流时的响应。
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.Header().Set("X-RateLimit-Reset", "1793384688")
					w.WriteHeader(int(code))
					fmt.Fprint(w, `{"message":"API rate limit exceeded for 192.0.2.1."}`)
					return
				}
				w.WriteHeader(int(code))
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
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

// flakyServer 在 /f 上提供 data，按 behave 决定每次请求的表现。behave 收到第几次请求（从 0 起）和
// Range 的起点（没有 Range 时为 -1），返回 "cut"（发出一半后断开）、"hang"（发出一半后不再发送）、
// "ignore"（忽略 Range 发全量）、"full"（正常，支持 Range）或 "status:<码>"。
func flakyServer(t *testing.T, data []byte, behave func(n int, start int64) string) (*httptest.Server, *[]int64) {
	var mu sync.Mutex
	var starts []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := int64(-1)
		if v, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes="); ok {
			start, _ = strconv.ParseInt(strings.TrimSuffix(v, "-"), 10, 64)
		}
		mu.Lock()
		n := len(starts)
		starts = append(starts, start)
		mu.Unlock()
		mode := behave(n, start)
		if code, ok := strings.CutPrefix(mode, "status:"); ok {
			c, _ := strconv.Atoi(code)
			w.WriteHeader(c)
			return
		}
		body := data
		if start >= 0 && mode != "ignore" {
			body = data[start:]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}
		switch mode {
		case "cut", "hang":
			w.Write(body[:len(body)/2])
			w.(http.Flusher).Flush()
			if mode == "hang" {
				<-r.Context().Done()
				return
			}
			// 不发完就断开：客户端读到 unexpected EOF。
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		default:
			w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &starts
}

func testData() []byte {
	data := make([]byte, 200<<10)
	for i := range data {
		data[i] = byte(i * 7)
	}
	return data
}

func fastClient() *Client {
	return &Client{StallTimeout: 300 * time.Millisecond, RetryDelay: time.Millisecond}
}

func TestGetResumes(t *testing.T) {
	data := testData()
	srv, starts := flakyServer(t, data, func(n int, start int64) string {
		switch n {
		case 0:
			return "cut"
		case 1:
			return "hang"
		case 2:
			return "status:503"
		}
		return "full"
	})
	var last int64
	ctx := WithProgress(context.Background(), func(_ string, done, total int64) {
		if done < last && done != 0 {
			t.Errorf("progress went back from %d to %d", last, done)
		}
		last = done
	})
	got, err := fastClient().get(ctx, "f", srv.URL+"/f", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("resumed content differs")
	}
	// 第一轮从头；断开后从一半处续传；那一轮又发了剩余部分的一半；503 不改变位置。
	half := int64(len(data) / 2)
	want := []int64{-1, half, half + (int64(len(data))-half)/2, half + (int64(len(data))-half)/2}
	if !slices.Equal(*starts, want) {
		t.Errorf("Range starts = %v, want %v", *starts, want)
	}
	if last != int64(len(data)) {
		t.Errorf("last progress = %d", last)
	}
}

func TestGetRestartsWhenRangeIgnored(t *testing.T) {
	data := testData()
	srv, starts := flakyServer(t, data, func(n int, start int64) string {
		if n == 0 {
			return "cut"
		}
		return "ignore"
	})
	got, err := fastClient().get(context.Background(), "f", srv.URL+"/f", int64(len(data)))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("get = %d bytes, %v", len(got), err)
	}
	if len(*starts) != 2 {
		t.Errorf("requests = %v", *starts)
	}
}

func TestGetGivesUp(t *testing.T) {
	data := testData()
	// 404 不重试。
	srv, starts := flakyServer(t, data, func(int, int64) string { return "status:404" })
	if _, err := fastClient().get(context.Background(), "f", srv.URL+"/f", int64(len(data))); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("404: err = %v", err)
	}
	if len(*starts) != 1 {
		t.Errorf("404 retried: %v", *starts)
	}
	// 一直 502：连续 maxStalledAttempts 轮没有进展后放弃。
	srv, starts = flakyServer(t, data, func(int, int64) string { return "status:502" })
	if _, err := fastClient().get(context.Background(), "f", srv.URL+"/f", int64(len(data))); err == nil || !strings.Contains(err.Error(), "gave up") {
		t.Errorf("502: err = %v", err)
	}
	if len(*starts) != maxStalledAttempts {
		t.Errorf("502 attempts = %d", len(*starts))
	}
	// 每轮都有进展的断开不计入放弃次数。
	srv, _ = flakyServer(t, data, func(n int, _ int64) string {
		if n < maxStalledAttempts+2 {
			return "cut"
		}
		return "full"
	})
	if got, err := fastClient().get(context.Background(), "f", srv.URL+"/f", int64(len(data))); err != nil || !bytes.Equal(got, data) {
		t.Errorf("progressing cuts: %d bytes, %v", len(got), err)
	}
	// 调用方取消时立即返回。
	srv, _ = flakyServer(t, data, func(int, int64) string { return "hang" })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c := &Client{StallTimeout: time.Minute}
	if _, err := c.get(ctx, "f", srv.URL+"/f", int64(len(data))); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("canceled: err = %v", err)
	}
}

// TestReleaseErrors 验证 API 只直连、不经镜像，错误里带 GitHub 的说明，限流时说明限额和解除时间。
func TestReleaseErrors(t *testing.T) {
	g := newFakeGitHub(t)
	ctx := context.Background()
	c := &Client{APIBase: g.URL, Mirror: g.URL + "/mirror/"}

	g.apiStatus.Store(http.StatusNotFound)
	if _, err := c.Release(ctx, "o/r", "v1"); err == nil || !strings.Contains(err.Error(), "HTTP 404 Not Found: Not Found") {
		t.Errorf("404: err = %v", err)
	}
	g.apiStatus.Store(http.StatusForbidden)
	_, err := c.Release(ctx, "o/r", "v1")
	if err == nil || !strings.Contains(err.Error(), "HTTP 403 Forbidden: GitHub API rate limit exceeded for this IP (60 unauthenticated requests per hour); it resets at ") {
		t.Errorf("rate limited: err = %v", err)
	}
	if g.apiMirrorHits.Load() != 0 {
		t.Errorf("the API was queried through the mirror %d times", g.apiMirrorHits.Load())
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
