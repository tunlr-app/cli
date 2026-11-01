package cli

import (
	"os"
	"strings"
	"testing"
)

// TestDevicesTable 验证终端中的 tunlr devices：没有设备可用本机时只说明原因；有记录的设备时在表格中选一台，再选
// 起名、改走法、忘记，做完回到刷新过的表格，Esc 结束；a 输入地址指定还没出现的设备。
func TestDevicesTable(t *testing.T) {
	et := newEditTest(t)
	addSubscriptions(t, et.a, "main", "main")
	et.run(0, "config", "apply", "--yes")

	et.run(0, "devices")
	if out := et.stdout.String(); !strings.Contains(out, "No LAN device can use this server") || len(et.prompt.asked) != 0 {
		t.Fatalf("asked %q, output:\n%s", et.prompt.asked, out)
	}

	// 预先设为直连的设备出现在表格中，有 ROUTE 列；起名后回到表格，名称已在其中。
	et.run(0, "devices", "route", "192.168.1.50", "direct", "--yes")
	et.stdout.Reset()
	et.prompt.picks = []string{"192.168.1.50", "Name it", ""}
	et.prompt.inputs = []string{"tv"}
	et.run(0, "devices", "--yes")
	if got := et.prompt.asked[0]; len(got) != 1 || got[0] != "-  192.168.1.50  -  away  -  0  direct" {
		t.Errorf("rows %q", got)
	}
	if got := et.prompt.asked[1]; len(got) != 4 || got[1] != "Follow the rules again (now direct for everything)" ||
		got[2] != "Forget it: drop its name and setting" {
		t.Errorf("actions %q", got)
	}
	if got := et.prompt.asked[2]; got[0] != "tv  192.168.1.50  -  away  -  0  direct" {
		t.Errorf("rows after naming %q", got)
	}
	if out := et.stdout.String(); !strings.Contains(out, "192.168.1.50 is now called tv.") {
		t.Errorf("output:\n%s", out)
	}

	// a：按地址指定还没出现的设备，设为直连；名称重复时说明原因，回到表格。
	et.stdout.Reset()
	et.prompt.picks = []string{"key:a", "Go direct", "tv", "Rename it", ""}
	et.prompt.inputs = []string{"192.168.1.60", "TV2"}
	et.run(0, "devices", "--yes")
	et.prompt.picks = []string{"192.168.1.60", "Name it", ""}
	et.prompt.inputs = []string{"tv2"}
	et.run(0, "devices", "--yes")
	out := et.stdout.String()
	if !strings.Contains(out, "192.168.1.60 now goes direct for everything.") || !strings.Contains(out, "192.168.1.50 is now called TV2.") ||
		!strings.Contains(out, `Not changed: invalid argument: another device is already called "TV2"`) {
		t.Errorf("output:\n%s", out)
	}

	// 改回按规则、忘记：没有记录的设备不再列出；什么都不剩时说明原因。
	et.stdout.Reset()
	et.prompt.picks = []string{"192.168.1.60", "Follow the rules again", "TV2", "Forget it"}
	et.run(0, "devices", "--yes")
	if out := et.stdout.String(); !strings.Contains(out, "192.168.1.60 follows the rules again.") ||
		!strings.Contains(out, "Forgot TV2 (192.168.1.50).") || !strings.HasSuffix(out, "No LAN device can use this server: gateway mode and LAN access to the proxy port are both off\n"+
		"(tunlr tun gateway on, tunlr proxy lan on).\n") {
		t.Errorf("output:\n%s", out)
	}
	if data, _ := os.ReadFile(et.a.layout.StateFile()); strings.Contains(string(data), `"devices"`) {
		t.Errorf("state:\n%s", data)
	}
}
