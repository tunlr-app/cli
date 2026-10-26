package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/paths"
)

func TestRenderUnits(t *testing.T) {
	units, err := RenderUnits(paths.Layout{})
	if err != nil {
		t.Fatal(err)
	}
	// 测试也在 Windows 上运行，路径分隔符统一为 /。
	for name, data := range units {
		units[name] = []byte(filepath.ToSlash(string(data)))
	}
	core := string(units[CoreUnit])
	for _, want := range []string{
		"User=tunlr\n",
		"ExecStart=/var/lib/tunlr/bin/mihomo -d /var/lib/tunlr/core\n",
		"ReadWritePaths=/var/lib/tunlr/bin /var/lib/tunlr/core\n",
		"OnFailure=" + RescueUnit + "\n",
	} {
		if !strings.Contains(core, want) {
			t.Errorf("%s lacks %q:\n%s", CoreUnit, want, core)
		}
	}
	if !strings.Contains(string(units[RescueUnit]), "ExecStart=/usr/local/bin/tunlr _rescue\n") {
		t.Errorf("%s:\n%s", RescueUnit, units[RescueUnit])
	}
	if !strings.Contains(string(units[UpdateUnit]), "ExecStart=/usr/local/bin/tunlr _update\n") {
		t.Errorf("%s:\n%s", UpdateUnit, units[UpdateUnit])
	}
	for name, data := range units {
		if strings.Contains(string(data), "{{") || strings.Contains(string(data), "<no value>") {
			t.Errorf("%s not fully rendered:\n%s", name, data)
		}
	}
}

// fakeSystemctl 记录执行的命令，并对 show 返回 out。
type fakeSystemctl struct {
	calls [][]string
	out   string
}

func (f *fakeSystemctl) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if len(args) > 0 && args[0] == "show" {
		return []byte(f.out), nil
	}
	return nil, nil
}

func TestStatus(t *testing.T) {
	f := &fakeSystemctl{out: "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=1234\nNRestarts=2\n"}
	s := &Systemd{Run: f.run}
	st, err := s.Status(context.Background(), CoreUnit)
	if err != nil {
		t.Fatal(err)
	}
	want := Status{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 1234, NRestarts: 2}
	if st != want || !st.Active() {
		t.Fatalf("status = %+v", st)
	}
}

func TestInstallAndUninstall(t *testing.T) {
	f := &fakeSystemctl{}
	s := &Systemd{Layout: paths.Layout{Root: t.TempDir()}, Run: f.run}
	ctx := context.Background()
	if err := s.Install(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range Units {
		if _, err := os.Stat(filepath.Join(s.Layout.UnitDir(), name)); err != nil {
			t.Errorf("%s not written: %v", name, err)
		}
	}
	if !s.Installed() {
		t.Error("Installed() = false after Install")
	}
	want := [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", CoreUnit},
		{"systemctl", "enable", "--now", UpdateTimer},
	}
	if !slices.EqualFunc(f.calls, want, slices.Equal) {
		t.Errorf("calls = %v", f.calls)
	}

	if err := s.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Installed() {
		t.Error("Installed() = true after Uninstall")
	}
	entries, _ := os.ReadDir(s.Layout.UnitDir())
	if len(entries) != 0 {
		t.Errorf("units left: %v", entries)
	}
}
