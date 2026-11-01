package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/config"
)

// TestDNSChooses 验证终端中不带子命令的 dns：先选改什么；fake-ip-filter 在文本框中编辑你自己的条目，
// 预先填好现在的，说明里给出配置的列表已有几条；取消时什么都不改。
func TestDNSChooses(t *testing.T) {
	et := newEditTest(t)
	addSubscriptions(t, et.a, "main", "main")
	et.run(0, "config", "apply", "--yes")

	et.prompt.picks = []string{"fake-ip-filter"}
	et.prompt.texts = []string{"nas.example.com\n\n+.lan\n printer.example.com \n"}
	et.run(0, "dns", "--yes")
	opts := et.prompt.asked[0]
	if len(opts) != 5 || opts[2] != "Let the configuration decide (now: fake-ip)" || et.prompt.initials[0] != 2 {
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
	et.run(0, "dns", "fake-ip-filter")
	_, desc, _ := strings.Cut(et.prompt.textShown[0], "|")
	if et.prompt.textShown[1] != "nas.example.com\nprinter.example.com|"+desc ||
		!strings.Contains(et.stdout.String(), "Cancelled; nothing changed.") {
		t.Errorf("prefilled %q, output:\n%s", et.prompt.textShown[1], et.stdout)
	}

	// 配置的列表里的条目不是你的，--remove 拒绝并指向 tunlr.fake-ip-filter.delete。
	et.stdout.Reset()
	et.run(exitUsage, "dns", "fake-ip-filter", "--remove", "+.lan", "--yes")
	if !strings.Contains(et.stdout.String(), `"+.lan" comes from the configuration's list, not from your names; drop it with tunlr.fake-ip-filter.delete`) {
		t.Errorf("output:\n%s", et.stdout)
	}

	// 选模式时走 dns mode <模式> 的流程。
	et.stdout.Reset()
	et.prompt.picks = []string{"redir-host"}
	et.run(0, "dns", "--yes")
	if !strings.Contains(et.stdout.String(), "mihomo answers DNS queries in redir-host mode now.") {
		t.Errorf("output:\n%s", et.stdout)
	}
	if opts := et.prompt.asked[len(et.prompt.asked)-1]; opts[1] != "redir-host: answer with real addresses" {
		t.Errorf("options = %q", opts)
	}

	// 不给模式的 dns mode 只选模式，光标停在 mixin 写定的那个上。
	et.stdout.Reset()
	et.prompt.picks = []string{"Let the configuration decide"}
	et.run(0, "dns", "mode", "--yes")
	last := len(et.prompt.asked) - 1
	if opts := et.prompt.asked[last]; len(opts) != 3 || opts[1] != "redir-host: answer with real addresses (now)" || et.prompt.initials[last] != 1 {
		t.Errorf("options = %q, cursor %d", opts, et.prompt.initials[last])
	}
	if !strings.Contains(et.stdout.String(), "The configuration decides the DNS mode again.") {
		t.Errorf("output:\n%s", et.stdout)
	}

	// 问谁：两个文本框预先填好现在用的（tunlr 的默认值）；不改的那个不写。
	et.stdout.Reset()
	et.prompt.picks = []string{"Servers"}
	et.prompt.texts = []string{keepText, "tls://8.8.8.8\n\n system \n"}
	et.run(0, "dns", "--yes")
	china, other := et.prompt.textShown[2], et.prompt.textShown[3]
	if !strings.HasPrefix(china, "https://223.5.5.5/dns-query\n") || !strings.Contains(china, "|Asked directly") ||
		!strings.HasPrefix(other, "https://1.1.1.1/dns-query\n") {
		t.Errorf("text boxes: %q, %q", china, other)
	}
	if out := et.stdout.String(); !strings.Contains(out, "  90-dns.yaml  set dns.nameserver\n") ||
		!strings.Contains(out, "The DNS servers are changed.") {
		t.Errorf("output:\n%s", out)
	}
	cfg, _ = os.ReadFile(et.a.layout.CoreConfig())
	china2, other2 := config.DNSServers(cfg)
	if strings.Join(other2, " ") != "tls://8.8.8.8 system" || !strings.HasPrefix(china2[0], "https://223.5.5.5/dns-query#DIRECT") {
		t.Errorf("published: %q, %q", china2, other2)
	}

	// 清空一个框：删除你的设置，回到 tunlr 的默认值。
	et.stdout.Reset()
	et.prompt.texts = []string{keepText, "\n"}
	et.run(0, "dns", "servers", "--yes")
	if !strings.Contains(et.prompt.textShown[5], "tls://8.8.8.8\nsystem|") {
		t.Errorf("prefilled: %q", et.prompt.textShown[5])
	}
	cfg, _ = os.ReadFile(et.a.layout.CoreConfig())
	if _, other := config.DNSServers(cfg); other[0] != "https://1.1.1.1/dns-query" {
		t.Errorf("after emptying: %q\n%s", other, et.stdout)
	}
}
