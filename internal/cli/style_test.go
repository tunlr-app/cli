package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
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
	p := newProgress(&buf, styles{})
	p.Step("one")
	p.Step("two")
	p.Done(nil)
	if buf.String() != "==> one\n==> two\n" {
		t.Errorf("plain progress = %q", buf.String())
	}

	buf.Reset()
	p = newProgress(&buf, styles{on: true})
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
