package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
)

// fakeHost 是一台假的机器：files 是可读的文件（目录的值为 nil），cmds 是命令的输出（键为命令行），
// missing 是找不到的命令。
type fakeHost struct {
	euid    int
	files   map[string][]byte
	cmds    map[string]string
	missing []string
}

type fakeInfo struct {
	fs.FileInfo
	dir bool
}

func (f fakeInfo) IsDir() bool { return f.dir }

func (f *fakeHost) host(arch string) *Host {
	return &Host{
		GOOS: "linux", GOARCH: arch,
		Euid: func() int { return f.euid },
		ReadFile: func(name string) ([]byte, error) {
			if data, ok := f.files[name]; ok && data != nil {
				return data, nil
			}
			return nil, fs.ErrNotExist
		},
		Stat: func(name string) (fs.FileInfo, error) {
			data, ok := f.files[name]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return fakeInfo{dir: data == nil}, nil
		},
		LookPath: func(file string) (string, error) {
			for _, m := range f.missing {
				if m == file {
					return "", errors.New("not found")
				}
			}
			return "/usr/bin/" + file, nil
		},
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			line := strings.TrimSpace(name + " " + strings.Join(args, " "))
			out, ok := f.cmds[line]
			if !ok {
				return nil, errors.New(name + ": not found")
			}
			return []byte(out), nil
		},
	}
}

const debian12 = `PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
NAME="Debian GNU/Linux"
VERSION_ID="12"
ID=debian
`

// cpuV3 是 x86-64-v3 的 CPU 标志。
const cpuV3 = "flags\t\t: fpu sse sse2 pni cx16 lahf_lm popcnt sse4_1 sse4_2 ssse3 avx avx2 bmi1 bmi2 f16c fma abm movbe xsave\n"

func healthy() *fakeHost {
	return &fakeHost{
		files: map[string][]byte{
			"/run/systemd/system":        nil,
			"/proc/cpuinfo":              []byte(cpuV3),
			"/proc/sys/kernel/osrelease": []byte("6.1.0-26-amd64\n"),
			"/etc/os-release":            []byte(debian12),
			"/dev/net/tun":               {},
		},
		cmds: map[string]string{"systemd-detect-virt --container": "none\n"},
	}
}

func statuses(checks []Check) map[string]string {
	m := map[string]string{}
	for _, c := range checks {
		m[c.ID] = c.Status
	}
	return m
}

func TestSupportHealthy(t *testing.T) {
	checks := healthy().host("amd64").Support(context.Background())
	for _, c := range checks {
		if c.Status != OK {
			t.Errorf("%s = %s: %s", c.ID, c.Status, c.Message)
		}
		if c.Group != GroupSystem || !strings.HasPrefix(c.ID, "system.") {
			t.Errorf("%s in group %s", c.ID, c.Group)
		}
	}
	want := []string{"system.os", "system.root", "system.systemd", "system.arch", "system.cpu-level", "system.wsl", "system.container", "system.distro"}
	if len(checks) != len(want) {
		t.Fatalf("%d checks, want %d", len(checks), len(want))
	}
	for i, id := range want {
		if checks[i].ID != id {
			t.Errorf("check %d = %s, want %s", i, checks[i].ID, id)
		}
	}
	if _, err := Failures(checks); err != nil {
		t.Errorf("Failures: %v", err)
	}
}

func TestSupportProblems(t *testing.T) {
	f := healthy()
	f.euid = 1000
	f.files["/proc/cpuinfo"] = []byte("flags\t\t: fpu sse sse2\n")
	f.files["/proc/sys/kernel/osrelease"] = []byte("5.15.153.1-microsoft-standard-WSL2\n")
	f.files["/etc/os-release"] = []byte("ID=arch\nPRETTY_NAME=\"Arch Linux\"\n")
	delete(f.files, "/run/systemd/system")
	f.cmds["systemd-detect-virt --container"] = "lxc\n"

	checks := f.host("amd64").Support(context.Background())
	got := statuses(checks)
	for id, want := range map[string]string{
		"system.root": Fail, "system.systemd": Fail, "system.cpu-level": Warn, "system.wsl": Fail,
		"system.container": Fail, "system.distro": Warn, "system.arch": OK,
	} {
		if got[id] != want {
			t.Errorf("%s = %s, want %s", id, got[id], want)
		}
	}
	warnings, err := Failures(checks)
	if !errors.Is(err, core.ErrCheckFailed) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "WSL") || !strings.Contains(err.Error(), "(run tunlr as root (sudo))") {
		t.Errorf("err = %v", err)
	}
	if len(warnings) != 2 {
		t.Errorf("warnings = %q", warnings)
	}
}

func TestSupportOther(t *testing.T) {
	h := healthy()
	if got := statuses(h.host("386").Support(context.Background()))["system.arch"]; got != Fail {
		t.Errorf("386: %s", got)
	}
	// arm64 没有 CPU 级别一项。
	if _, ok := statuses(h.host("arm64").Support(context.Background()))["system.cpu-level"]; ok {
		t.Error("arm64 has a cpu-level check")
	}
	// 非 Linux 只有一项。
	host := h.host("amd64")
	host.GOOS = "windows"
	if checks := host.Support(context.Background()); len(checks) != 1 || checks[0].Status != Fail {
		t.Errorf("windows: %+v", checks)
	}
	// 没有 systemd-detect-virt 时看标记文件。
	delete(h.cmds, "systemd-detect-virt --container")
	h.files["/.dockerenv"] = []byte{}
	if got := statuses(h.host("amd64").Support(context.Background()))["system.container"]; got != Fail {
		t.Errorf("docker: %s", got)
	}
	// Ubuntu 20.04 太旧，只警告。
	h.files["/etc/os-release"] = []byte("ID=ubuntu\nVERSION_ID=\"20.04\"\n")
	if got := statuses(h.host("amd64").Support(context.Background()))["system.distro"]; got != Warn {
		t.Errorf("ubuntu 20.04: %s", got)
	}
}

func TestParseOSRelease(t *testing.T) {
	rel := parseOSRelease([]byte("# comment\nID=ubuntu\nVERSION_ID=\"22.04\"\nNAME='Ubuntu'\nBAD\n"))
	if rel["ID"] != "ubuntu" || rel["VERSION_ID"] != "22.04" || rel["NAME"] != "Ubuntu" || len(rel) != 3 {
		t.Errorf("rel = %v", rel)
	}
}

// fakeRules 返回假的 nft 与 ip 输出：没有其他表。
func fakeRules(ipRules string) *netfilter.Rules {
	return fakeRuleset(ipRules, `{"nftables": []}`)
}

// fakeRuleset 同 fakeRules，nft -j list ruleset 的输出为 ruleset。
func fakeRuleset(ipRules, ruleset string) *netfilter.Rules {
	return &netfilter.Rules{
		IPv6: func() bool { return false },
		Run: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			switch line := name + " " + strings.Join(args, " "); {
			case strings.HasPrefix(line, "ip -4 rule"), strings.HasPrefix(line, "ip rule"):
				return []byte(ipRules), nil
			case line == "nft -j list ruleset":
				return []byte(ruleset), nil
			}
			return nil, nil
		},
	}
}

// TestTunFirewall 验证默认丢弃入站的链警告（UFW），先放行 DNAT 连接的不警告（firewalld）。
func TestTunFirewall(t *testing.T) {
	f := healthy()
	firewall := func(ruleset string) Check {
		for _, c := range f.host("amd64").Tun(context.Background(), fakeRuleset("", ruleset)) {
			if c.ID == "tun.firewall" {
				return c
			}
		}
		t.Fatal("no firewall check")
		return Check{}
	}
	ufw := `{"nftables": [{"chain": {"family": "ip", "table": "filter", "name": "INPUT", "hook": "input", "prio": 0, "policy": "drop"}},
{"chain": {"family": "ip", "table": "filter", "name": "ufw-before-input"}}]}`
	if c := firewall(ufw); c.Status != Warn || !strings.HasPrefix(c.Message, "ip filter INPUT (UFW) drops incoming connections by default") ||
		c.Hint != "to let it through: ufw allow in on <interface>" {
		t.Errorf("ufw: %+v", c)
	}
	firewalld := `{"nftables": [{"chain": {"family": "inet", "table": "firewalld", "name": "filter_INPUT", "hook": "input", "prio": 10, "policy": "accept"}},
{"rule": {"family": "inet", "table": "firewalld", "chain": "filter_INPUT", "expr": [{"match": {"op": "in", "left": {"ct": {"key": "status"}}, "right": "dnat"}}, {"accept": null}]}},
{"rule": {"family": "inet", "table": "firewalld", "chain": "filter_INPUT", "expr": [{"reject": {"type": "icmpx", "expr": "admin-prohibited"}}]}}]}`
	if c := firewall(firewalld); c.Status != OK {
		t.Errorf("firewalld: %+v", c)
	}
	if c := firewall("not json"); c.Status != Skip {
		t.Errorf("bad output: %+v", c)
	}
}

func TestTun(t *testing.T) {
	f := healthy()
	checks := f.host("amd64").Tun(context.Background(), fakeRules(""))
	for _, c := range checks {
		if c.Status != OK {
			t.Errorf("%s = %s: %s", c.ID, c.Status, c.Message)
		}
	}

	delete(f.files, "/dev/net/tun")
	f.missing = []string{"nft"}
	got := statuses(f.host("amd64").Tun(context.Background(), fakeRules("")))
	if got["tun.device"] != Fail || got["tun.nft"] != Fail || got["tun.ip"] != OK || got["tun.ip-rules"] != Skip {
		t.Errorf("statuses = %v", got)
	}

	// 他人的 ip rule 占用了需要的优先级。
	f = healthy()
	conflict := fakeRules(fmt.Sprintf("0:\tfrom all lookup local\n%d:\tfrom all lookup 200\n", netfilter.RulePriority))
	if got := statuses(f.host("amd64").Tun(context.Background(), conflict)); got["tun.ip-rules"] != Fail {
		t.Errorf("conflict: %v", got)
	}

	// 非 root：需要 root 的项跳过。
	f = healthy()
	f.euid = 1000
	got = statuses(f.host("amd64").Tun(context.Background(), fakeRules("")))
	if got["tun.ip-rules"] != Skip || got["tun.firewall"] != Skip || got["tun.iptables-legacy"] != Skip {
		t.Errorf("non-root: %v", got)
	}
}

func TestTunLegacyIptables(t *testing.T) {
	f := healthy()
	f.cmds["iptables-legacy-save"] = "*filter\n:INPUT ACCEPT [0:0]\n-A INPUT -p tcp --dport 22 -j ACCEPT\n-A INPUT -j DROP\nCOMMIT\n"
	f.cmds["ip6tables-legacy-save"] = "*filter\nCOMMIT\n"
	var legacy Check
	for _, c := range f.host("amd64").Tun(context.Background(), fakeRules("")) {
		if c.ID == "tun.iptables-legacy" {
			legacy = c
		}
	}
	if legacy.Status != Warn || !strings.HasPrefix(legacy.Message, "2 rules") || legacy.Hint == "" {
		t.Errorf("legacy = %+v", legacy)
	}
}

func TestParseSS(t *testing.T) {
	out := `tcp   LISTEN 0      4096       127.0.0.1:9090      0.0.0.0:*    users:(("mihomo",pid=812,fd=9))
tcp   LISTEN 0      4096               *:7890            *:*    users:(("mihomo",pid=812,fd=7),("mihomo",pid=812,fd=8))
udp   UNCONN 0      0            0.0.0.0:1053      0.0.0.0:*    users:(("dnsmasq",pid=90,fd=4))
tcp   LISTEN 0      128             [::]:22             [::]:*    users:(("sshd",pid=1,fd=3))
tcp   LISTEN 0      128          0.0.0.0:8080      0.0.0.0:*
`
	got := parseSS([]byte(out))
	for key, want := range map[string]string{
		"tcp/9090": "mihomo", "tcp/7890": "mihomo", "udp/1053": "dnsmasq", "tcp/22": "sshd", "tcp/8080": "",
	} {
		procs, ok := got[key]
		if !ok || strings.Join(procs, ",") != want {
			t.Errorf("%s = %q (%v), want %q", key, procs, ok, want)
		}
	}
}

func TestCheckerAndReport(t *testing.T) {
	c := &Checker{Group: GroupInstall}
	c.Add("a", OK, "A", "fine", "ignored")
	c.Add("b", Warn, "B", "meh", "do this")
	c.Add("c", Fail, "C", "bad", "do that")
	if c.List[0].Hint != "" || c.List[1].Hint != "do this" || c.List[2].ID != "install.c" {
		t.Errorf("list = %+v", c.List)
	}
	r := &Report{Checks: c.List}
	if r.Count(Warn) != 1 || r.Count(Fail) != 1 {
		t.Errorf("counts %d %d", r.Count(Warn), r.Count(Fail))
	}
	if err := r.Err(); !errors.Is(err, core.ErrCheckFailed) || err.Error() != core.ErrCheckFailed.Error()+": 1 of 3 checks failed" {
		t.Errorf("err = %v", err)
	}
	if err := (&Report{Checks: c.List[:2]}).Err(); err != nil {
		t.Errorf("warnings only: %v", err)
	}
}

func TestDefaultHost(t *testing.T) {
	// 默认的 Host 使用真实的系统，只确认不会出错；不运行任何命令。
	h := &Host{}
	if h.goos() == "" || h.goarch() == "" {
		t.Error("empty GOOS or GOARCH")
	}
	if _, err := h.stat(os.TempDir()); err != nil {
		t.Error(err)
	}
}
