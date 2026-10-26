package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	tests := []struct{ content, want string }{
		{"version: 2\n", "version 2 is newer than this tunlr supports (1)"},
		{"github_mirror: https://gh-proxy.com/\n", "field github_mirror not found"},
		{"github-mirror: gh-proxy.com\n", "github-mirror must be an http or https URL"},
		{"github-mirror: [\n", "yaml:"},
	}
	for _, tt := range tests {
		if _, err := Load(write(tt.content)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: err = %v, want %q", tt.content, err, tt.want)
		}
	}
}
