package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// publishMixin 写入一个 mixin 并发布。
func publishMixin(t *testing.T, i *Installer, content string) {
	t.Helper()
	if err := os.MkdirAll(i.Layout.MixinDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(i.Layout.MixinDir(), "10-api.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	p := i.pipeline(nil)
	plan, err := p.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
}

func TestPanel(t *testing.T) {
	ctx := context.Background()
	i, _, _ := newTunInstaller(t)
	st, err := state.Load(i.Layout.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	secret := st.APISecret

	p, err := i.Panel(ctx, PanelOptions{SSHConnection: "198.51.100.7 51234 203.0.113.10 2222", User: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if p.SSHCommand != "ssh -N -L 9090:127.0.0.1:9090 -p 2222 alice@203.0.113.10" || !p.SSHHostGuessed || p.Public {
		t.Errorf("ssh = %q (guessed %v, public %v)", p.SSHCommand, p.SSHHostGuessed, p.Public)
	}
	if want := "http://127.0.0.1:9090/ui/#/setup?hostname=127.0.0.1&port=9090&secret=" + secret; p.URL != want {
		t.Errorf("url = %q, want %q", p.URL, want)
	}
	if p.LocalForward != "LocalForward 9090 127.0.0.1:9090" || p.Secret != secret || p.UIInstalled || p.Running != "" {
		t.Errorf("panel = %+v", p)
	}

	// 不在 SSH 会话中；端口 22 不写 -p。
	p, _ = i.Panel(ctx, PanelOptions{})
	if p.SSHCommand != "ssh -N -L 9090:127.0.0.1:9090 <server>" || p.SSHHostGuessed {
		t.Errorf("no ssh: %q", p.SSHCommand)
	}
	p, _ = i.Panel(ctx, PanelOptions{SSHConnection: "198.51.100.7 51234 203.0.113.10 22"})
	if p.SSHCommand != "ssh -N -L 9090:127.0.0.1:9090 203.0.113.10" {
		t.Errorf("port 22: %q", p.SSHCommand)
	}

	// mixin 改了端口和 secret；secret 中的特殊字符在链接中转义。
	publishMixin(t, i, "external-controller: \"[::1]:9091\"\nsecret: \"a b&c\"\n")
	p, _ = i.Panel(ctx, PanelOptions{})
	if p.SSHCommand != "ssh -N -L 9091:[::1]:9091 <server>" || p.Port != 9091 {
		t.Errorf("ipv6: %q", p.SSHCommand)
	}
	if !strings.HasSuffix(p.URL, "port=9091&secret=a%20b%26c") {
		t.Errorf("escaped url = %q", p.URL)
	}

	// 对外监听：不需要隧道，用 SSH 连进来的地址。
	publishMixin(t, i, "external-controller: 0.0.0.0:9090\n")
	p, _ = i.Panel(ctx, PanelOptions{SSHConnection: "198.51.100.7 51234 203.0.113.10 22"})
	if !p.Public || p.SSHCommand != "" || !strings.HasPrefix(p.URL, "http://203.0.113.10:9090/ui/#/setup?hostname=203.0.113.10&port=9090") {
		t.Errorf("public: %+v", p)
	}

	// 面板关掉了。
	publishMixin(t, i, "external-controller: null\n")
	if _, err := i.Panel(ctx, PanelOptions{}); !errors.Is(err, core.ErrCheckFailed) {
		t.Errorf("no controller: %v", err)
	}
}

func TestPanelNothingPublished(t *testing.T) {
	i, _ := newDoctorInstaller(t)
	if _, err := i.Panel(context.Background(), PanelOptions{}); !errors.Is(err, core.ErrCheckFailed) {
		t.Fatalf("err = %v", err)
	}
}
