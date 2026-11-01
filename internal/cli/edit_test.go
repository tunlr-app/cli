package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/templates"
)

// editValidator 在配置含 MATCH,NOPE 时报告检查未通过，含 GEO-DOWN 时报告数据下载失败。
type editValidator struct{}

func (editValidator) Validate(_ context.Context, cfg []byte, _ *config.GeoNeeds) error {
	switch {
	case bytes.Contains(cfg, []byte("MATCH,NOPE")):
		return &mihomo.CheckError{Messages: []string{"rules[0] [MATCH,NOPE] error: proxy [NOPE] not found"}}
	case bytes.Contains(cfg, []byte("GEO-DOWN")):
		return &mihomo.CheckError{Messages: []string{"can't download geosite.dat"}, MissingData: true}
	}
	return nil
}

// unreachableGitHub 返回一个对所有请求都返回 404 的地址，测试不访问真正的 GitHub。
func unreachableGitHub(t *testing.T) string {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return srv.URL
}

type stoppedKernel struct{}

func (stoppedKernel) Reload(context.Context) error { return mihomo.ErrNotRunning }

// scriptedPrompt 按顺序给出预设的回答，并记录问过的选项。choices 中的 -1 表示用户取消。
type scriptedPrompt struct {
	t       *testing.T
	choices []int
	// picks 按选项开头的文字选择，比 choices 优先；"" 表示用户取消。
	picks []string
	// many 是多选的回答，nil 表示用户取消；selected 记录预先勾上的项。
	many     [][]int
	selected [][]int
	inputs   []string
	asked    [][]string
	// initials 记录单选时光标的起始位置。
	initials []int
	// described、prefilled 记录输入时的说明和预先填好的内容。
	described []string
	prefilled []string
	// confirms 是是非题的回答；confirmed 记录问过的问题和说明。
	confirms  []bool
	confirmed []string
	// texts 是多行文字的回答，cancelText 表示用户取消，keepText 表示不改预先填好的内容；textShown 记录预先填好的
	// 内容和说明（以 | 分隔）。
	texts     []string
	textShown []string
}

// cancelText、keepText 是 scriptedPrompt.texts 中表示用户取消、不改预先填好的内容的回答。
const (
	cancelText = "<cancel>"
	keepText   = "<keep>"
)

func (p *scriptedPrompt) Text(_ context.Context, _, description, value string, validate func(string) error) (string, error) {
	p.textShown = append(p.textShown, value+"|"+description)
	if len(p.texts) == 0 {
		p.t.Fatal("unexpected text request")
	}
	s := p.texts[0]
	p.texts = p.texts[1:]
	switch s {
	case cancelText:
		return "", huh.ErrUserAborted
	case keepText:
		s = value
	}
	if validate != nil {
		if err := validate(s); err != nil {
			p.t.Fatalf("text %q does not validate: %v", s, err)
		}
	}
	return s, nil
}

func (p *scriptedPrompt) Confirm(_ context.Context, title, description string) (bool, error) {
	p.confirmed = append(p.confirmed, title+"|"+description)
	if len(p.confirms) == 0 {
		p.t.Fatalf("unexpected confirmation: %s", title)
	}
	ok := p.confirms[0]
	p.confirms = p.confirms[1:]
	return ok, nil
}

func (p *scriptedPrompt) Choose(_ context.Context, _ string, options []string, initial int) (int, error) {
	p.asked = append(p.asked, options)
	p.initials = append(p.initials, initial)
	if len(p.picks) > 0 {
		pick := p.picks[0]
		p.picks = p.picks[1:]
		if pick == "" {
			return 0, huh.ErrUserAborted
		}
		for i, o := range options {
			if strings.HasPrefix(o, pick) {
				return i, nil
			}
		}
		p.t.Fatalf("no option %q in %q", pick, options)
	}
	if len(p.choices) == 0 {
		p.t.Fatalf("unexpected question: %v", options)
	}
	c := p.choices[0]
	p.choices = p.choices[1:]
	if c < 0 {
		return 0, huh.ErrUserAborted
	}
	return c, nil
}

// ChooseRow 也用 picks：以 key: 开头的回答按下那个键，否则选包含这段文字的第一行（单元格以两个空格相连）；
// 每次的各行记在 asked 中。
func (p *scriptedPrompt) ChooseRow(_ context.Context, c rowChoice) (int, string, error) {
	rows := make([]string, len(c.rows))
	for i, r := range c.rows {
		rows[i] = strings.Join(r, "  ")
	}
	p.asked = append(p.asked, rows)
	p.initials = append(p.initials, c.initial)
	if len(p.picks) == 0 {
		p.t.Fatalf("unexpected table: %q", rows)
	}
	pick := p.picks[0]
	p.picks = p.picks[1:]
	if pick == "" {
		return 0, "", huh.ErrUserAborted
	}
	if k, ok := strings.CutPrefix(pick, "key:"); ok {
		return -1, k, nil
	}
	for i, r := range rows {
		if strings.Contains(r, pick) {
			return i, "", nil
		}
	}
	p.t.Fatalf("no row %q in %q", pick, rows)
	return 0, "", nil
}

func (p *scriptedPrompt) ChooseMany(_ context.Context, _ string, options []string, selected []int) ([]int, error) {
	p.asked = append(p.asked, options)
	p.selected = append(p.selected, selected)
	if len(p.many) == 0 {
		p.t.Fatalf("unexpected question: %v", options)
	}
	c := p.many[0]
	p.many = p.many[1:]
	if c == nil {
		return nil, huh.ErrUserAborted
	}
	return c, nil
}

func (p *scriptedPrompt) Input(_ context.Context, _, description, value string, validate func(string) error) (string, error) {
	p.described = append(p.described, description)
	p.prefilled = append(p.prefilled, value)
	if len(p.inputs) == 0 {
		p.t.Fatal("unexpected input request")
	}
	s := p.inputs[0]
	p.inputs = p.inputs[1:]
	return s, validate(s)
}

// editTest 是一次 tunlr config edit 的测试环境：编辑器每次被调用时依次写入 edits 中的内容
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
		download:  &download.Client{APIBase: unreachableGitHub(t)},
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
	et := newEditTest(t, "mihomo:\n  log-level: debug\n")
	et.run(0, "config", "edit", "20-home", "--yes")
	if len(et.seen) != 1 || et.seen[0] != string(templates.Mixin) {
		t.Errorf("new file did not start from the example: %q", et.seen)
	}
	if got := et.mixin("20-home.yaml"); got != "mihomo:\n  log-level: debug\n" {
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
	et := newEditTest(t, "tunlr:\n  rules:\n    prepend: x\n", "tunlr:\n  rules:\n    prepend:\n      - DOMAIN,a.example,DIRECT\n")
	et.prompt.choices = []int{0}
	et.during = func() {
		if _, err := os.Stat(filepath.Join(et.a.layout.MixinDir(), "50-rules.yaml")); len(et.seen) == 2 && err == nil {
			t.Error("the mistake was saved")
		}
	}
	et.run(0, "config", "edit", "50-rules.yaml", "--yes")
	if !strings.Contains(et.stdout.String(), "mixin 50-rules.yaml:3: tunlr.rules.prepend: value must be a list") {
		t.Errorf("output:\n%s", et.stdout)
	}
	if et.seen[1] != "tunlr:\n  rules:\n    prepend: x\n" {
		t.Errorf("second edit did not reopen the mistake: %q", et.seen[1])
	}
	if got := et.mixin("50-rules.yaml"); got != "tunlr:\n  rules:\n    prepend:\n      - DOMAIN,a.example,DIRECT\n" {
		t.Errorf("saved = %q", got)
	}
}

func TestEditCheckFailedDiscard(t *testing.T) {
	et := newEditTest(t, "tunlr:\n  rules:\n    prepend:\n      - MATCH,NOPE\n")
	writeMixin(t, et.a.layout, "50-rules.yaml", "mihomo:\n  log-level: info\n")
	// 先按 Esc，在确认时反悔，回到选项；再选放弃并确认。
	et.prompt.choices = []int{-1, 1}
	et.prompt.confirms = []bool{false, true}
	et.run(0, "config", "edit", "50-rules")
	out := et.stdout.String()
	for _, want := range []string{"Check (mihomo -t): FAILED", "proxy [NOPE] not found", "Discarded; nothing saved."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if got := et.mixin("50-rules.yaml"); got != "mihomo:\n  log-level: info\n" {
		t.Errorf("file changed: %q", got)
	}
	if len(et.prompt.asked) != 2 || strings.Join(et.prompt.asked[0], "|") != choiceEdit+"|"+choiceDiscard {
		t.Errorf("choices = %v", et.prompt.asked)
	}
	if len(et.prompt.confirmed) != 2 || !strings.HasPrefix(et.prompt.confirmed[0], "Discard your edits?") {
		t.Errorf("confirmed = %q", et.prompt.confirmed)
	}
	if !et.a.aborted {
		t.Error("discarding should count as cancelled")
	}
}

// TestEditCheckIncomplete 验证校验未能完成时可以选择只保存、不发布。
func TestEditCheckIncomplete(t *testing.T) {
	et := newEditTest(t, "tunlr:\n  rules:\n    prepend:\n      - GEOSITE,GEO-DOWN,DIRECT\n")
	et.prompt.choices = []int{1}
	et.run(0, "config", "edit", "50-rules")
	if !strings.Contains(et.stdout.String(), "not applied") || !strings.Contains(et.mixin("50-rules.yaml"), "GEO-DOWN") {
		t.Errorf("output:\n%s", et.stdout)
	}
	if _, err := os.Stat(et.a.layout.CoreConfig()); !errors.Is(err, os.ErrNotExist) {
		t.Error("published although the check did not complete")
	}
}

func TestEditNoChanges(t *testing.T) {
	et := newEditTest(t, "")
	et.run(0, "config", "edit", "20-home")
	if !strings.Contains(et.stdout.String(), "No changes.") {
		t.Errorf("output:\n%s", et.stdout)
	}
	if _, err := os.Stat(et.a.layout.MixinDir()); !errors.Is(err, os.ErrNotExist) {
		t.Error("an unchanged example was saved")
	}
}

func TestEditDryRun(t *testing.T) {
	et := newEditTest(t, "mihomo:\n  log-level: debug\n")
	et.run(0, "config", "edit", "20-home", "--dry-run")
	if !strings.Contains(et.stdout.String(), "Dry run: the mixin was not saved.") {
		t.Errorf("output:\n%s", et.stdout)
	}
	if _, err := os.Stat(et.a.layout.MixinDir()); !errors.Is(err, os.ErrNotExist) {
		t.Error("dry run saved the mixin")
	}
}

// TestEditChooseNewFile 验证不给名称时可以选择新建文件并输入名称。
func TestEditChooseNewFile(t *testing.T) {
	et := newEditTest(t, "mihomo:\n  mode: global\n")
	writeMixin(t, et.a.layout, "10-a.yaml", "mihomo:\n  log-level: info\n")
	writeMixin(t, et.a.layout, "20-b.yaml", "mihomo:\n  log-level: info\n")
	et.prompt.choices = []int{2}
	et.prompt.inputs = []string{"30-new"}
	et.run(0, "config", "edit", "--yes")
	if strings.Join(et.prompt.asked[0], "|") != "10-a.yaml|20-b.yaml|New file for global (all subscriptions)..." {
		t.Errorf("choices = %v", et.prompt.asked)
	}
	if len(et.prompt.described) != 1 || !strings.Contains(et.prompt.described[0], "Current order: 10-a.yaml → 20-b.yaml") {
		t.Errorf("name prompt description = %q", et.prompt.described)
	}
	if got := et.mixin("30-new.yaml"); got != "mihomo:\n  mode: global\n" {
		t.Errorf("saved = %q", got)
	}
}

// TestEditChooseSubscriptionFile 验证不给名称时可以给任何一个订阅新建 mixin，启用的订阅排在前面；
// 没有启用的订阅的 mixin 校验后保存，不发布。
func TestEditChooseSubscriptionFile(t *testing.T) {
	et := newEditTest(t, "tunlr:\n  proxy-filter:\n    exclude-filter: x\n")
	addSubscriptions(t, et.a, "main", "backup", "main")
	et.prompt.choices = []int{2}
	et.prompt.inputs = []string{"10-filter"}
	et.run(0, "config", "edit", "--yes")
	want := "New file for global (all subscriptions)...|New file for subscription main (active)...|New file for subscription backup (applied after tunlr sub use backup)..."
	if strings.Join(et.prompt.asked[0], "|") != want {
		t.Errorf("choices = %q", et.prompt.asked[0])
	}
	if got := et.mixin("backup/10-filter.yaml"); got != "tunlr:\n  proxy-filter:\n    exclude-filter: x\n" {
		t.Errorf("saved = %q", got)
	}
	if strings.Contains(et.stdout.String(), "Published revision") {
		t.Errorf("published a mixin of an inactive subscription:\n%s", et.stdout)
	}
}

// TestSubAddAsksFilter 验证在终端里添加订阅时询问节点筛选（留空的项不写），给了 --include 或 --exclude 时不问。
func TestSubAddAsksFilter(t *testing.T) {
	et := newEditTest(t)
	file := filepath.Join(t.TempDir(), "airport.yaml")
	sub := "proxies:\n  - {name: 香港 01, type: ss, server: a.example.net, port: 1, cipher: aes-128-gcm, password: p}\n" +
		"  - {name: 日本 01, type: ss, server: b.example.net, port: 1, cipher: aes-128-gcm, password: p}\nrules:\n  - MATCH,DIRECT\n"
	if err := os.WriteFile(file, []byte(sub), 0o600); err != nil {
		t.Fatal(err)
	}
	et.prompt.choices = []int{0}
	et.prompt.inputs = []string{"香港", ""}
	et.run(0, "sub", "add", "home", file, "--yes")
	if got := et.mixin("home/10-filter.yaml"); !strings.Contains(got, "filter: 香港") || strings.Contains(got, "exclude-filter:") {
		t.Errorf("filter mixin = %q", got)
	}

	// 两项都留空时不写文件；给了参数时不问。
	et.prompt.choices = []int{0}
	et.prompt.inputs = []string{"", ""}
	et.run(0, "sub", "add", "spare", file, "--yes")
	if _, err := os.Stat(et.a.layout.SubscriptionMixinDir("spare")); !errors.Is(err, os.ErrNotExist) {
		t.Error("an empty filter was written")
	}
	et.run(0, "sub", "add", "third", file, "--update", "off", "--exclude", "日本")
	if len(et.prompt.inputs) != 0 || !strings.Contains(et.mixin("third/10-filter.yaml"), "exclude-filter: 日本") {
		t.Errorf("third: %q", et.mixin("third/10-filter.yaml"))
	}
}

// TestFilterPortsAsks 验证 filter ports on、set 不带端口时询问，输入框预先填好保存的列表。
func TestFilterPortsAsks(t *testing.T) {
	et := newEditTest(t)
	et.prompt.inputs = []string{"80,8000-9000"}
	et.run(0, "filter", "ports", "on", "--yes")
	if want := strings.Join(pipeline.DefaultPorts, ","); len(et.prompt.prefilled) != 1 || et.prompt.prefilled[0] != want {
		t.Errorf("prefilled = %q, want %q", et.prompt.prefilled, want)
	}
	cfg, _ := os.ReadFile(et.a.layout.CoreConfig())
	if !bytes.Contains(cfg, []byte("NOT,((DST-PORT,80/8000-9000)),DIRECT")) {
		t.Errorf("published config:\n%s", cfg)
	}
	et.prompt.inputs = []string{"443"}
	et.run(0, "filter", "ports", "set", "--yes")
	if et.prompt.prefilled[1] != "80,8000-9000" {
		t.Errorf("prefilled = %q", et.prompt.prefilled)
	}
	// 取消时什么都不改。
	et.prompt.inputs = nil
	et.a.prompt = cancelInput{et.prompt}
	et.run(0, "filter", "ports", "set", "--yes")
	cfg, _ = os.ReadFile(et.a.layout.CoreConfig())
	if !bytes.Contains(cfg, []byte("NOT,((DST-PORT,443)),DIRECT")) {
		t.Errorf("published config:\n%s", cfg)
	}
}

// cancelInput 是输入时用户按 Esc 的提问者。
type cancelInput struct{ *scriptedPrompt }

func (cancelInput) Input(context.Context, string, string, string, func(string) error) (string, error) {
	return "", huh.ErrUserAborted
}

// TestEditConcurrentChange 验证文件在编辑期间被改动时不覆盖，编辑结果保留在临时文件中。
func TestEditConcurrentChange(t *testing.T) {
	et := newEditTest(t, "mihomo:\n  log-level: debug\n")
	writeMixin(t, et.a.layout, "20-home.yaml", "mihomo:\n  log-level: info\n")
	et.during = func() { writeMixin(t, et.a.layout, "20-home.yaml", "mihomo:\n  log-level: error\n") }
	et.run(1, "config", "edit", "20-home", "--yes")
	out := et.stdout.String()
	if !strings.Contains(out, "changed by someone else") || et.mixin("20-home.yaml") != "mihomo:\n  log-level: error\n" {
		t.Fatalf("output:\n%s", out)
	}
	_, kept, _ := strings.Cut(out, "your version is kept in ")
	kept = strings.TrimSpace(kept)
	if data, err := os.ReadFile(kept); err != nil || string(data) != "mihomo:\n  log-level: debug\n" {
		t.Errorf("kept copy %q: %q %v", kept, data, err)
	}
	os.Remove(kept)
}

func TestEditUsage(t *testing.T) {
	et := newEditTest(t)
	et.run(2, "config", "edit", "../x")
	et.a.terminal = func() bool { return false }
	et.run(2, "config", "edit", "20-home")
	if !strings.Contains(et.stdout.String(), "config edit needs a terminal") {
		t.Errorf("output:\n%s", et.stdout)
	}
}

func TestQuitOnCancel(t *testing.T) {
	f := huh.NewForm(huh.NewGroup(huh.NewNote()))
	filter := quitOnCancel(f)
	if _, ok := filter(nil, tea.InterruptMsg{}).(tea.InterruptMsg); !ok {
		t.Error("an interrupt the user did not ask for must stay an interrupt")
	}
	f.State = huh.StateAborted
	if _, ok := filter(nil, tea.InterruptMsg{}).(tea.QuitMsg); !ok {
		t.Error("a cancelled form must quit normally")
	}
	if _, ok := filter(nil, tea.KeyPressMsg{Code: 'q'}).(tea.KeyPressMsg); !ok {
		t.Error("other messages must pass through")
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

// 没有校验函数的提问（如 init 的订阅地址）提交时不能崩溃：huh 的 Validate(nil) 会把默认的校验换成 nil。
func TestInputWithoutValidate(t *testing.T) {
	v := "https://x.example/sub"
	in := newInput("Subscription URL (optional)", "", &v, nil)
	in.Focus()
	in.Blur() // 失去焦点时校验，与提交相同
	if err := in.Error(); err != nil {
		t.Errorf("error = %v", err)
	}
	in = newInput("Name", "", &v, func(string) error { return errors.New("bad") })
	in.Focus()
	in.Blur()
	if in.Error() == nil {
		t.Error("the validate function was not used")
	}
}
