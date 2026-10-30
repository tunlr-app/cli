package setup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/backup"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
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

// TestSuggestLANSwitches 验证还原时建议的放行入口：备份中选过、这台机器上也有的网卡。
func TestSuggestLANSwitches(t *testing.T) {
	i := &Installer{Layout: paths.Layout{Root: t.TempDir()}}
	i.Interfaces = func(name string) (*netfilter.Interface, error) {
		if name == "ens3" {
			return &netfilter.Interface{Name: name, Up: true}, nil
		}
		return nil, fs.ErrNotExist
	}
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	b := &backup.Backup{Switches: backup.Switches{PanelLAN: true, LANInterfaces: []string{"ens3", "eth9"}, LANContainers: true}}
	s, err := i.SuggestSwitches(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if !s.PanelLAN || s.ProxyLAN || !slices.Equal(s.LANInterfaces, []string{"ens3"}) || !slices.Equal(s.LANWas, []string{"ens3", "eth9"}) || !s.LANContainers {
		t.Errorf("suggestion = %+v", s)
	}
}

func TestWriteKeepsOtherFiles(t *testing.T) {
	i := &Installer{Layout: paths.Layout{Root: t.TempDir()}}
	if _, err := i.Backup(context.Background()); !errors.Is(err, core.ErrCheckFailed) {
		t.Fatalf("without state: err = %v", err)
	}
	st := &state.State{Version: state.Version, InstanceID: "8f0f8b4e-3c1d-4d6a-9b2e-0c5f6a7b8c9d",
		Kernels: []state.Kernel{{Version: "v1.19.33"}}, Tun: state.TunOn,
		Fetches: map[string]state.Fetch{"old": {FetchedAt: time.Now()}}}
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(i.Layout.ConfigDir(), 0o755)
	os.WriteFile(i.Layout.SubscriptionList(), []byte("active: old\nsubscriptions:\n  - {name: old, url: https://old.example/}\n"), 0o600)
	mixins := i.Layout.MixinDir()
	os.MkdirAll(mixins, 0o700)
	os.WriteFile(filepath.Join(mixins, "10-old.yaml"), []byte("a: 1\n"), 0o600)
	os.WriteFile(filepath.Join(mixins, ".10-old.yaml.swp"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(mixins, "README"), []byte("x"), 0o600)
	os.MkdirAll(filepath.Join(mixins, "old"), 0o700)
	os.WriteFile(filepath.Join(mixins, "old", "10-f.yaml"), []byte("a: 1\n"), 0o600)
	os.MkdirAll(filepath.Join(mixins, "kept"), 0o700)
	os.WriteFile(filepath.Join(mixins, "kept", "10-f.yaml"), []byte("a: 1\n"), 0o600)
	os.WriteFile(filepath.Join(mixins, "kept", "notes"), []byte("x"), 0o600)

	b := &backup.Backup{
		List:      []byte("active: new\nsubscriptions:\n  - {name: new, url: https://new.example/}\n"),
		APISecret: "s", Contents: map[string][]byte{"new": []byte("proxies: []\n")},
		Mixins: []config.Mixin{{Name: "new/10-filter.yaml", Data: []byte("b: 2\n")}},
	}
	if err := i.write(b); err != nil {
		t.Fatal(err)
	}
	// 备份中没有的订阅 mixin 被删除，目录中有别的文件时保留目录；备份中的写入各自的目录。
	for name, want := range map[string]bool{"10-old.yaml": false, ".10-old.yaml.swp": true, "README": true,
		"old": false, "kept/10-f.yaml": false, "kept/notes": true, "new/10-filter.yaml": true} {
		if _, err := os.Stat(filepath.Join(mixins, filepath.FromSlash(name))); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", name, err == nil, want)
		}
	}
	if all, err := config.LoadAllMixins(mixins); err != nil || len(all) != 1 || all[0].Name != "new/10-filter.yaml" {
		t.Errorf("mixins after restore = %+v, %v", all, err)
	}
	got, err := state.Load(i.Layout.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	// 本机的实例 ID、内核、开关不变。
	if got.InstanceID != st.InstanceID || len(got.Kernels) != 1 || got.Tun != state.TunOn {
		t.Errorf("local state changed: %+v", got)
	}
	// 抓取记录清空，订阅列表整体替换。
	if got.APISecret != "s" || got.Fetches != nil {
		t.Errorf("restored state: %+v", got)
	}
	if l, err := subscription.LoadList(i.Layout.SubscriptionList()); err != nil || l.Active != "new" || len(l.Subscriptions) != 1 {
		t.Errorf("restored list: %+v, %v", l, err)
	}
	if _, err := os.Stat(i.Layout.SubscriptionFile("new")); err != nil {
		t.Error(err)
	}
}
