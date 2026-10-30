package mihomo

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeKernel 在 Unix socket 上模拟内核 API：/version 返回版本；PUT /configs 读取 path 指向的文件，
// 内容含 NOPE 时按内核的格式返回 400，含 CRASH 时关闭 API（模拟内核退出）。
type fakeKernel struct {
	socket  string
	ln      net.Listener
	srv     *http.Server
	loaded  []string
	queries []string
}

func newFakeKernel(t *testing.T) *fakeKernel {
	t.Helper()
	// Unix socket 路径有长度上限（约 108 字节），t.TempDir() 在部分系统上过长。
	dir, err := os.MkdirTemp("", "tk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	k := &fakeKernel{socket: filepath.Join(dir, "api.sock")}
	k.ln, err = net.Listen("unix", k.socket)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"meta":true,"version":"v1.19.33"}`))
	})
	mux.HandleFunc("GET /configs", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"mode":"rule","tun":{"enable":true,"device":"tunlr0","auto-detect-interface":true,"exclude-uid":[999]}}`))
	})
	mux.HandleFunc("PUT /configs", func(w http.ResponseWriter, r *http.Request) {
		k.queries = append(k.queries, r.URL.RawQuery)
		var req struct{ Path string }
		json.NewDecoder(r.Body).Decode(&req)
		if !filepath.IsAbs(req.Path) {
			http.Error(w, `{"message":"path is not a absolute path"}`, http.StatusBadRequest)
			return
		}
		data, _ := os.ReadFile(req.Path)
		switch {
		case strings.Contains(string(data), "NOPE"):
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"message":"rules[0] [MATCH,NOPE] error: proxy [NOPE] not found"}`))
			return
		case strings.Contains(string(data), "CRASH"):
			go k.srv.Close()
		}
		k.loaded = append(k.loaded, string(data))
		w.WriteHeader(http.StatusNoContent)
	})
	k.srv = &http.Server{Handler: mux}
	go k.srv.Serve(k.ln)
	t.Cleanup(func() { k.srv.Close() })
	return k
}

func TestClient(t *testing.T) {
	k := newFakeKernel(t)
	c := NewClient(k.socket)
	ctx := context.Background()

	v, err := c.Version(ctx)
	if err != nil || v != "v1.19.33" {
		t.Fatalf("version = %q, %v", v, err)
	}
	if tun, err := c.Tun(ctx); err != nil || !tun.Enable || !tun.AutoDetectInterface || len(tun.ExcludeUID) != 1 || tun.ExcludeUID[0] != 999 {
		t.Fatalf("tun = %+v, %v", tun, err)
	}

	cfg := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(cfg, []byte("mode: rule\n"), 0o600)
	if err := c.ReloadConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if len(k.loaded) != 1 || k.queries[0] != "force=true" {
		t.Errorf("loaded %q with query %q", k.loaded, k.queries)
	}

	os.WriteFile(cfg, []byte("rules: [MATCH,NOPE]\n"), 0o600)
	err = c.ReloadConfig(ctx, cfg)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 400 || ae.Message != "rules[0] [MATCH,NOPE] error: proxy [NOPE] not found" {
		t.Fatalf("bad config: err = %v", err)
	}
	if !strings.HasPrefix(err.Error(), "mihomo API: rules[0]") {
		t.Errorf("message = %q", err.Error())
	}
}

func TestClientNotRunning(t *testing.T) {
	// socket 不存在。
	c := NewClient(filepath.Join(t.TempDir(), "api.sock"))
	if _, err := c.Version(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Errorf("missing socket: err = %v", err)
	}

	// socket 文件还在，但内核已经退出。Windows 的拒绝连接错误码不同，工具也不支持 Windows。
	if runtime.GOOS == "windows" {
		t.Skip("stale socket detection is Linux-only")
	}
	k := newFakeKernel(t)
	// 直接关闭监听并保留 socket 文件，模拟内核退出后留下的 socket。不能用 k.srv.Close()：
	// Serve 可能还没开始，监听会在短时间内仍可连接，结果取决于时序。
	k.ln.(*net.UnixListener).SetUnlinkOnClose(false)
	k.ln.Close()
	if _, err := os.Stat(k.socket); err != nil {
		t.Fatalf("socket file: %v", err)
	}
	if _, err := NewClient(k.socket).Version(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stale socket: err = %v", err)
	}
}

func TestReloaderDetectsCrash(t *testing.T) {
	k := newFakeKernel(t)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(cfg, []byte("CRASH: true\n"), 0o600)
	r := &Reloader{Client: NewClient(k.socket), ConfigPath: cfg}
	err := r.Reload(context.Background())
	if err == nil || errors.Is(err, ErrNotRunning) || !strings.Contains(err.Error(), "stopped responding") {
		t.Fatalf("err = %v, want a reload failure that is not ErrNotRunning", err)
	}
}
