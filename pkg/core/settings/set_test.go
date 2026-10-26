package settings

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
)

func TestSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "tunlr", "settings.yaml")

	if err := Set(path, "github-mirror", "https://gh-proxy.com/"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "github-mirror: https://gh-proxy.com/\n" {
		t.Fatalf("new file = %q", got)
	}

	// 保留其他键和注释。
	os.WriteFile(path, []byte("# 我的设置\nuser-agent: clash.meta # 机场要求\ngithub-mirror: https://a.example/\n"), 0o644)
	if err := Set(path, "github-mirror", "https://b.example/"); err != nil {
		t.Fatal(err)
	}
	want := "# 我的设置\nuser-agent: clash.meta # 机场要求\ngithub-mirror: https://b.example/\n"
	if got := read(t, path); got != want {
		t.Fatalf("updated = %q, want %q", got, want)
	}

	if err := Set(path, "github-mirror", ""); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "# 我的设置\nuser-agent: clash.meta # 机场要求\n" {
		t.Fatalf("after removing = %q", got)
	}

	before := read(t, path)
	if err := Set(path, "github-mirror", "ftp://x"); !errors.Is(err, core.ErrInvalidArgument) {
		t.Fatalf("invalid mirror: err = %v", err)
	}
	if err := Set(path, "no-such-key", "x"); !errors.Is(err, core.ErrInvalidArgument) {
		t.Fatalf("unknown key: err = %v", err)
	}
	if got := read(t, path); got != before {
		t.Fatalf("file changed by a rejected Set: %q", got)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
