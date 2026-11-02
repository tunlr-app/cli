package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/setup"
)

// TestDNSChooses 验证终端中不带子命令的 dns：先选改什么（模式、IPv6 选中后再选取值）；fake-ip-filter 在文本框中编辑你自己的条目，
// 预先填好现在的，说明里给出配置的列表已有几条；取消时什么都不改。
func TestDNSChooses(t *testing.T) {
	et := newEditTest(t)
	addSubscriptions(t, et.a, "main", "main")
	et.run(0, "config", "apply", "--yes")

	et.prompt.picks = []string{"fake-ip-filter"}
	et.prompt.texts = []string{"nas.example.com\n\n+.lan\n printer.example.com \n"}
	et.run(0, "dns", "--yes")
	opts := et.prompt.asked[0]
	if len(opts) != 4 || opts[0] != "Mode: fake-ip or redir-host (now fake-ip)" || et.prompt.initials[0] != 0 {
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

	// 先选 Mode，再选模式（光标停在“按配置”上），走 dns mode <模式> 的流程。
	et.stdout.Reset()
	et.prompt.picks = []string{"Mode:", "redir-host"}
	et.run(0, "dns", "--yes")
	if !strings.Contains(et.stdout.String(), "mihomo answers DNS queries in redir-host mode now.") {
		t.Errorf("output:\n%s", et.stdout)
	}
	last := len(et.prompt.asked) - 1
	if opts := et.prompt.asked[last]; len(opts) != 3 || opts[2] != "Let the configuration decide (now: fake-ip)" || et.prompt.initials[last] != 2 {
		t.Errorf("options = %q, cursor %d", opts, et.prompt.initials[last])
	}

	// 不给模式的 dns mode 只选模式，光标停在 mixin 写定的那个上。
	et.stdout.Reset()
	et.prompt.picks = []string{"Let the configuration decide"}
	et.run(0, "dns", "mode", "--yes")
	last = len(et.prompt.asked) - 1
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

	// IPv6：先选这一项，再选流量（光标停在现在的 rules 上），再选回答（光标停在“按配置”上，tunlr 的兜底值为 off）。
	et.stdout.Reset()
	first := len(et.prompt.asked)
	et.prompt.picks = []string{"IPv6:", "By the rules", "On"}
	et.run(0, "dns", "--yes")
	if opt := et.prompt.asked[first][1]; opt != "IPv6: traffic and answers (now rules, answers off)" {
		t.Errorf("menu option %q", opt)
	}
	if opts := et.prompt.asked[first+1]; len(opts) != 2 || !strings.HasSuffix(opts[0], " (now)") || et.prompt.initials[first+1] != 0 {
		t.Errorf("traffic options = %q, cursor %d", opts, et.prompt.initials[first+1])
	}
	if opts := et.prompt.asked[first+2]; len(opts) != 3 || opts[2] != "Let the configuration decide (now: off)" || et.prompt.initials[first+2] != 2 {
		t.Errorf("answer options = %q, cursor %d", opts, et.prompt.initials[first+2])
	}
	if out := et.stdout.String(); !strings.Contains(out, "  90-dns.yaml  set dns.fake-ip-range6, dns.ipv6\n") ||
		!strings.Contains(out, "Names get IPv6 addresses too now.") {
		t.Errorf("output:\n%s", out)
	}
	cfg, _ = os.ReadFile(et.a.layout.CoreConfig())
	if !config.DNSIPv6(cfg) || !strings.Contains(string(cfg), "fake-ip-range6: "+setup.FakeIPRange6) {
		t.Errorf("published:\n%s", cfg)
	}

	// 选 bypass 时不问回答。
	et.stdout.Reset()
	first = len(et.prompt.asked)
	et.prompt.picks = []string{"IPv6:", "Bypass"}
	et.run(0, "dns", "--yes")
	if len(et.prompt.asked) != first+2 || !strings.Contains(et.stdout.String(), "IPv6 bypasses mihomo now.") {
		t.Errorf("bypass: asked %d, output:\n%s", len(et.prompt.asked)-first, et.stdout)
	}

	// 从 bypass 换回 rules，同时关掉回答：一份计划，只发布一次；回答的光标停在留着的 on 上。
	et.stdout.Reset()
	first = len(et.prompt.asked)
	et.prompt.picks = []string{"IPv6:", "By the rules", "Off"}
	et.run(0, "dns", "--yes")
	if opt := et.prompt.asked[first][1]; !strings.HasSuffix(opt, "(now bypass; answers on, not used)") {
		t.Errorf("menu option %q", opt)
	}
	if et.prompt.initials[first+1] != 1 || et.prompt.initials[first+2] != 1 {
		t.Errorf("cursors %v", et.prompt.initials[first:])
	}
	out = et.stdout.String()
	if strings.Count(out, "Published revision") != 1 || !strings.Contains(out, "  90-dns.yaml  set dns.ipv6; unset dns.fake-ip-range6\n") ||
		!strings.Contains(out, "IPv6 goes through mihomo again, by the rules. Names get IPv4 addresses only now.") {
		t.Errorf("output:\n%s", out)
	}
	cfg, _ = os.ReadFile(et.a.layout.CoreConfig())
	if config.DNSIPv6(cfg) || !config.TopIPv6(cfg) {
		t.Errorf("published:\n%s", cfg)
	}
}
