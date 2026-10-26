package paths

import (
	"path/filepath"
	"testing"
)

func TestLayout(t *testing.T) {
	root := t.TempDir()
	l := Layout{Root: root}
	tests := []struct{ got, want string }{
		{l.MixinFile(), filepath.Join(root, "etc", "tunlr", "mixin.yaml")},
		{l.StateFile(), filepath.Join(root, "var", "lib", "tunlr", "state.json")},
		{l.CoreConfig(), filepath.Join(root, "var", "lib", "tunlr", "core", "config.yaml")},
		{l.LockFile(), filepath.Join(root, "run", "lock", "tunlr.lock")},
		{Layout{}.StateFile(), filepath.FromSlash("/var/lib/tunlr/state.json")},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %s, want %s", tt.got, tt.want)
		}
	}
}
