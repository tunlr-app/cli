package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/service"
)

// TestInitSteps 验证 init 前确认时列出的步骤随参数和现状变化，且只读取、不做改动。
func TestInitSteps(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	i := &Installer{Layout: l, Version: "test", Systemd: &service.Systemd{Layout: l}}
	mirror := "https://axisnow.gh-proxy.org/"
	steps, err := i.InitSteps(context.Background(), InitOptions{Mirror: &mirror, SubName: "home", SubURL: "https://example.net/s"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(steps, "\n")
	for _, want := range []string{
		"Copy tunlr to " + l.ToolBinary(),
		"Download from GitHub through https://axisnow.gh-proxy.org/",
		"Install mihomo (latest release)",
		"Install and enable tunlr-core.service, tunlr-update.timer (subscriptions daily unless set otherwise), tunlr-aftercare.path (checks mihomo after the panel upgrades it) " +
			"and tunlr-firewall.service (reloads tunlr's nftables rules after nftables.service)",
		`Add the subscription "home"`,
		`Publish the configuration from "home"`,
		"Start mihomo",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("steps lack %q:\n%s", want, got)
		}
	}

	// 没有订阅、也没有发布过配置时不发布、不启动；内核版本照参数。
	steps, err = i.InitSteps(context.Background(), InitOptions{KernelVersion: "v1.19.33"})
	if err != nil {
		t.Fatal(err)
	}
	got = strings.Join(steps, "\n")
	if !strings.Contains(got, "Install mihomo v1.19.33") || strings.Contains(got, "Publish") || strings.Contains(got, "mihomo (") {
		t.Errorf("steps:\n%s", got)
	}
	if strings.Contains(got, "Start mihomo") || strings.Contains(got, "GitHub") {
		t.Errorf("steps:\n%s", got)
	}

	// 离线安装：版本来自文件名，名称不对时拒绝。
	steps, err = i.InitSteps(context.Background(), InitOptions{KernelFile: "/tmp/mihomo-linux-amd64-v3-v1.19.32.gz"})
	if err != nil || !strings.Contains(strings.Join(steps, "\n"), "Install mihomo v1.19.32 from /tmp/mihomo-linux-amd64-v3-v1.19.32.gz, verified against the official sha256") {
		t.Errorf("steps = %q, %v", steps, err)
	}
	if _, err = i.InitSteps(context.Background(), InitOptions{KernelFile: "/tmp/mihomo.gz"}); err == nil || !strings.Contains(err.Error(), "is not a mihomo release asset") {
		t.Errorf("bad file name: err = %v", err)
	}
}

// TestInitStepsKeepsMihomoRunning 验证内核在运行、已经装好、它的 unit 没变时（如升级 tunlr 之后）init 不重启它；
// unit 变了时要重启。
func TestInitStepsKeepsMihomoRunning(t *testing.T) {
	ctx := context.Background()
	i, h, _ := newTunInstaller(t)
	h.active = true
	if _, err := i.systemd().Install(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(i.Layout.CoreBinary()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(i.Layout.CoreBinary(), []byte("mihomo"), 0o755); err != nil {
		t.Fatal(err)
	}
	last := func() string {
		t.Helper()
		steps, err := i.InitSteps(ctx, InitOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return steps[len(steps)-1]
	}
	if got := last(); !strings.HasPrefix(got, "Keep mihomo running (its service unit is unchanged)") {
		t.Errorf("unit unchanged: %q", got)
	}
	if err := os.WriteFile(filepath.Join(i.Layout.UnitDir(), service.CoreUnit), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := last(); got != "Restart mihomo (open connections drop)" {
		t.Errorf("unit changed: %q", got)
	}
}
