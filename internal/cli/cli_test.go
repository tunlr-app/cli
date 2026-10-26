package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"tunlr": func() {
			os.Exit(Main(BuildInfo{Version: "v0.0.0-test", Commit: "abc1234", Date: "2026-01-01"}, os.Args[1:]))
		},
	})
}

func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:   "testdata/script",
		Setup: setupScript,
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"exitcode": cmdExitCode,
		},
	})
}

// setupScript 把磁盘布局放进 $WORK/root，并为每个脚本启动一个模拟机场 $PROVIDER。
// 模拟机场的行为由 $WORK/provider 下的文件决定，脚本用普通的文件操作控制它：
//   - GET /<name> 返回 provider/<name> 的内容，文件不存在时返回 404；
//   - provider/<name>.userinfo 存在时，其内容作为 subscription-userinfo 响应头；
//   - provider/.down 存在时一律返回 500；
//   - 每次请求的 User-Agent 写入 provider/.last-ua。
func setupScript(env *testscript.Env) error {
	env.Setenv(rootEnv, filepath.Join(env.WorkDir, "root"))
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
