package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestStylesOffIsPlain(t *testing.T) {
	var s styles
	if got := s.good("on") + s.diff("+a\n-b\n"); got != "on+a\n-b\n" {
		t.Errorf("plain styles changed the text: %q", got)
	}
}

func TestDiffColors(t *testing.T) {
	s := styles{on: true}
	got := s.diff("--- current\n+++ new\n@@ -1 +1 @@\n-a\n+b\n c\n")
	lines := strings.Split(got, "\n")
	if len(lines) != 7 || lines[5] != " c" || lines[6] != "" {
		t.Fatalf("lines = %q", lines)
	}
	for i, want := range []string{"\x1b[2m", "\x1b[2m", "\x1b[36m", "\x1b[31m", "\x1b[32m"} {
		if !strings.HasPrefix(lines[i], want) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], want)
		}
	}
}

func TestPrintCheck(t *testing.T) {
	var buf bytes.Buffer
	a := &app{stdout: &buf}
	a.printCheck("ok", "dns", 6, "resolved")
	a.style.on = true
	a.printCheck("failed", "ssh", 6, "not acknowledged")
	a.printCheck("skipped", "http", 6, "no network")
	got := strings.Split(buf.String(), "\n")
	if got[0] != "ok       dns    resolved" {
		t.Errorf("plain = %q", got[0])
	}
	if !strings.Contains(got[1], "✗") || !strings.HasSuffix(got[1], "ssh     not acknowledged") {
		t.Errorf("failed = %q", got[1])
	}
	if !strings.Contains(got[2], "– http") {
		t.Errorf("skipped = %q", got[2])
	}
}

func TestProgress(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, styles{}, 0)
	p.Step("one")
	p.Step("two")
	p.Done(nil)
	if buf.String() != "==> one\n==> two\n" {
		t.Errorf("plain progress = %q", buf.String())
	}

	buf.Reset()
	p = newProgress(&buf, styles{on: true}, 0)
	p.Step("fetch")
	time.Sleep(3 * spinnerInterval)
	p.Write([]byte("a log line\n"))
	p.Step("publish")
	p.Done(errors.New("boom"))
	p.Done(nil) // 没有进行中的步骤
	time.Sleep(2 * spinnerInterval)
	out := buf.String()
	final := strings.Split(strings.ReplaceAll(out, "\r", "\n"), "\n")
	var kept []string
	for _, l := range final {
		if l != "" && !strings.HasPrefix(l, "\x1b[K\x1b[36m") { // 去掉 spinner 的各帧
			kept = append(kept, strings.TrimPrefix(l, "\x1b[K"))
		}
	}
	if len(kept) != 3 || kept[0] != "a log line" || !strings.Contains(kept[1], "✓") || !strings.Contains(kept[1], "fetch") ||
		!strings.Contains(kept[2], "✗") || !strings.Contains(kept[2], "publish") {
		t.Errorf("lines = %q\nraw = %q", kept, out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("spinner drew after the last step: %q", out)
	}
}

func TestProgressDownload(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, styles{on: true}, 60)
	p.Step("Installing mihomo for linux/amd64")
	p.Download("mihomo.gz", 5<<20, 20<<20)
	p.mu.Lock()
	detail := p.downloadDetail()
	buf.Reset()
	p.drawLocked()
	line := strings.TrimPrefix(buf.String(), "\r\x1b[K")
	p.mu.Unlock()
	p.Done(nil)
	if plain := ansi.Strip(detail); !strings.Contains(plain, " 25% 5.0/20.0 MB") {
		t.Errorf("detail = %q", plain)
	}
	// 超出终端宽度的行被截断，否则 \r 回不到行首。
	if w := ansi.StringWidth(line); w != 59 || !strings.HasSuffix(ansi.Strip(line), "…") {
		t.Errorf("line width %d: %q", w, ansi.Strip(line))
	}
	// 步骤结束后进度清空。
	p.Step("next")
	p.mu.Lock()
	if d := p.downloadDetail(); d != "" {
		t.Errorf("detail after the step = %q", d)
	}
	p.mu.Unlock()
	p.Done(nil)
}
