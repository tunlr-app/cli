package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := WriteFile(path, []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2\n" {
		t.Errorf("content = %q, want %q", got, "v2\n")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("perm = %v, want 0600", fi.Mode().Perm())
		}
	}
	assertOnlyFiles(t, dir, "config.yaml")
}

func TestWriteFileMissingDir(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFile(filepath.Join(dir, "missing", "config.yaml"), []byte("x"), 0o600); err == nil {
		t.Fatal("want error for missing directory")
	}
	assertOnlyFiles(t, dir)
}

// assertOnlyFiles 确认目录中只有给定的文件，没有残留的临时文件。
func assertOnlyFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != len(names) {
		t.Fatalf("files = %v, want %v", got, names)
	}
	for i := range got {
		if got[i] != names[i] {
			t.Fatalf("files = %v, want %v", got, names)
		}
	}
}
