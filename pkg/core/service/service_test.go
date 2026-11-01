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

func TestTailWithoutConnections(t *testing.T) {
	log := "a\n" +
		"time=1 level=info msg=\"[TCP] 10.0.0.2:1 --> x:443 match DIRECT\"\n" +
		"b\n" +
		"time=2 level=info msg=\"[UDP] 10.0.0.2:2 --> y:53 match DIRECT\"\n" +
		"\n" +
		"c\n"
	if got := tailWithoutConnections(log, 2); got != "  b\n  c" {
		t.Errorf("tail = %q", got)
	}
	if got := tailWithoutConnections(log, 10); got != "  a\n  b\n  c" {
		t.Errorf("all = %q", got)
	}
	if got := tailWithoutConnections("", 5); got != "" {
		t.Errorf("empty = %q", got)
	}
}

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
		"Environment=SKIP_SYSTEM_IPV6_CHECK=1\n",
		"ReadWritePaths=/var/lib/tunlr/bin /var/lib/tunlr/core\n",
		"OnFailure=" + RescueUnit + "\n",
		"ExecStartPre=-+/usr/local/bin/tunlr _prestart\n",
		"ExecStopPost=-+/usr/local/bin/tunlr _poststop\n",
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
	if !strings.Contains(string(units[CheckUnit]), "ExecStart=/usr/local/bin/tunlr _check\n") {
		t.Errorf("%s:\n%s", CheckUnit, units[CheckUnit])
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
	changed, err := s.Install(ctx)
	if err != nil || !slices.Equal(changed, Units) {
		t.Fatalf("changed %v, %v", changed, err)
	}
	// 再次安装时内容没有变化；只改了一个 unit 时只报告它。
	if changed, err := s.Install(ctx); err != nil || len(changed) != 0 {
		t.Errorf("reinstall: changed %v, %v", changed, err)
	}
	if err := os.WriteFile(filepath.Join(s.Layout.UnitDir(), CoreUnit), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.Install(ctx); err != nil || !slices.Equal(changed, []string{CoreUnit}) {
		t.Errorf("after editing %s: changed %v, %v", CoreUnit, changed, err)
	}
	f.calls = f.calls[:2]
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
