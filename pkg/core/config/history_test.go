package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHistory(t *testing.T) {
	h := &History{Dir: filepath.Join(t.TempDir(), "history"), Keep: 3}

	revs, err := h.List()
	if err != nil || len(revs) != 0 {
		t.Fatalf("empty history: %v, %v", revs, err)
	}
	for i, content := range []string{"a: 1\n", "a: 2\n", "a: 3\n", "a: 4\n"} {
		rev, err := h.Add([]byte(content), Revision{Reason: ReasonApply, Subscription: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if rev.Number != i+1 || rev.SHA256 != Checksum([]byte(content)) || rev.Version != revisionVersion {
			t.Errorf("add %d: %+v", i, rev)
		}
	}

	// 只保留最近 3 个，最新的在前。
	revs, err = h.List()
	if err != nil {
		t.Fatal(err)
	}
	var numbers []int
	for _, r := range revs {
		numbers = append(numbers, r.Number)
	}
	if len(numbers) != 3 || numbers[0] != 4 || numbers[2] != 2 {
		t.Fatalf("revisions = %v", numbers)
	}
	if _, err := os.Stat(filepath.Join(h.Dir, "1.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pruned config still exists: %v", err)
	}

	data, rev, err := h.Read(3)
	if err != nil || string(data) != "a: 3\n" || rev.Subscription != "main" {
		t.Fatalf("read 3: %q %+v %v", data, rev, err)
	}
	if _, _, err := h.Read(1); !errors.Is(err, ErrNoRevision) {
		t.Errorf("read pruned: err = %v", err)
	}

	// 配置含机场凭据，只有属主可读。
	if fi, err := os.Stat(filepath.Join(h.Dir, "4.yaml")); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("config permissions = %v", fi.Mode().Perm())
	}

	// 内容被改动时拒绝读取。
	if err := os.WriteFile(filepath.Join(h.Dir, "3.yaml"), []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.Read(3); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("tampered: err = %v", err)
	}

	// 缺少配置文件的记录和无关文件被跳过。
	os.Remove(filepath.Join(h.Dir, "2.yaml"))
	os.WriteFile(filepath.Join(h.Dir, "notes.txt"), nil, 0o600)
	os.WriteFile(filepath.Join(h.Dir, "07.json"), []byte("{}"), 0o600)
	revs, err = h.List()
	if err != nil || len(revs) != 2 {
		t.Fatalf("after damage: %+v %v", revs, err)
	}
}
