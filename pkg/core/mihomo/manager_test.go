package mihomo

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// fakeService 模拟 systemd 运行内核：启动时读取 bin/mihomo，内容形如 "mihomo <版本> ..."，
// 在 API socket 上用这个版本响应 /version；内容以 BROKEN 开头时启动失败。
type fakeService struct {
	t      *testing.T
	layout paths.Layout

	mu        sync.Mutex
	installed bool
	state     string
	pid       int
	srv       *http.Server
	calls     []string
}

func newFakeService(t *testing.T, l paths.Layout) *fakeService {
	s := &fakeService{t: t, layout: l, installed: true, state: "inactive"}
	t.Cleanup(func() { s.stop() })
	return s
}

func (s *fakeService) Installed() bool { return s.installed }

func (s *fakeService) Status(context.Context, string) (service.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := service.Status{LoadState: "loaded", ActiveState: s.state}
	if s.state == "active" {
		st.MainPID = s.pid
	}
	return st, nil
}

func (s *fakeService) Start(_ context.Context, unit string) error {
	s.record("start")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == "active" {
		return nil
	}
	return s.start()
}

func (s *fakeService) Stop(_ context.Context, unit string) error {
	s.record("stop")
	s.stop()
	return nil
}

func (s *fakeService) Restart(_ context.Context, unit string) error {
	s.record("restart")
	s.stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.start()
}

func (s *fakeService) ResetFailed(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == "failed" {
		s.state = "inactive"
	}
	return nil
}

func (s *fakeService) record(call string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
}

func (s *fakeService) start() error {
	bin, err := os.ReadFile(s.layout.CoreBinary())
	if err != nil || bytes.HasPrefix(bin, []byte("BROKEN")) {
		s.state = "failed"
		return nil
	}
	version := strings.Fields(string(bin))[1]
	if err := os.MkdirAll(s.layout.CoreDir(), 0o755); err != nil {
		return err
	}
	os.Remove(s.layout.CoreSocket())
	ln, err := net.Listen("unix", s.layout.CoreSocket())
	if err != nil {
		s.t.Skipf("unix sockets unavailable: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"meta": true, "version": version})
	})
	s.srv = &http.Server{Handler: mux}
	go s.srv.Serve(ln)
	s.pid++
	s.state = "active"
	return nil
}

func (s *fakeService) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		s.srv.Close()
		s.srv = nil
	}
	os.Remove(s.layout.CoreSocket())
	if s.state == "active" {
		s.state = "inactive"
	}
}

// fakeReleases 模拟 MetaCubeX/mihomo 的发布：每个版本的 amd64-v3 资产内容为 binaries[版本]。
func fakeReleases(t *testing.T, latest string, binaries map[string]string) *download.Client {
	var srv *httptest.Server
	release := func(tag string) map[string]any {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write([]byte(binaries[tag]))
		zw.Close()
		sum := sha256.Sum256(buf.Bytes())
		name := "mihomo-linux-amd64-v3-" + tag + ".gz"
		return map[string]any{"tag_name": tag, "assets": []download.Asset{{
			Name: name, Size: int64(buf.Len()), Digest: "sha256:" + hex.EncodeToString(sum[:]),
			URL: srv.URL + "/download/" + tag,
		}}}
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/"+kernelRepo+"/releases/latest":
			json.NewEncoder(w).Encode(release(latest))
		case strings.HasPrefix(r.URL.Path, "/repos/"+kernelRepo+"/releases/tags/"):
			tag := strings.TrimPrefix(r.URL.Path, "/repos/"+kernelRepo+"/releases/tags/")
			if _, ok := binaries[tag]; !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(release(tag))
		case strings.HasPrefix(r.URL.Path, "/download/"):
			zw := gzip.NewWriter(w)
			zw.Write([]byte(binaries[strings.TrimPrefix(r.URL.Path, "/download/")]))
			zw.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &download.Client{APIBase: srv.URL}
}

func newTestManager(t *testing.T, d *download.Client) (*Manager, *fakeService) {
	// Unix socket 路径有长度上限，t.TempDir() 在部分系统上过长。
	root, err := os.MkdirTemp("", "tm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	l := paths.Layout{Root: root}
	svc := newFakeService(t, l)
	return &Manager{
		Layout: l, Service: svc, Download: d, Arch: Arch{GOARCH: "amd64", Level: 3},
		StartTimeout: 2 * time.Second, Settle: 10 * time.Millisecond,
		RunsBinary: func(int) (bool, error) { return true, nil },
	}, svc
}

var testBinaries = map[string]string{
	"v1.19.34": "mihomo v1.19.34 build",
	"v1.19.35": "mihomo v1.19.35 build",
	"v1.19.36": "BROKEN v1.19.36 build",
}

func knownGood(t *testing.T, m *Manager) []string {
	t.Helper()
	st, err := state.Load(m.Layout.StateFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var vs []string
	for _, k := range st.Kernels {
		vs = append(vs, k.Version)
	}
	return vs
}

func binary(t *testing.T, m *Manager) string {
	t.Helper()
	data, err := os.ReadFile(m.Layout.CoreBinary())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestManagerUpgradeAndRollback(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, fakeReleases(t, "v1.19.34", testBinaries))

	// 服务没有运行时只替换，健康检查通过后才记为已知可用。
	res, err := m.Upgrade(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.To != "v1.19.34" || !res.Changed || res.Restarted || res.Asset != "mihomo-linux-amd64-v3-v1.19.34.gz" {
		t.Fatalf("install = %+v", res)
	}
	if got := knownGood(t, m); got != nil {
		t.Fatalf("known-good before the first start = %v", got)
	}
	h, err := m.Start(ctx)
	if err != nil || h.Version != "v1.19.34" {
		t.Fatalf("Start = %+v, %v", h, err)
	}
	if got := knownGood(t, m); !slices.Equal(got, []string{"v1.19.34"}) {
		t.Fatalf("known-good = %v", got)
	}

	res, err = m.Upgrade(ctx, "v1.19.35")
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "v1.19.34" || res.To != "v1.19.35" || !res.Changed || !res.Restarted {
		t.Fatalf("upgrade = %+v", res)
	}
	if got := knownGood(t, m); !slices.Equal(got, []string{"v1.19.35", "v1.19.34"}) {
		t.Fatalf("known-good = %v", got)
	}

	if res, err = m.Upgrade(ctx, "v1.19.35"); err != nil || res.Changed {
		t.Fatalf("upgrade to the same version = %+v, %v", res, err)
	}

	res, err = m.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "v1.19.35" || res.To != "v1.19.34" || !res.Restarted {
		t.Fatalf("rollback = %+v", res)
	}
	if got := binary(t, m); got != testBinaries["v1.19.34"] {
		t.Fatalf("binary after rollback = %q", got)
	}
	if got := knownGood(t, m); !slices.Equal(got, []string{"v1.19.34", "v1.19.35"}) {
		t.Fatalf("known-good after rollback = %v", got)
	}
}

func TestManagerUpgradeRevertsUnhealthyKernel(t *testing.T) {
	ctx := context.Background()
	m, svc := newTestManager(t, fakeReleases(t, "v1.19.34", testBinaries))
	if _, err := m.Upgrade(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}

	_, err := m.Upgrade(ctx, "v1.19.36")
	if !errors.Is(err, core.ErrRolledBack) || !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("upgrade to a broken kernel: err = %v", err)
	}
	if got := binary(t, m); got != testBinaries["v1.19.34"] {
		t.Fatalf("binary after revert = %q", got)
	}
	if st, _ := svc.Status(ctx, service.CoreUnit); !st.Active() {
		t.Fatalf("service after revert = %+v", st)
	}
	if got := knownGood(t, m); !slices.Equal(got, []string{"v1.19.34"}) {
		t.Fatalf("known-good = %v", got)
	}
}

func TestManagerRescue(t *testing.T) {
	ctx := context.Background()
	m, svc := newTestManager(t, fakeReleases(t, "v1.19.34", testBinaries))
	if _, err := m.Upgrade(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// 面板升级换上了起不来的二进制，systemd 多次重启后触发救援。
	if err := m.writeBinary([]byte("BROKEN from the panel")); err != nil {
		t.Fatal(err)
	}
	svc.Restart(ctx, service.CoreUnit)
	res, err := m.Rescue(ctx)
	if err != nil || res.To != "v1.19.34" {
		t.Fatalf("Rescue = %+v, %v", res, err)
	}
	if st, _ := svc.Status(ctx, service.CoreUnit); !st.Active() || binary(t, m) != testBinaries["v1.19.34"] {
		t.Fatalf("after rescue: %+v, binary %q", st, binary(t, m))
	}

	// 已知可用的版本也起不来时保持停止。
	svc.stop()
	svc.state = "failed"
	if _, err := m.Rescue(ctx); err == nil || !strings.Contains(err.Error(), "known-good version but keeps failing") {
		t.Fatalf("Rescue of a known-good kernel: err = %v", err)
	}
	if st, _ := svc.Status(ctx, service.CoreUnit); st.Active() {
		t.Fatal("rescue restarted a known-good kernel")
	}
}

func TestManagerAftercare(t *testing.T) {
	ctx := context.Background()
	bins := map[string]string{"v1.19.34": testBinaries["v1.19.34"], "v1.19.35": testBinaries["v1.19.35"]}
	m, svc := newTestManager(t, fakeReleases(t, "v1.19.34", bins))
	if _, err := m.Upgrade(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if res, err := m.Aftercare(ctx); err != nil || res.Action != AftercareKnown {
		t.Fatalf("Aftercare of a known-good kernel = %+v, %v", res, err)
	}

	// 面板升级到 v1.19.35，二进制与官方资产一致。
	if err := m.writeBinary([]byte(bins["v1.19.35"])); err != nil {
		t.Fatal(err)
	}
	svc.Restart(ctx, service.CoreUnit)
	res, err := m.Aftercare(ctx)
	if err != nil || res.Action != AftercareVerified || res.Version != "v1.19.35" {
		t.Fatalf("Aftercare of an official kernel = %+v, %v", res, err)
	}
	if got := knownGood(t, m); !slices.Equal(got, []string{"v1.19.35", "v1.19.34"}) {
		t.Fatalf("known-good = %v", got)
	}

	// 面板经镜像下载到被篡改的二进制：换成校验过的官方构建。
	if err := m.writeBinary([]byte("mihomo v1.19.35 tampered")); err != nil {
		t.Fatal(err)
	}
	svc.Restart(ctx, service.CoreUnit)
	res, err = m.Aftercare(ctx)
	if err != nil || res.Action != AftercareReplaced || res.Version != "v1.19.35" {
		t.Fatalf("Aftercare of a tampered kernel = %+v, %v", res, err)
	}
	if got := binary(t, m); got != bins["v1.19.35"] {
		t.Fatalf("binary after aftercare = %q", got)
	}

	// 查不到运行中版本的官方构建（如 alpha）：换回最近的已知可用版本。面板替换文件后内核还没有重新执行
	// 自己时，等它换成新文件再查版本，不拿旧版本去比。
	if err := m.writeBinary([]byte("mihomo alpha-abc123 build")); err != nil {
		t.Fatal(err)
	}
	svc.Restart(ctx, service.CoreUnit)
	checks := 0
	m.RunsBinary = func(int) (bool, error) { checks++; return checks > 1, nil }
	res, err = m.Aftercare(ctx)
	if err != nil || res.Action != AftercareReplaced || res.Version != "v1.19.35" || checks < 2 {
		t.Fatalf("Aftercare of an alpha kernel = %+v, %v, checks %d", res, err, checks)
	}
}

func TestManagerKeepsRecentKernels(t *testing.T) {
	ctx := context.Background()
	bins := map[string]string{
		"v1.19.33": "mihomo v1.19.33", "v1.19.34": "mihomo v1.19.34", "v1.19.35": "mihomo v1.19.35",
	}
	m, _ := newTestManager(t, fakeReleases(t, "v1.19.33", bins))
	m.Keep = 2
	if _, err := m.Upgrade(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v1.19.34", "v1.19.35"} {
		if _, err := m.Upgrade(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	if got := knownGood(t, m); !slices.Equal(got, []string{"v1.19.35", "v1.19.34"}) {
		t.Fatalf("known-good = %v", got)
	}
	entries, err := os.ReadDir(m.Layout.KernelsDir())
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		dirs = append(dirs, e.Name())
	}
	if !slices.Equal(dirs, []string{"v1.19.34", "v1.19.35"}) {
		t.Fatalf("kernels/ = %v", dirs)
	}
	if _, err := os.Stat(filepath.Join(m.Layout.KernelsDir(), "v1.19.33")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pruned kernel still present: %v", err)
	}
}

func TestFileChecksum(t *testing.T) {
	data := bytes.Repeat([]byte("mihomo"), 100_000)
	path := filepath.Join(t.TempDir(), "mihomo")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if sum, err := FileChecksum(path); err != nil || sum != Checksum(data) {
		t.Fatalf("FileChecksum = %q, %v; want %q", sum, err, Checksum(data))
	}
	if _, err := FileChecksum(path + ".missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestCachedFileChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mihomo")
	write := func(data string) {
		t.Helper()
		// 与替换内核一样，写临时文件再 rename。
		if err := os.WriteFile(path+".tmp", []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want string) {
		t.Helper()
		if sum, err := cachedFileChecksum(path); err != nil || sum != Checksum([]byte(want)) {
			t.Fatalf("cachedFileChecksum = %q, %v; want the sum of %q", sum, err, want)
		}
	}
	write("v1")
	check("v1")
	check("v1")
	// 换成同样大小的另一个文件也要重新计算。
	write("v2")
	check("v2")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := cachedFileChecksum(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
}
