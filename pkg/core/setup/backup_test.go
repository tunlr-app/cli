package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/backup"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/state"
)

func TestSaveBackupKeepsThree(t *testing.T) {
	i := &Installer{Layout: paths.Layout{Root: t.TempDir()}}
	start := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	var paths []string
	for n := range 5 {
		p, err := i.saveBackup(&backup.Backup{Version: backup.Version, CreatedAt: start.Add(time.Duration(n) * time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	entries, err := os.ReadDir(i.Layout.BackupsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != keepBackups {
		t.Fatalf("%d backups kept, want %d", len(entries), keepBackups)
	}
	for _, p := range paths[:2] {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s not removed", filepath.Base(p))
		}
	}
	if fi, err := os.Stat(paths[4]); err != nil || fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("newest backup: %v %v", fi, err)
	}
}

func TestWriteKeepsOtherFiles(t *testing.T) {
	i := &Installer{Layout: paths.Layout{Root: t.TempDir()}}
	if _, err := i.Backup(context.Background()); !errors.Is(err, core.ErrCheckFailed) {
		t.Fatalf("without state: err = %v", err)
	}
	st := &state.State{Version: state.Version, InstanceID: "8f0f8b4e-3c1d-4d6a-9b2e-0c5f6a7b8c9d",
		Kernels: []state.Kernel{{Version: "v1.19.33"}}, Tun: state.TunOn,
		Subscriptions: []state.Subscription{{Name: "old", URL: "https://old.example/"}}, Active: "old"}
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	mixins := i.Layout.MixinDir()
	os.MkdirAll(mixins, 0o700)
	os.WriteFile(filepath.Join(mixins, "10-old.yaml"), []byte("a: 1\n"), 0o600)
	os.WriteFile(filepath.Join(mixins, ".10-old.yaml.swp"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(mixins, "README"), []byte("x"), 0o600)

	b := &backup.Backup{
		Subscriptions: []state.Subscription{{Name: "new", URL: "https://new.example/"}}, Active: "new",
		APISecret: "s", Contents: map[string][]byte{"new": []byte("proxies: []\n")},
	}
	if err := i.write(b); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"10-old.yaml": false, ".10-old.yaml.swp": true, "README": true} {
		if _, err := os.Stat(filepath.Join(mixins, name)); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", name, err == nil, want)
		}
	}
	got, err := state.Load(i.Layout.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	// 本机的实例 ID、内核、开关不变。
	if got.InstanceID != st.InstanceID || len(got.Kernels) != 1 || got.Tun != state.TunOn {
		t.Errorf("local state changed: %+v", got)
	}
	if got.Active != "new" || got.APISecret != "s" || len(got.Subscriptions) != 1 {
		t.Errorf("restored state: %+v", got)
	}
	if _, err := os.Stat(i.Layout.SubscriptionFile("new")); err != nil {
		t.Error(err)
	}
}
