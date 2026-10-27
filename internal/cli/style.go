package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bprogress "charm.land/bubbles/v2/progress"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// styles 给终端上的输出加颜色和图标。输出不是终端、设置了 NO_COLOR 或 TERM=dumb 时，所有方法
// 原样返回文字，输出与没有样式时完全相同，脚本和测试不受影响。只用 16 色，任何终端都能显示。
type styles struct {
	on bool
}

// detectStyles 判断 w 能否显示样式。
func detectStyles(w io.Writer) styles {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(f.Fd()) {
		return styles{}
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return styles{}
	}
	return styles{on: true}
}

func (s styles) render(st lipgloss.Style, text string) string {
	if !s.on || text == "" {
		return text
	}
	return st.Render(text)
}

var (
	styleGood  = lipgloss.NewStyle().Foreground(lipgloss.Green)
	styleBad   = lipgloss.NewStyle().Foreground(lipgloss.Red)
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	styleDim   = lipgloss.NewStyle().Faint(true)
	styleHunk  = lipgloss.NewStyle().Foreground(lipgloss.Cyan)
	styleTitle = lipgloss.NewStyle().Bold(true)
)

func (s styles) good(text string) string  { return s.render(styleGood, text) }
func (s styles) bad(text string) string   { return s.render(styleBad, text) }
func (s styles) warn(text string) string  { return s.render(styleWarn, text) }
func (s styles) dim(text string) string   { return s.render(styleDim, text) }
func (s styles) title(text string) string { return s.render(styleTitle, text) }

// size 返回标准输出所在终端的列数和行数，不是终端时都为 0。
func (a *app) size() (cols, rows int) {
	if a.screenSize == nil {
		return 0, 0
	}
	return a.screenSize()
}

// rows 返回标准输出所在终端的行数，不是终端时为 0。
func (a *app) rows() int {
	_, rows := a.size()
	return rows
}

// downloadProgress 把下载进度交给正在显示的步骤（经 download.WithProgress 传给 core）。
func (a *app) downloadProgress(name string, done, total int64) {
	if p := a.live.Load(); p != nil && p.s.on {
		p.Download(name, done, total)
	}
}

// clearScreen 清屏，但不丢掉之前的输出：先把光标移到最后一行，输出一屏换行把屏幕上的内容
// 全部推进回滚缓冲区，再回到左上角，往上翻仍看得到。标准输出不是终端时什么都不做。
func (a *app) clearScreen() {
	rows := a.rows()
	if rows == 0 {
		return
	}
	fmt.Fprintf(a.stdout, "\x1b[%d;1H%s\x1b[H", rows, strings.Repeat("\n", rows))
}

// page 输出 text。在终端里交互时超过一屏的内容交给分页器（像 git 一样）：$PAGER，默认 less；
// 没有设置 LESS 时用 FRX：不足一屏直接输出，保留颜色，退出后内容留在屏幕上。
// 分页器无法运行时直接输出。atEnd 时 less 从末尾开始显示（日志最新的在最后）。
func (a *app) page(text string, atEnd bool) error {
	rows := a.rows()
	if rows == 0 || !a.interactive() || strings.Count(text, "\n") < rows-1 {
		_, err := io.WriteString(a.stdout, text)
		return err
	}
	pager := strings.Fields(cmp.Or(os.Getenv("PAGER"), "less"))
	if len(pager) == 0 || pager[0] == "cat" {
		_, err := io.WriteString(a.stdout, text)
		return err
	}
	if atEnd && filepath.Base(pager[0]) == "less" {
		pager = append(pager, "+G")
	}
	c := exec.Command(pager[0], pager[1:]...)
	c.Env = os.Environ()
	if _, ok := os.LookupEnv("LESS"); !ok {
		c.Env = append(c.Env, "LESS=FRX")
	}
	c.Stdin, c.Stdout, c.Stderr = strings.NewReader(text), a.stdout, a.stderr
	if err := c.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil // 分页器自己报告了问题，内容已经显示过或用户已退出
		}
		_, err := io.WriteString(a.stdout, text)
		return err
	}
	return nil
}

// printCheck 输出一项检查或步骤的结果（status 为 ok、failed、skipped）。有样式时为
// “✓ 名称  说明”，跳过的项整行暗色；否则为“状态 名称 说明”三列，名称按 nameWidth 对齐。
func (a *app) printCheck(status, name string, nameWidth int, message string) {
	if !a.style.on {
		fmt.Fprintf(a.stdout, "%-8s %-*s %s\n", status, nameWidth, name, message)
		return
	}
	line := fmt.Sprintf("%-*s  %s", nameWidth, name, message)
	switch status {
	case "ok":
		fmt.Fprintf(a.stdout, "%s %s\n", styleGood.Render("✓"), line)
	case "failed":
		fmt.Fprintf(a.stdout, "%s %s\n", styleBad.Render("✗"), line)
	default:
		fmt.Fprintf(a.stdout, "%s\n", styleDim.Render("– "+line))
	}
}

// diff 给统一格式的 diff 上色：增加的行绿色，删除的行红色，段头青色，文件头暗色。
func (s styles) diff(text string) string {
	if !s.on {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	for i, l := range lines {
		body, nl := strings.CutSuffix(l, "\n")
		switch {
		case strings.HasPrefix(body, "+++"), strings.HasPrefix(body, "---"):
			body = styleDim.Render(body)
		case strings.HasPrefix(body, "@@"):
			body = styleHunk.Render(body)
		case strings.HasPrefix(body, "+"):
			body = styleGood.Render(body)
		case strings.HasPrefix(body, "-"):
			body = styleBad.Render(body)
		}
		if nl {
			body += "\n"
		}
		lines[i] = body
	}
	return strings.Join(lines, "")
}

// spinnerFrames 是进行中的步骤前转动的图案。
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerInterval 是 spinner 的刷新间隔。不必太快：经较慢的 SSH 连接也不该刷屏。
const spinnerInterval = 120 * time.Millisecond

// progress 显示一串步骤的进度。有样式时，进行中的步骤前是转动的 spinner，完成后换成 ✓ 或 ✗ 和耗时；
// 否则每步开始时输出一行“==> 步骤”，与没有样式时相同。
//
// 步骤进行中其他输出（日志、提示）须经 Write 写出：先擦掉 spinner 所在的行，写完再画回来。
type progress struct {
	mu    sync.Mutex
	w     io.Writer
	s     styles
	cols  int // 终端宽度，0 为未知；行超出时截断，否则 \r 无法回到行首重画
	step  string
	start time.Time
	frame int
	stop  chan struct{}
	done  chan struct{}
	// 当前步骤中正在下载的资产，没有时 dlTotal 为 0。
	dlName          string
	dlDone, dlTotal int64
	dlStart         time.Time
}

func newProgress(w io.Writer, s styles, cols int) *progress {
	return &progress{w: w, s: s, cols: cols}
}

// downloadBar 画下载进度条。bubbles 的 progress 只用来渲染字符串（ViewAs），不起 Bubble Tea 程序。
var downloadBar = func() bprogress.Model {
	m := bprogress.New(bprogress.WithWidth(16), bprogress.WithoutPercentage(), bprogress.WithColors(lipgloss.Cyan),
		bprogress.WithFillCharacters('█', '░'))
	m.EmptyColor = lipgloss.BrightBlack
	return m
}()

// Download 在当前步骤后显示资产的下载进度，由 download.WithProgress 的回调调用。
func (p *progress) Download(name string, done, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.step == "" || total <= 0 {
		return
	}
	if name != p.dlName || done < p.dlDone {
		p.dlName, p.dlStart = name, time.Now()
	}
	p.dlDone, p.dlTotal = done, total
}

// downloadDetail 是下载进度的文字，例如“━━━━━━──── 45% 8.1/18.0 MB 2.3 MB/s”。
func (p *progress) downloadDetail() string {
	if p.dlTotal <= 0 {
		return ""
	}
	const mb = 1 << 20
	frac := float64(p.dlDone) / float64(p.dlTotal)
	detail := fmt.Sprintf("%s %3.0f%% %.1f/%.1f MB", downloadBar.ViewAs(frac), frac*100,
		float64(p.dlDone)/mb, float64(p.dlTotal)/mb)
	if el := time.Since(p.dlStart).Seconds(); el >= 1 {
		detail += fmt.Sprintf(" %.1f MB/s", float64(p.dlDone)/mb/el)
	}
	return detail
}

// Step 结束上一步（记为成功）并开始新的一步。
func (p *progress) Step(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finishLocked(true)
	if !p.s.on {
		fmt.Fprintf(p.w, "==> %s\n", name)
		return
	}
	p.step, p.start, p.frame = name, time.Now(), 0
	p.drawLocked()
	p.stop, p.done = make(chan struct{}), make(chan struct{})
	go p.spin(p.stop, p.done)
}

// Done 结束当前的一步：err 为 nil 记为成功，否则记为失败。没有进行中的步骤时什么都不做。
func (p *progress) Done(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finishLocked(err == nil)
}

// Write 在进行中的步骤上方写出其他输出。
func (p *progress) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.step == "" {
		return p.w.Write(b)
	}
	fmt.Fprint(p.w, "\r\x1b[K")
	n, err := p.w.Write(b)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		fmt.Fprintln(p.w)
	}
	p.drawLocked()
	return n, err
}

func (p *progress) spin(stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(spinnerInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p.mu.Lock()
			select {
			case <-stop: // 等锁期间这一步已经结束
				p.mu.Unlock()
				return
			default:
			}
			p.frame++
			p.drawLocked()
			p.mu.Unlock()
		}
	}
}

func (p *progress) drawLocked() {
	frame := spinnerFrames[p.frame%len(spinnerFrames)]
	line := styleHunk.Render(frame) + " " + p.step
	if d := p.downloadDetail(); d != "" {
		line += "  " + d
	}
	if p.cols > 1 {
		line = ansi.Truncate(line, p.cols-1, "…")
	}
	fmt.Fprint(p.w, "\r\x1b[K"+line)
}

func (p *progress) finishLocked(ok bool) {
	if p.step == "" {
		return
	}
	// spin 可能正在等锁，不能持锁等它退出；关闭 stop 后它拿到锁就会退出。
	close(p.stop)
	icon := styleGood.Render("✓")
	if !ok {
		icon = styleBad.Render("✗")
	}
	elapsed := time.Since(p.start)
	took := ""
	if elapsed >= 100*time.Millisecond {
		took = " " + styleDim.Render(formatElapsed(elapsed))
	}
	fmt.Fprintf(p.w, "\r\x1b[K%s %s%s\n", icon, p.step, took)
	p.step = ""
	p.dlName, p.dlDone, p.dlTotal = "", 0, 0
}

func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

// spin 在执行 fn 期间显示一个步骤：有样式时为 spinner，否则不输出任何东西（保持原来的输出）。
// --json 时不显示。
func (a *app) spin(title string, fn func() error) error {
	if a.opts.JSON || !a.style.on {
		return fn()
	}
	step, done := a.stepper()
	step(title)
	err := fn()
	done(err)
	return err
}

// stepper 返回显示多个步骤进度的 Progress 回调和结束时调用的 done；--json 时都不输出。
// 进行中日志经由它输出（见 logWriter）。
func (a *app) stepper() (step func(string), done func(error)) {
	if a.opts.JSON {
		return nil, func(error) {}
	}
	cols, _ := a.size()
	p := newProgress(a.stdout, a.style, cols)
	a.live.Store(p)
	return p.Step, func(err error) {
		p.Done(err)
		a.live.CompareAndSwap(p, nil)
	}
}

// logWriter 是日志的输出：有步骤正在显示 spinner 时经由它写出，否则写到标准错误。
type logWriter struct{ a *app }

func (w logWriter) Write(b []byte) (int, error) {
	// 标准错误也是终端（与 spinner 在同一个屏幕上）时才需要绕道。
	if p := w.a.live.Load(); p != nil && p.s.on && w.a.errStyle.on {
		return p.Write(b)
	}
	return w.a.stderr.Write(b)
}
