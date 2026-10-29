package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadOrNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "var", "lib", "tunlr", "state.json")
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.FixedZone("CST", 8*3600))

	if _, err := Load(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load before create: err = %v, want fs.ErrNotExist", err)
	}

	s, created, err := LoadOrNew(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("created = false on first call")
	}
	if s.Version != Version || !uuidPattern.MatchString(s.InstanceID) || !s.CreatedAt.Equal(now) {
		t.Errorf("unexpected state %+v", s)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadOrNew wrote the state file: %v", err)
	}
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}

	again, created, err := LoadOrNew(path, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if created || again.InstanceID != s.InstanceID || !again.CreatedAt.Equal(now) {
		t.Errorf("second call changed state: created=%v %+v, first %+v", created, again, s)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"version": 2,`) {
		t.Errorf("state file lacks version field:\n%s", data)
	}
}

func TestInstanceIDsDiffer(t *testing.T) {
	a, err := newInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !uuidPattern.MatchString(a) || !uuidPattern.MatchString(b) {
		t.Errorf("ids %q, %q", a, b)
	}
}

const validID = "0b6e2c1a-3f4d-4a5b-8c6d-7e8f9a0b1c2d"

func TestDecodeErrors(t *testing.T) {
	tests := []struct{ name, data, want string }{
		{"not json", `{`, "unexpected end of JSON input"},
		{"no version", `{"instance_id":"` + validID + `"}`, "missing or invalid version"},
		{"fractional version", `{"version":1.5,"instance_id":"` + validID + `"}`, "missing or invalid version"},
		{"newer", `{"version":9,"instance_id":"` + validID + `"}`, "version 9 is newer than this tunlr supports (2); upgrade tunlr"},
		{"bad id", `{"version":2,"instance_id":"abc"}`, `invalid instance_id "abc"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decode([]byte(tt.data), Version, migrations)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestDecodeMigrates 用假设的 v1→v2→v3 迁移验证升级链按顺序执行，且保留未涉及的字段。
func TestDecodeMigrates(t *testing.T) {
	var order []int
	migs := map[int]func(map[string]any) error{
		1: func(raw map[string]any) error {
			order = append(order, 1)
			raw["instance_id"] = raw["id"]
			delete(raw, "id")
			return nil
		},
		2: func(raw map[string]any) error {
			order = append(order, 2)
			return nil
		},
	}
	s, err := decode([]byte(`{"version":1,"id":"`+validID+`","created_at":"2026-01-02T03:04:05Z"}`), 3, migs)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Errorf("migration order = %v", order)
	}
	if s.Version != 3 || s.InstanceID != validID || s.CreatedAt.Year() != 2026 {
		t.Errorf("unexpected state %+v", s)
	}

	if _, err := decode([]byte(`{"version":1,"instance_id":"`+validID+`"}`), 3, map[int]func(map[string]any) error{}); err == nil ||
		err.Error() != "no migration from version 1" {
		t.Errorf("missing migration: err = %v", err)
	}
}
