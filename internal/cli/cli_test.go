package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/tunlr-app/cli/pkg/core/paths"
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"tunlr": func() {
			os.Exit(Main(BuildInfo{Version: "v0.0.0-test", Commit: "abc1234", Date: "2026-01-01"}, os.Args[1:]))
		},
		"mihomo": func() { os.Exit(fakeMihomo(os.Args[1:])) },
	})
}

func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:   "testdata/script",
		Setup: setupScript,
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"exitcode":       cmdExitCode,
			"install-mihomo": cmdInstallMihomo,
			"kernel":         cmdKernel,
		},
	})
}

// fakeMihomo 扮演 mihomo，只实现 -t -d <dir> -f <file>：配置含 MATCH,NOPE 时报告引用了不存在的策略组。
func fakeMihomo(args []string) int {
	if len(args) != 5 || args[0] != "-t" {
		fmt.Println("unexpected args", args)
		return 2
	}
	cfg, err := os.ReadFile(args[4])
	if err != nil {
		fmt.Println(err)
		return 2
	}
	if strings.Contains(string(cfg), "MATCH,NOPE") {
		fmt.Println(`time="2026-10-26T17:00:00+08:00" level=error msg="rules[0] [MATCH,NOPE] error: proxy [NOPE] not found"`)
		return 1
	}
	fmt.Println("configuration file " + args[4] + " test is successful")
	return 0
}

// cmdKernel 实现 kernel start|stop：在布局中的 API socket 上模拟运行中的内核。
// PUT /configs 加载 path 指向的配置，内容含 REJECT 时按内核的格式拒绝；
// 每次成功加载的内容写入 $WORK/kernel-loaded，供脚本检查。
func cmdKernel(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: kernel start|stop")
	}
	switch args[0] {
	case "start":
		socket := paths.Layout{Root: ts.Getenv(rootEnv)}.CoreSocket()
		ts.Check(os.MkdirAll(filepath.Dir(socket), 0o750))
		ln, err := net.Listen("unix", socket)
		ts.Check(err)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"meta":true,"version":"v1.19.33"}`))
		})
		mux.HandleFunc("PUT /configs", func(w http.ResponseWriter, r *http.Request) {
			var req struct{ Path string }
			json.NewDecoder(r.Body).Decode(&req)
			data, err := os.ReadFile(req.Path)
			if err != nil || strings.Contains(string(data), "REJECT") {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"message":"listener REJECT failed"}`))
				return
			}
			os.WriteFile(ts.MkAbs("kernel-loaded"), data, 0o644)
			w.WriteHeader(http.StatusNoContent)
		})
		srv := &http.Server{Handler: mux}
		go srv.Serve(ln)
		kernelsMu.Lock()
		kernels[ts] = srv
		kernelsMu.Unlock()
		ts.Defer(func() { srv.Close() })
	case "stop":
		kernelsMu.Lock()
		srv := kernels[ts]
		delete(kernels, ts)
		kernelsMu.Unlock()
		if srv == nil {
			ts.Fatalf("kernel not started")
		}
		srv.Close()
	default:
		ts.Fatalf("usage: kernel start|stop")
	}
}

// kernels 记录各脚本启动的模拟内核。脚本并行运行，访问需加锁。
var (
	kernels   = map[*testscript.TestScript]*http.Server{}
	kernelsMu sync.Mutex
)

// cmdInstallMihomo 把测试二进制复制到布局中的内核位置，它以 mihomo 的名字运行时由 fakeMihomo 处理。
func cmdInstallMihomo(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 0 {
		ts.Fatalf("usage: install-mihomo")
	}
	exe, err := os.Executable()
	ts.Check(err)
	data, err := os.ReadFile(exe)
	ts.Check(err)
	dst := paths.Layout{Root: ts.Getenv(rootEnv)}.CoreBinary()
	ts.Check(os.MkdirAll(filepath.Dir(dst), 0o755))
	ts.Check(os.WriteFile(dst, data, 0o755))
	if runtime.GOOS == "windows" {
		// Windows 只执行带扩展名的文件；无扩展名的副本让存在性检查通过。
		ts.Check(os.WriteFile(dst+".exe", data, 0o755))
	}
}

// setupScript 把磁盘布局放进 $WORK/root（其中已有面板），并为每个脚本启动一个模拟机场 $PROVIDER。
// 模拟机场的行为由 $WORK/provider 下的文件决定，脚本用普通的文件操作控制它：
//   - GET /<name> 返回 provider/<name> 的内容，文件不存在时返回 404；
//   - provider/<name>.userinfo 存在时，其内容作为 subscription-userinfo 响应头；
//   - provider/.down 存在时一律返回 500；
//   - 每次请求的 User-Agent 写入 provider/.last-ua。
func setupScript(env *testscript.Env) error {
	root := filepath.Join(env.WorkDir, "root")
	env.Setenv(rootEnv, root)
	// 面板已存在，发布时不去 GitHub 下载。
	if err := os.MkdirAll(filepath.Join(paths.Layout{Root: root}.CoreDir(), "ui", "zashboard"), 0o755); err != nil {
		return err
	}
	dir := filepath.Join(env.WorkDir, "provider")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		os.WriteFile(filepath.Join(dir, ".last-ua"), []byte(r.UserAgent()+"\n"), 0o644)
		if _, err := os.Stat(filepath.Join(dir, ".down")); err == nil {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		name := filepath.Base(r.URL.Path)
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if info, err := os.ReadFile(filepath.Join(dir, name+".userinfo")); err == nil {
			w.Header().Set("Subscription-Userinfo", strings.TrimSpace(string(info)))
		}
		w.Write(data)
	}))
	env.Defer(srv.Close)
	env.Setenv("PROVIDER", srv.URL)
	return os.MkdirAll(dir, 0o755)
}

// cmdExitCode 实现 exitcode <code> <command> [args...]：运行命令并断言退出码。
func cmdExitCode(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("unsupported: ! exitcode")
	}
	if len(args) < 2 {
		ts.Fatalf("usage: exitcode <code> <command> [args...]")
	}
	want, err := strconv.Atoi(args[0])
	ts.Check(err)
	got := 0
	if err := ts.Exec(args[1], args[2:]...); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			ts.Fatalf("exec %s: %v", args[1], err)
		}
		got = ee.ExitCode()
	}
	if got != want {
		ts.Fatalf("exit code = %d, want %d", got, want)
	}
}
