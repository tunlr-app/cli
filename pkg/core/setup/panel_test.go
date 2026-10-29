package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// editPublished 按 mixin 的语义直接改生效的配置，模拟工具之外（如手动、旧版本）改出的配置：
// 经 tunlr 发布时 mixin 改不了面板的键。
func editPublished(t *testing.T, i *Installer, mixin string) {
	t.Helper()
	cur, err := os.ReadFile(i.Layout.CoreConfig())
	if err != nil {
		t.Fatal(err)
	}
	res, err := config.Compose(config.Layers{Template: cur, Mixins: []config.Mixin{{Name: "edit.yaml", Data: []byte(mixin)}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(i.Layout.CoreConfig(), res.Config, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeMixin 写入一个 mixin 文件。
func writeMixin(t *testing.T, i *Installer, content string) {
	t.Helper()
	if err := os.MkdirAll(i.Layout.MixinDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(i.Layout.MixinDir(), "10-api.yaml"), []byte(content), 0o600); err != nil {
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

	p, err := i.Panel(ctx, PanelOptions{SSHConnection: "198.51.100.7 51234 203.0.113.10 2222", User: "alice", Label: "srv"})
	if err != nil {
		t.Fatal(err)
	}
	if p.SSHCommand != "ssh -N -L 9090:127.0.0.1:9090 -p 2222 alice@203.0.113.10" || !p.SSHHostGuessed || p.Public {
		t.Errorf("ssh = %q (guessed %v, public %v)", p.SSHCommand, p.SSHHostGuessed, p.Public)
	}
	// 经隧道的后端在名称上与局域网的区分开；空格编码为 %20。
	if want := "http://127.0.0.1:9090/ui/#/setup?hostname=127.0.0.1&label=srv%20%28SSH%20tunnel%29&port=9090&secret=" + secret; p.URL != want {
		t.Errorf("url = %q, want %q", p.URL, want)
	}
	if p.Bookmark != "http://127.0.0.1:9090/ui/" || p.Label != "srv" {
		t.Errorf("bookmark %q, label %q", p.Bookmark, p.Label)
	}
	// 没有给出名称时用主机名。
	if host, _ := os.Hostname(); host != "" {
		if p, _ := i.Panel(ctx, PanelOptions{}); p.Label != host {
			t.Errorf("default label = %q, want %q", p.Label, host)
		}
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

	// 配置中是别的端口和 secret；secret 中的特殊字符在链接中转义。
	editPublished(t, i, "mihomo:\n  external-controller: \"[::1]:9091\"\n  secret: \"a b&c\"\n")
	p, _ = i.Panel(ctx, PanelOptions{})
	if p.SSHCommand != "ssh -N -L 9091:[::1]:9091 <server>" || p.Port != 9091 {
		t.Errorf("ipv6: %q", p.SSHCommand)
	}
	if !strings.HasSuffix(p.URL, "port=9091&secret=a%20b%26c") {
		t.Errorf("escaped url = %q", p.URL)
	}

	// 对外监听：隧道经 127.0.0.1，另外给出默认路由网卡和 SSH 连进来的地址的链接。
	i.Interfaces = lanInterface
	editPublished(t, i, "mihomo:\n  external-controller: 0.0.0.0:9090\n")
	p, _ = i.Panel(ctx, PanelOptions{SSHConnection: "198.51.100.7 51234 203.0.113.10 22", Label: "srv"})
	if !p.Public || p.SSHCommand != "ssh -N -L 9090:127.0.0.1:9090 203.0.113.10" || p.LAN || len(p.LANURLs) != 2 ||
		!strings.HasPrefix(p.LANURLs[0], "http://192.168.1.20:9090/ui/#/setup?hostname=192.168.1.20&label=srv&port=9090") ||
		!strings.HasPrefix(p.LANURLs[1], "http://203.0.113.10:9090/") {
		t.Errorf("public: %+v", p)
	}
	if want := []string{"http://192.168.1.20:9090/ui/", "http://203.0.113.10:9090/ui/"}; !slices.Equal(p.LANBookmarks, want) {
		t.Errorf("bookmarks = %q", p.LANBookmarks)
	}

	// 面板关掉了。
	editPublished(t, i, "mihomo:\n  external-controller: null\n")
	if _, err := i.Panel(ctx, PanelOptions{}); !errors.Is(err, core.ErrCheckFailed) {
		t.Errorf("no controller: %v", err)
	}
}

// installUnit 写一个内核服务的 unit，让安装看起来已经完成。
func installUnit(t *testing.T, i *Installer) {
	t.Helper()
	if err := os.MkdirAll(i.Layout.UnitDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(i.Layout.UnitDir(), service.CoreUnit), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func lanInterface(name string) (*netfilter.Interface, error) {
	return &netfilter.Interface{Name: name, Up: true, IPv4: []string{"192.168.1.20"}}, nil
}

func TestPanelLAN(t *testing.T) {
	ctx := context.Background()
	i, h, r := newTunInstaller(t)
	i.Interfaces = lanInterface
	installUnit(t, i)

	pre, err := i.PanelLANPreflight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pre.On || pre.Interface != "ens3" || len(pre.Addresses) != 1 || len(pre.PublicAddresses) != 0 {
		t.Errorf("preflight = %+v", pre)
	}

	plan, err := i.pipeline(nil).PlanPanelLAN(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.Restart, "external-controller") || len(plan.Warnings) != 0 {
		t.Errorf("restart %q, warnings %v", plan.Restart, plan.Warnings)
	}
	if _, err := i.PanelLANOn(ctx, plan); err != nil {
		t.Fatal(err)
	}
	// 表先于发布加载。
	up := slices.IndexFunc(h.calls, func(c string) bool { return c == "nft -f -" })
	restart := slices.IndexFunc(h.calls, func(c string) bool { return strings.Contains(c, "restart") })
	if up < 0 || (restart >= 0 && restart < up) || !slices.Contains(h.tables, netfilter.PanelTable) {
		t.Errorf("calls = %q, tables %v", h.calls, h.tables)
	}
	if st := tunSwitch(t, i); st.PanelLAN == nil {
		t.Error("switch not saved")
	}
	p, err := i.Panel(ctx, PanelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.LAN || !p.LANRules || !p.Public || p.Controller != "0.0.0.0:9090" || len(p.LANURLs) != 1 {
		t.Errorf("panel = %+v", p)
	}
	if pre, _ := i.PanelLANPreflight(ctx); !pre.On {
		t.Error("preflight does not see the switch")
	}

	// _prestart 补上丢失的表。
	h.tables = slices.DeleteFunc(h.tables, func(t string) bool { return t == netfilter.PanelTable })
	if err := i.PreStart(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.tables, netfilter.PanelTable) {
		t.Errorf("not repaired: %v", h.tables)
	}

	// mixin 设置了 external-controller 时拒绝，提示改用的命令。
	writeMixin(t, i, "mihomo:\n  external-controller: 127.0.0.1:9091\n")
	var me *config.MixinError
	if _, err := i.pipeline(nil).PlanPanelLAN(ctx, false); !errors.As(err, &me) || !strings.Contains(err.Error(), "tunlr panel lan on/off") {
		t.Errorf("mixin: %v", err)
	}
	if err := os.Remove(filepath.Join(i.Layout.MixinDir(), "10-api.yaml")); err != nil {
		t.Fatal(err)
	}

	plan, err = i.pipeline(nil).PlanPanelLAN(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	r.loaded = nil
	if _, err := i.PanelLANOff(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if st := tunSwitch(t, i); st.PanelLAN != nil || slices.Contains(h.tables, netfilter.PanelTable) {
		t.Errorf("switch %v, tables %v", st.PanelLAN, h.tables)
	}
	if p, _ := i.Panel(ctx, PanelOptions{}); p.LAN || p.Public || p.Controller != "127.0.0.1:9090" {
		t.Errorf("after off: %+v", p)
	}
}

func TestPanelLANPublicAddress(t *testing.T) {
	i, _, _ := newTunInstaller(t)
	installUnit(t, i)
	i.Interfaces = func(name string) (*netfilter.Interface, error) {
		return &netfilter.Interface{Name: name, Up: true, IPv4: []string{"203.0.113.10", "10.0.0.5"}}, nil
	}
	pre, err := i.PanelLANPreflight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pre.PublicAddresses, []string{"203.0.113.10"}) {
		t.Errorf("public = %v", pre.PublicAddresses)
	}
}

func TestPanelNothingPublished(t *testing.T) {
	i, _ := newDoctorInstaller(t)
	if _, err := i.Panel(context.Background(), PanelOptions{}); !errors.Is(err, core.ErrCheckFailed) {
		t.Fatalf("err = %v", err)
	}
}
