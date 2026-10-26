package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/templates"
)

// editValidator 在配置含 MATCH,NOPE 时报告检查未通过，含 GEO-DOWN 时报告数据下载失败。
type editValidator struct{}

func (editValidator) Validate(_ context.Context, cfg []byte) error {
	switch {
	case bytes.Contains(cfg, []byte("MATCH,NOPE")):
		return &mihomo.CheckError{Messages: []string{"rules[0] [MATCH,NOPE] error: proxy [NOPE] not found"}}
	case bytes.Contains(cfg, []byte("GEO-DOWN")):
		return &mihomo.CheckError{Messages: []string{"can't download geosite.dat"}, MissingData: true}
	}
	return nil
}

type stoppedKernel struct{}

func (stoppedKernel) Reload(context.Context) error { return mihomo.ErrNotRunning }

// scriptedPrompt 按顺序给出预设的回答，并记录问过的选项。
type scriptedPrompt struct {
	t       *testing.T
	choices []int
	inputs  []string
	asked   [][]string
}

func (p *scriptedPrompt) Choose(_ context.Context, _ string, options []string) (int, error) {
	p.asked = append(p.asked, options)
	if len(p.choices) == 0 {
		p.t.Fatalf("unexpected question: %v", options)
	}
	c := p.choices[0]
	p.choices = p.choices[1:]
	return c, nil
}

func (p *scriptedPrompt) Input(_ context.Context, _ string, validate func(string) error) (string, error) {
	if len(p.inputs) == 0 {
		p.t.Fatal("unexpected input request")
	}
	s := p.inputs[0]
	p.inputs = p.inputs[1:]
	return s, validate(s)
}

// editTest 是一次 tunlr edit 的测试环境：编辑器每次被调用时依次写入 edits 中的内容
// （nil 表示不改动），并记录打开时看到的内容。
type editTest struct {
	t      *testing.T
	a      *app
	stdout *bytes.Buffer
	prompt *scriptedPrompt
	edits  [][]byte
	seen   []string
	// during 在编辑器被调用时执行，模拟编辑期间的其他改动。
	during func()
}

func newEditTest(t *testing.T, edits ...string) *editTest {
	stdout := &bytes.Buffer{}
	et := &editTest{t: t, stdout: stdout, prompt: &scriptedPrompt{t: t}}
	for _, e := range edits {
		if e == "" {
			et.edits = append(et.edits, nil)
		} else {
			et.edits = append(et.edits, []byte(e))
		}
	}
	et.a = &app{
		info:      BuildInfo{Version: "test"},
		layout:    paths.Layout{Root: t.TempDir()},
		stdin:     os.Stdin,
		stdout:    stdout,
		stderr:    stdout,
		validator: editValidator{},
		reloader:  stoppedKernel{},
		terminal:  func() bool { return true },
		prompt:    et.prompt,
	}
	et.a.editor = func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		et.seen = append(et.seen, string(data))
		if et.during != nil {
			et.during()
		}
		if len(et.edits) == 0 {
			t.Fatal("editor opened more often than expected")
		}
		e := et.edits[0]
		et.edits = et.edits[1:]
		if e == nil {
			return nil
		}
		return os.WriteFile(path, e, 0o600)
	}
	return et
}

func (et *editTest) run(want int, args ...string) {
	et.t.Helper()
	if got := et.a.execute(context.Background(), args); got != want {
		et.t.Fatalf("exit code = %d, want %d\n%s", got, want, et.stdout)
	}
	if len(et.edits) > 0 {
		et.t.Errorf("editor not opened for %d edits", len(et.edits))
	}
}

func (et *editTest) mixin(name string) string {
	data, err := os.ReadFile(filepath.Join(et.a.layout.MixinDir(), name))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(data)
}

func TestEditNewFileAndApply(t *testing.T) {
	et := newEditTest(t, "log-level: debug\n")
	et.run(0, "edit", "20-home", "--yes")
	if len(et.seen) != 1 || et.seen[0] != string(templates.Mixin) {
		t.Errorf("new file did not start from the example: %q", et.seen)
	}
	if got := et.mixin("20-home.yaml"); got != "log-level: debug\n" {
		t.Errorf("saved = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(et.a.layout.MixinDir(), "20-home.yaml")); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mixin permissions = %v", fi.Mode().Perm())
	}
	out := et.stdout.String()
	for _, want := range []string{"Saved ", "Mixins (in order): 20-home.yaml", "Published revision 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	cfg, _ := os.ReadFile(et.a.layout.CoreConfig())
	if !bytes.Contains(cfg, []byte("\nlog-level: debug\n")) {
		t.Errorf("published config:\n%s", cfg)
	}
}

// TestEditFixMistake 验证有错的编辑不会保存：回到编辑器改正后才写入。
func TestEditFixMistake(t *testing.T) {
	et := newEditTest(t, "prepend-rules: x\n", "prepend-rules:\n  - DOMAIN,a.example,DIRECT\n")
	et.prompt.choices = []int{0}
	et.during = func() {
		if _, err := os.Stat(filepath.Join(et.a.layout.MixinDir(), "50-rules.yaml")); len(et.seen) == 2 && err == nil {
			t.Error("the mistake was saved")
		}
	}
	et.run(0, "edit", "50-rules.yaml", "--yes")
	if !strings.Contains(et.stdout.String(), "mixin 50-rules.yaml:1: prepend-rules: value must be a list") {
		t.Errorf("output:\n%s", et.stdout)
	}
	if et.seen[1] != "prepend-rules: x\n" {
		t.Errorf("second edit did not reopen the mistake: %q", et.seen[1])
	}
	if got := et.mixin("50-rules.yaml"); got != "prepend-rules:\n  - DOMAIN,a.example,DIRECT\n" {
		t.Errorf("saved = %q", got)
	}
}

func TestEditCheckFailedDiscard(t *testing.T) {
	et := newEditTest(t, "prepend-rules:\n  - MATCH,NOPE\n")
	writeMixin(t, et.a.layout, "50-rules.yaml", "log-level: info\n")
	et.prompt.choices = []int{1}
	et.run(0, "edit", "50-rules")
	out := et.stdout.String()
	for _, want := range []string{"Check (mihomo -t): FAILED", "proxy [NOPE] not found", "Discarded; nothing saved."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if got := et.mixin("50-rules.yaml"); got != "log-level: info\n" {
		t.Errorf("file changed: %q", got)
	}
	if len(et.prompt.asked) != 1 || strings.Join(et.prompt.asked[0], "|") != choiceEdit+"|"+choiceDiscard {
		t.Errorf("choices = %v", et.prompt.asked)
	}
}

// TestEditCheckIncomplete 验证校验未能完成时可以选择只保存、不发布。
func TestEditCheckIncomplete(t *testing.T) {
	et := newEditTest(t, "prepend-rules:\n  - GEOSITE,GEO-DOWN,DIRECT\n")
	et.prompt.choices = []int{1}
	et.run(0, "edit", "50-rules")
	if !strings.Contains(et.stdout.String(), "not applied") || !strings.Contains(et.mixin("50-rules.yaml"), "GEO-DOWN") {
		t.Errorf("output:\n%s", et.stdout)
	}
	if _, err := os.Stat(et.a.layout.CoreConfig()); !errors.Is(err, os.ErrNotExist) {
		t.Error("published although the check did not complete")
	}
}

func TestEditNoChanges(t *testing.T) {
	et := newEditTest(t, "")
	et.run(0, "edit", "20-home")
	if !strings.Contains(et.stdout.String(), "No changes.") {
		t.Errorf("output:\n%s", et.stdout)
	}
	if _, err := os.Stat(et.a.layout.MixinDir()); !errors.Is(err, os.ErrNotExist) {
		t.Error("an unchanged example was saved")
	}
}

func TestEditDryRun(t *testing.T) {
	et := newEditTest(t, "log-level: debug\n")
	et.run(0, "edit", "20-home", "--dry-run")
	if !strings.Contains(et.stdout.String(), "Dry run: the mixin was not saved.") {
		t.Errorf("output:\n%s", et.stdout)
	}
	if _, err := os.Stat(et.a.layout.MixinDir()); !errors.Is(err, os.ErrNotExist) {
		t.Error("dry run saved the mixin")
	}
}

// TestEditChooseNewFile 验证不给名称时可以选择新建文件并输入名称。
func TestEditChooseNewFile(t *testing.T) {
	et := newEditTest(t, "mode: global\n")
	writeMixin(t, et.a.layout, "10-a.yaml", "log-level: info\n")
	et.prompt.choices = []int{1}
	et.prompt.inputs = []string{"30-new"}
	et.run(0, "edit", "--yes")
	if strings.Join(et.prompt.asked[0], "|") != "10-a.yaml|New file..." {
		t.Errorf("choices = %v", et.prompt.asked)
	}
	if got := et.mixin("30-new.yaml"); got != "mode: global\n" {
		t.Errorf("saved = %q", got)
	}
}

// TestEditConcurrentChange 验证文件在编辑期间被改动时不覆盖，编辑结果保留在临时文件中。
func TestEditConcurrentChange(t *testing.T) {
	et := newEditTest(t, "log-level: debug\n")
	writeMixin(t, et.a.layout, "20-home.yaml", "log-level: info\n")
	et.during = func() { writeMixin(t, et.a.layout, "20-home.yaml", "log-level: error\n") }
	et.run(1, "edit", "20-home", "--yes")
	out := et.stdout.String()
	if !strings.Contains(out, "changed by someone else") || et.mixin("20-home.yaml") != "log-level: error\n" {
		t.Fatalf("output:\n%s", out)
	}
	_, kept, _ := strings.Cut(out, "your version is kept in ")
	kept = strings.TrimSpace(kept)
	if data, err := os.ReadFile(kept); err != nil || string(data) != "log-level: debug\n" {
		t.Errorf("kept copy %q: %q %v", kept, data, err)
	}
	os.Remove(kept)
}

func TestEditUsage(t *testing.T) {
	et := newEditTest(t)
	et.run(2, "edit", "../x")
	et.a.terminal = func() bool { return false }
	et.run(2, "edit", "20-home")
	if !strings.Contains(et.stdout.String(), "edit needs a terminal") {
		t.Errorf("output:\n%s", et.stdout)
	}
}

func writeMixin(t *testing.T, l paths.Layout, name, content string) {
	t.Helper()
	if err := os.MkdirAll(l.MixinDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.MixinDir(), name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
