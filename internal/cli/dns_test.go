package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/config"
)

// TestTunDNSChooses 验证终端中不带子命令的 tun dns：先选改什么；fake-ip-filter 在文本框中编辑你自己的条目，
// 预先填好现在的，说明里给出配置的列表已有几条；取消时什么都不改。
func TestTunDNSChooses(t *testing.T) {
	et := newEditTest(t)
	addSubscriptions(t, et.a, "main", "main")
	et.run(0, "config", "apply", "--yes")

	et.prompt.picks = []string{"fake-ip-filter"}
	et.prompt.texts = []string{"nas.example.com\n\n+.lan\n printer.example.com \n"}
	et.run(0, "tun", "dns", "--yes")
	opts := et.prompt.asked[0]
	if len(opts) != 4 || opts[2] != "Let the configuration decide (now)" || et.prompt.initials[0] != 2 {
		t.Errorf("options = %q, cursor %d", opts, et.prompt.initials[0])
	}
	if shown := et.prompt.textShown[0]; !strings.HasPrefix(shown, "|Only your own names; the configuration's list already has ") {
		t.Errorf("text box: %q", shown)
	}
	out := et.stdout.String()
	if !strings.Contains(out, "Already in the configuration's list, not added: +.lan") ||
		!strings.Contains(out, "Your names in fake-ip-filter: nas.example.com, printer.example.com.") {
		t.Errorf("output:\n%s", out)
	}
	cfg, _ := os.ReadFile(et.a.layout.CoreConfig())
	if got := strings.Join(config.FakeIPFilter(cfg), " "); !strings.HasSuffix(got, " nas.example.com printer.example.com") {
		t.Errorf("published: %s", got)
	}

	// 再打开时预先填好你的条目；取消时什么都不改。
	et.stdout.Reset()
	et.prompt.texts = []string{cancelText}
	et.run(0, "tun", "dns", "fake-ip-filter")
	_, desc, _ := strings.Cut(et.prompt.textShown[0], "|")
	if et.prompt.textShown[1] != "nas.example.com\nprinter.example.com|"+desc ||
		!strings.Contains(et.stdout.String(), "Cancelled; nothing changed.") {
		t.Errorf("prefilled %q, output:\n%s", et.prompt.textShown[1], et.stdout)
	}

	// 配置的列表里的条目不是你的，--remove 拒绝并指向 tunlr.fake-ip-filter.delete。
	et.stdout.Reset()
	et.run(exitUsage, "tun", "dns", "fake-ip-filter", "--remove", "+.lan", "--yes")
	if !strings.Contains(et.stdout.String(), `"+.lan" comes from the configuration's list, not from your names; drop it with tunlr.fake-ip-filter.delete`) {
		t.Errorf("output:\n%s", et.stdout)
	}

	// 选模式时走 tun dns <模式> 的流程。
	et.stdout.Reset()
	et.prompt.picks = []string{"redir-host"}
	et.run(0, "tun", "dns", "--yes")
	if !strings.Contains(et.stdout.String(), "mihomo answers DNS queries in redir-host mode now.") {
		t.Errorf("output:\n%s", et.stdout)
	}
}
