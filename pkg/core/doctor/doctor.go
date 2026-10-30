package doctor

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
)

// 检查的结果。
const (
	OK   = "ok"
	Warn = "warn"
	Fail = "fail"
	Skip = "skip"
)

// 检查的分组，按输出顺序。
const (
	GroupSystem  = "system"
	GroupTun     = "tun"
	GroupInstall = "install"
	GroupNetwork = "network"
)

// Groups 是全部分组，按输出顺序。
var Groups = []string{GroupSystem, GroupTun, GroupInstall, GroupNetwork}

// Check 是一项检查的结果。
type Check struct {
	// ID 形如 "system.wsl"，固定不变，供脚本判断。
	ID     string `json:"id"`
	Group  string `json:"group"`
	Status string `json:"status"`
	// Name 是给人看的名称，Message 是结果的说明。
	Name    string `json:"name"`
	Message string `json:"message,omitempty"`
	// Hint 是修复建议，只在 warn、fail 时给出。
	Hint string `json:"hint,omitempty"`
}

// Report 是一次诊断的全部结果，按 Groups 的顺序。
type Report struct {
	Checks []Check `json:"checks"`
}

// Count 返回结果为 status 的项数。
func (r *Report) Count(status string) int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == status {
			n++
		}
	}
	return n
}

// Err 在有 fail 时返回满足 errors.Is(err, core.ErrCheckFailed) 的错误。
func (r *Report) Err() error {
	if n := r.Count(Fail); n > 0 {
		return fmt.Errorf("%w: %d of %d checks failed", core.ErrCheckFailed, n, len(r.Checks))
	}
	return nil
}

// Failures 把 checks 中的 fail 合成一个满足 errors.Is(err, core.ErrCheckFailed) 的错误（每项附上修复
// 建议），没有时返回 nil；warnings 是 warn 项的说明。init、tun on 用它报告前置检查的结果。
func Failures(checks []Check) (warnings []string, err error) {
	var msgs []string
	for _, c := range checks {
		line := c.Message
		if c.Hint != "" {
			line += " (" + c.Hint + ")"
		}
		switch c.Status {
		case Fail:
			msgs = append(msgs, line)
		case Warn:
			warnings = append(warnings, line)
		}
	}
	if len(msgs) > 0 {
		err = fmt.Errorf("%w: %s", core.ErrCheckFailed, strings.Join(msgs, "; "))
	}
	return warnings, err
}

// Host 是被检查的本机。字段为空时使用真实的系统，测试时替换。
type Host struct {
	GOOS, GOARCH string
	Euid         func() int
	ReadFile     func(name string) ([]byte, error)
	Stat         func(name string) (fs.FileInfo, error)
	LookPath     func(file string) (string, error)
	// Run 执行命令，返回标准输出；失败时错误中含标准错误。
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (h *Host) goos() string {
	if h.GOOS != "" {
		return h.GOOS
	}
	return runtime.GOOS
}

func (h *Host) goarch() string {
	if h.GOARCH != "" {
		return h.GOARCH
	}
	return runtime.GOARCH
}

// Root 报告是否以 root 运行。
func (h *Host) Root() bool {
	if h.Euid != nil {
		return h.Euid() == 0
	}
	return os.Geteuid() == 0
}

func (h *Host) readFile(name string) ([]byte, error) {
	if h.ReadFile != nil {
		return h.ReadFile(name)
	}
	return os.ReadFile(name)
}

func (h *Host) stat(name string) (fs.FileInfo, error) {
	if h.Stat != nil {
		return h.Stat(name)
	}
	return os.Stat(name)
}

func (h *Host) lookPath(file string) error {
	if h.LookPath != nil {
		_, err := h.LookPath(file)
		return err
	}
	_, err := exec.LookPath(file)
	return err
}

func (h *Host) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if h.Run != nil {
		return h.Run(ctx, name, args...)
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%s: %s", name, strings.TrimSpace(stderr.String()))
	}
	return out, err
}

// Checker 依次收集一组检查的结果。
type Checker struct {
	Group string
	List  []Check
}

// Add 记下一项结果，ID 为“组.id”。ok、skip 时不保留修复建议。
func (c *Checker) Add(id, status, name, message, hint string) {
	if status == OK || status == Skip {
		hint = ""
	}
	c.List = append(c.List, Check{ID: c.Group + "." + id, Group: c.Group, Status: status, Name: name, Message: message, Hint: hint})
}

// NeedsRoot 是非 root 运行时跳过的项的说明。
const NeedsRoot = "needs root"

// PortOwners 返回正在监听的端口（键如 "tcp/7890"）及监听它的进程名。需要 root 才能看到进程。
func (h *Host) PortOwners(ctx context.Context) (map[string][]string, error) {
	out, err := h.run(ctx, "ss", "-Hlntup")
	if err != nil {
		return nil, err
	}
	return parseSS(out), nil
}

// parseSS 解析 ss -Hlntup 的输出，每行形如
// “tcp LISTEN 0 4096 127.0.0.1:7890 0.0.0.0:* users:(("mihomo",pid=812,fd=9))”。
func parseSS(out []byte) map[string][]string {
	owners := map[string][]string{}
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		local := f[4]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		key := f[0] + "/" + local[i+1:]
		if _, ok := owners[key]; !ok {
			owners[key] = []string{}
		}
		for _, m := range processName.FindAllStringSubmatch(strings.Join(f[5:], " "), -1) {
			if !slices.Contains(owners[key], m[1]) {
				owners[key] = append(owners[key], m[1])
			}
		}
	}
	return owners
}

// processName 匹配 ss 输出中的进程，如 ("mihomo",pid=812,fd=9)。
var processName = regexp.MustCompile(`\("([^"]+)",pid=`)

// Support 检查本机是否满足支持条件（第一组）：Linux、root、systemd、架构与 CPU 级别、WSL、容器、
// 发行版。init 有 fail 就拒绝安装。
func (h *Host) Support(ctx context.Context) []Check {
	c := &Checker{Group: GroupSystem}
	if goos := h.goos(); goos != "linux" {
		c.Add("os", Fail, "Operating system", goos+": tunlr only runs on Linux", "")
		return c.List
	}
	c.Add("os", OK, "Operating system", "Linux", "")

	if h.Root() {
		c.Add("root", OK, "Root", "running as root", "")
	} else {
		c.Add("root", Fail, "Root", "not running as root", "run tunlr as root (sudo)")
	}

	if fi, err := h.stat("/run/systemd/system"); err == nil && fi.IsDir() {
		c.Add("systemd", OK, "systemd", "systemd is the init system", "")
	} else {
		c.Add("systemd", Fail, "systemd", "systemd is not running", "tunlr needs a systemd-based Linux such as Debian or Ubuntu")
	}

	switch arch := h.goarch(); arch {
	case "arm64":
		c.Add("arch", OK, "Architecture", "arm64", "")
	case "amd64":
		c.Add("arch", OK, "Architecture", "amd64", "")
		data, err := h.readFile("/proc/cpuinfo")
		switch level := mihomo.CPULevel(data); {
		case err != nil:
			c.Add("cpu-level", Warn, "CPU level", "cannot read /proc/cpuinfo: "+err.Error(), "")
		case level < 3:
			c.Add("cpu-level", Warn, "CPU level",
				fmt.Sprintf("x86-64-v%d: mihomo runs its v%d build, which is slower than the standard v3 build", level, level),
				"on a virtual machine, set the CPU type to host or a newer model (this also brings AES-NI)")
		default:
			c.Add("cpu-level", OK, "CPU level", "x86-64-v3", "")
		}
	default:
		c.Add("arch", Fail, "Architecture", arch+": only amd64 and arm64 are supported", "")
	}

	if rel, err := h.readFile("/proc/sys/kernel/osrelease"); err == nil &&
		(bytes.Contains(bytes.ToLower(rel), []byte("microsoft")) || bytes.Contains(bytes.ToLower(rel), []byte("wsl"))) {
		c.Add("wsl", Fail, "WSL", "running in WSL, which is not supported", "use a Linux server or virtual machine")
	} else {
		c.Add("wsl", OK, "WSL", "not WSL", "")
	}

	if kind := h.container(ctx); kind != "" {
		c.Add("container", Fail, "Container", "running in a container ("+kind+"), which is not supported",
			"install tunlr on the host or in a virtual machine")
	} else {
		c.Add("container", OK, "Container", "not in a container", "")
	}

	c.distro(h)
	return c.List
}

// container 返回所在容器的类型，不在容器中时返回空串。
func (h *Host) container(ctx context.Context) string {
	out, err := h.run(ctx, "systemd-detect-virt", "--container")
	kind := strings.TrimSpace(string(out))
	if err == nil && kind != "" && kind != "none" {
		return kind
	}
	if kind == "none" {
		return ""
	}
	// 没有 systemd-detect-virt 时看常见的标记文件。
	for path, kind := range map[string]string{"/.dockerenv": "docker", "/run/.containerenv": "podman"} {
		if _, err := h.stat(path); err == nil {
			return kind
		}
	}
	return ""
}

// distro 检查发行版：Debian 12+、Ubuntu 22.04+ 经过验证，其他只警告。
func (c *Checker) distro(h *Host) {
	data, err := h.readFile("/etc/os-release")
	if err != nil {
		c.Add("distro", Warn, "Distribution", "cannot read /etc/os-release", "tunlr is tested on Debian 12+ and Ubuntu 22.04+")
		return
	}
	rel := parseOSRelease(data)
	name := rel["PRETTY_NAME"]
	if name == "" {
		name = strings.TrimSpace(rel["ID"] + " " + rel["VERSION_ID"])
	}
	min := map[string]float64{"debian": 12, "ubuntu": 22.04}[rel["ID"]]
	v, err := strconv.ParseFloat(rel["VERSION_ID"], 64)
	if min > 0 && err == nil && v >= min {
		c.Add("distro", OK, "Distribution", name, "")
		return
	}
	c.Add("distro", Warn, "Distribution", name+": not tested", "tunlr is tested on Debian 12+ and Ubuntu 22.04+; it should work on other systemd-based distributions")
}

// parseOSRelease 解析 /etc/os-release 的 KEY=value 行（值可以带引号）。
func parseOSRelease(data []byte) map[string]string {
	rel := map[string]string{}
	for line := range strings.Lines(string(data)) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		if u, err := strconv.Unquote(v); err == nil {
			v = u
		} else {
			v = strings.Trim(v, `'"`)
		}
		rel[k] = v
	}
	return rel
}

// Tun 检查能否开启透明代理（第二组）：TUN 设备、nft 与 ip 命令、ip rule 优先级、默认丢弃入站的防火墙、
// iptables-legacy。tun on 有 fail 就不开启。
func (h *Host) Tun(ctx context.Context, rules *netfilter.Rules) []Check {
	c := &Checker{Group: GroupTun}
	if h.goos() != "linux" {
		c.Add("device", Skip, "TUN device", "not Linux", "")
		return c.List
	}
	if _, err := h.stat("/dev/net/tun"); err == nil {
		c.Add("device", OK, "TUN device", "/dev/net/tun is available", "")
	} else {
		c.Add("device", Fail, "TUN device", "/dev/net/tun is missing (the tun kernel module is not available)",
			"load the module with modprobe tun; on a VPS, ask the provider to enable TUN")
	}
	tools := true
	for _, t := range []struct{ name, pkg string }{{"nft", "nftables"}, {"ip", "iproute2"}} {
		if err := h.lookPath(t.name); err == nil {
			c.Add(t.name, OK, t.name+" command", "found", "")
		} else {
			tools = false
			c.Add(t.name, Fail, t.name+" command", "the "+t.name+" command is missing", "install "+t.pkg)
		}
	}

	switch {
	case !h.Root():
		c.Add("ip-rules", Skip, "ip rule priorities", NeedsRoot, "")
	case !tools:
		c.Add("ip-rules", Skip, "ip rule priorities", "the ip command is missing", "")
	default:
		if conflicts := rules.RuleConflicts(ctx); len(conflicts) > 0 {
			c.Add("ip-rules", Fail, "ip rule priorities", strings.Join(conflicts, "; "),
				"remove these rules or move them to other priorities")
		} else {
			c.Add("ip-rules", OK, "ip rule priorities", fmt.Sprintf("%d-%d are free", netfilter.RulePriority, netfilter.RuleIndex+10), "")
		}
	}

	switch {
	case !h.Root():
		c.Add("firewall", Skip, "Firewall", NeedsRoot, "")
	case !tools:
		c.Add("firewall", Skip, "Firewall", "the nft command is missing", "")
	default:
		c.firewall(ctx, rules)
	}

	c.legacyIptables(ctx, h)
	return c.List
}

// firewall 检查有没有默认丢弃入站、又不先放行 DNAT 连接的规则链（见 netfilter.RedirectBlockers）：内核把
// TCP 重定向到自己的端口后经入站，会被它们丢弃。放行方法按链给出，网卡写作 <interface>（局域网网卡、容器
// 的网桥等）。
func (c *Checker) firewall(ctx context.Context, rules *netfilter.Rules) {
	list, err := rules.RedirectBlockers(ctx)
	switch {
	case err != nil:
		c.Add("firewall", Skip, "Firewall", "cannot list the nftables ruleset: "+err.Error(), "")
	case len(list) > 0:
		c.Add("firewall", Warn, "Firewall", netfilter.RedirectBlocked(list),
			"to let it through: "+strings.Join(netfilter.Fixes("<interface>", netfilter.PathInput, list), "; "))
	default:
		c.Add("firewall", OK, "Firewall", "nothing drops the TCP mihomo redirects to its own port by default", "")
	}
}

// legacyIptables 检查 iptables-legacy 中是否有规则：与 nftables 混用时，规则的先后难以预料。
func (c *Checker) legacyIptables(ctx context.Context, h *Host) {
	if !h.Root() {
		c.Add("iptables-legacy", Skip, "iptables-legacy", NeedsRoot, "")
		return
	}
	n, found := 0, false
	for _, cmd := range []string{"iptables-legacy-save", "ip6tables-legacy-save"} {
		if h.lookPath(cmd) != nil {
			continue
		}
		found = true
		out, err := h.run(ctx, cmd)
		if err != nil {
			continue
		}
		for line := range strings.Lines(string(out)) {
			if strings.HasPrefix(line, "-A ") {
				n++
			}
		}
	}
	switch {
	case n > 0:
		c.Add("iptables-legacy", Warn, "iptables-legacy", fmt.Sprintf("%d rules in iptables-legacy; mixing them with nftables is not supported", n),
			"move these rules to nftables (or iptables-nft) if the transparent proxy misbehaves")
	case found:
		c.Add("iptables-legacy", OK, "iptables-legacy", "no rules", "")
	default:
		c.Add("iptables-legacy", OK, "iptables-legacy", "not installed", "")
	}
}
