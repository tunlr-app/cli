package settings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/templates"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		path := filepath.Join(dir, "settings.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	s, err := Load(filepath.Join(dir, "missing.yaml"))
	if err != nil || s.Version != Version || s.GitHubMirror != "" {
		t.Fatalf("missing file: %+v, %v", s, err)
	}
	s, err = Load(write("# only comments\n"))
	if err != nil || s.Version != Version {
		t.Fatalf("empty file: %+v, %v", s, err)
	}
	s, err = Load(write("github-mirror: https://gh-proxy.com/\n"))
	if err != nil || s.Version != Version || s.GitHubMirror != "https://gh-proxy.com/" {
		t.Fatalf("without version: %+v, %v", s, err)
	}
	s, err = Load(write("user-agent: clash-verge/v2.0.0\n"))
	if err != nil || s.UserAgent != "clash-verge/v2.0.0" {
		t.Fatalf("user-agent: %+v, %v", s, err)
	}

	tests := []struct{ content, want string }{
		{"version: 2\n", "version 2 is newer than this tunlr supports (1)"},
		{"github_mirror: https://gh-proxy.com/\n", "field github_mirror not found"},
		{"github-mirror: gh-proxy.com\n", "github-mirror must be an http or https URL"},
		{"github-mirror: [\n", "yaml:"},
		{"user-agent: \"a\\r\\nX-Injected: 1\"\n", "user-agent: invalid argument: User-Agent must not contain control characters"},
	}
	for _, tt := range tests {
		if _, err := Load(write(tt.content)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: err = %v, want %q", tt.content, err, tt.want)
		}
	}
}

func TestTemplateIsDefault(t *testing.T) {
	s, err := Parse(templates.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if *s != (Settings{Version: Version}) {
		t.Errorf("template sets %+v, want only defaults", *s)
	}
}

func TestWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "settings.yaml")
	read := func() string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	if err := Write(path, []byte("github-mirror: soon\n"), nil, false); !errors.Is(err, core.ErrInvalidArgument) {
		t.Fatalf("invalid: err = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid content was written: %v", err)
	}

	v1 := []byte("user-agent: a\n")
	if err := Write(path, v1, nil, false); err != nil || read() != string(v1) {
		t.Fatalf("create: %v", err)
	}
	// 编辑时文件还不存在，此后被别人创建了。
	if err := Write(path, []byte("user-agent: b\n"), nil, false); !errors.Is(err, ErrChanged) {
		t.Fatalf("created meanwhile: err = %v", err)
	}
	// 编辑期间文件内容变了。
	if err := Write(path, []byte("user-agent: b\n"), []byte("other\n"), true); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed meanwhile: err = %v", err)
	}
	v2 := []byte("user-agent: b\n")
	if err := Write(path, v2, v1, true); err != nil || read() != string(v2) {
		t.Fatalf("update: %v", err)
	}

	if err := Restore(path, v1, true); err != nil || read() != string(v1) {
		t.Fatalf("restore: %v", err)
	}
	if err := Restore(path, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore to missing: %v", err)
	}
	// 编辑时文件存在，此后被删除了。
	if err := Write(path, v2, v1, true); !errors.Is(err, ErrChanged) {
		t.Fatalf("removed meanwhile: err = %v", err)
	}
}

func TestEntries(t *testing.T) {
	got := (&Settings{Version: Version}).Entries("ua/1")
	want := []Entry{
		{Key: "github-mirror", Value: "", Default: true},
		{Key: "user-agent", Value: "ua/1", Default: true},
	}
	check := func(got, want []Entry) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %d entries, want %d", len(got), len(want))
		}
		for i := range want {
			got[i].Help = ""
			if got[i] != want[i] {
				t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	}
	check(got, want)

	set := &Settings{Version: Version, GitHubMirror: "https://m/", UserAgent: "x"}
	check(set.Entries("ua/1"), []Entry{
		{Key: "github-mirror", Value: "https://m/"},
		{Key: "user-agent", Value: "x"},
	})
}
