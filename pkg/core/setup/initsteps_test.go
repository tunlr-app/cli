package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/service"
)

// TestInitSteps 验证 init 前确认时列出的步骤随参数和现状变化，且只读取、不做改动。
func TestInitSteps(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	i := &Installer{Layout: l, Version: "test", Systemd: &service.Systemd{Layout: l}}
	mirror := "https://gh-proxy.com/"
	steps, err := i.InitSteps(context.Background(), InitOptions{Mirror: &mirror, SubName: "home", SubURL: "https://example.net/s"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(steps, "\n")
	for _, want := range []string{
		"Copy tunlr to " + l.ToolBinary(),
		"Download from GitHub through https://gh-proxy.com/",
		"Install mihomo (latest release)",
		"Install and enable tunlr-core.service and tunlr-update.timer (subscriptions daily unless set otherwise)",
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
}
