package netfilter

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/template"

	"github.com/tunlr-app/cli/templates"
)

// 透明代理中与内核约定的值。它们作为强制项写入配置的 tun 段，工具的规则、清理和检测都依赖它们。
const (
	// Device 是内核建立的 TUN 网卡。
	Device = "tunlr0"
	// RouteTable 是内核 auto-route 使用的路由表。
	RouteTable = 2022
	// RuleIndex 是内核 auto-route 的 ip rule 起始优先级，内核使用 RuleIndex 到 RuleIndex+10。
	RuleIndex = 9000
	// RulePriority 是工具的 ip rule 的优先级，早于内核的规则。
	RulePriority = RuleIndex - 1
	// Mark 是工具给入站连接及其回包打的标记。只占一个高位并按位设置，不覆盖其他程序的标记：
	// Tailscale 用 0xff0000 段，kube-proxy 用 0x4000、0x8000，内核的 auto-redirect 用 0x2023、0x2024。
	Mark = 0x10000000
	// Table 是工具的 nftables 表（inet 族）。
	Table = "tunlr"
	// KernelTable 是内核 auto-redirect 建立的 nftables 表（inet 族），内核正常退出时自己删除。
	KernelTable = "mihomo"
)

// Runner 执行外部命令，stdin 不为 nil 时作为标准输入；返回标准输出，失败时的错误包含标准错误的内容。
type Runner func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)

// Exec 是默认的 Runner。
func Exec(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
		}
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// Rules 加载和清理工具补充的规则：inet tunlr 表，以及让带标记的回包走主路由表的 ip rule。
// 规则不依赖 TUN 是否开启（关闭时无害），随内核服务启动加载、停止清理。
type Rules struct {
	// Run 为空时使用 Exec。
	Run Runner
	// IPv6 报告是否同时维护 IPv6 的 ip rule；为 nil 时看本机是否启用了 IPv6。
	IPv6 func() bool
}

// Render 生成 inet tunlr 表的 nft 脚本。
func Render() ([]byte, error) {
	t, err := template.New("tunlr.nft").Option("missingkey=error").Parse(string(templates.Nft))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = t.Execute(&b, map[string]string{
		"Table":  Table,
		"Device": Device,
		"Mark":   fmt.Sprintf("%#x", Mark),
	})
	return b.Bytes(), err
}

// Up 加载（或替换）inet tunlr 表，并确保每个地址族恰好有一条工具的 ip rule。可以重复执行。
func (r *Rules) Up(ctx context.Context) error {
	script, err := Render()
	if err != nil {
		return err
	}
	if _, err := r.run(ctx, script, "nft", "-f", "-"); err != nil {
		return err
	}
	for _, family := range r.families() {
		if err := r.deleteRules(ctx, family); err != nil {
			return err
		}
		if _, err := r.run(ctx, nil, "ip", append([]string{family, "rule", "add"}, ruleSpec()...)...); err != nil {
			return err
		}
	}
	return nil
}

// Down 删除 inet tunlr 表和工具的 ip rule。内核的 inet mihomo 表还在时（内核被 SIGKILL 或崩溃，
// 没能自己清理）一并删除，否则它会把容器的 TCP 和 DNS 重定向到已不存在的监听端口；这时内核在
// RuleIndex 到 RuleIndex+10 留下的 ip rule 也一并删除（优先级由强制项固定，可以确定是内核的）。
// kernelLeftover 报告是否有这种残留。只在内核已经停止时调用。
func (r *Rules) Down(ctx context.Context) (kernelLeftover bool, err error) {
	tables, err := r.Tables(ctx)
	if err != nil {
		return false, err
	}
	var errs []error
	if tables[Table] {
		if _, err := r.run(ctx, nil, "nft", "delete", "table", "inet", Table); err != nil {
			errs = append(errs, err)
		}
	}
	for _, family := range r.families() {
		errs = append(errs, r.deleteRules(ctx, family))
	}
	if tables[KernelTable] {
		kernelLeftover = true
		if _, err := r.run(ctx, nil, "nft", "delete", "table", "inet", KernelTable); err != nil {
			errs = append(errs, err)
		}
		for _, family := range r.families() {
			errs = append(errs, r.deleteKernelRules(ctx, family))
		}
	}
	return kernelLeftover, errors.Join(errs...)
}

// deleteKernelRules 按优先级删除内核在 family 中留下的 ip rule。
func (r *Rules) deleteKernelRules(ctx context.Context, family string) error {
	rules, err := r.listRules(ctx, family)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if rule.priority < RuleIndex || rule.priority > RuleIndex+10 {
			continue
		}
		if _, err := r.run(ctx, nil, "ip", family, "rule", "del", "priority", strconv.Itoa(rule.priority)); err != nil {
			return err
		}
	}
	return nil
}

// Tables 返回存在的 inet 族 nftables 表，键为表名。
func (r *Rules) Tables(ctx context.Context) (map[string]bool, error) {
	out, err := r.run(ctx, nil, "nft", "list", "tables", "inet")
	if err != nil {
		return nil, err
	}
	tables := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 3 && f[0] == "table" && f[1] == "inet" {
			tables[f[2]] = true
		}
	}
	return tables, nil
}

// DeviceUp 报告内核的 TUN 网卡是否存在。
func DeviceUp() bool {
	_, err := net.InterfaceByName(Device)
	return err == nil
}

// Preflight 检查开启 TUN 的前置条件。problems 不满足时不能开启；warnings 是可能影响使用、未实测的情况。
func (r *Rules) Preflight(ctx context.Context) (problems, warnings []string) {
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		problems = append(problems, "/dev/net/tun is missing (the tun kernel module is not available)")
	}
	for _, name := range []string{"nft", "ip"} {
		if _, err := exec.LookPath(name); err != nil {
			problems = append(problems, fmt.Sprintf("the %s command is missing (install nftables and iproute2)", name))
		}
	}
	if len(problems) > 0 {
		return problems, nil
	}
	problems = append(problems, r.RuleConflicts(ctx)...)
	for _, fw := range r.ActiveFirewalls(ctx) {
		warnings = append(warnings, fmt.Sprintf("%s is active; its input policy may block the traffic mihomo redirects (not tested yet)", fw))
	}
	return problems, warnings
}

// RuleConflicts 返回占用了工具和内核所需 ip rule 优先级（RulePriority 到 RuleIndex+10）的他人规则，
// 每条一句说明。TUN 网卡在时，RuleIndex 起的规则是内核自己的，不算冲突。
func (r *Rules) RuleConflicts(ctx context.Context) []string {
	var problems []string
	tunUp := DeviceUp()
	for _, family := range r.families() {
		rules, err := r.listRules(ctx, family)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for _, rule := range rules {
			switch {
			case rule.priority == RulePriority && rule.ours():
			case rule.priority == RulePriority, rule.priority >= RuleIndex && rule.priority <= RuleIndex+10 && !tunUp:
				problems = append(problems, fmt.Sprintf("ip %s rule %q uses a priority that tunlr needs (%d-%d)", family, rule.text, RulePriority, RuleIndex+10))
			}
		}
	}
	return problems
}

// ActiveFirewalls 返回正在运行的 UFW、firewalld。它们的入站策略可能挡住内核重定向的流量（未实测）。
func (r *Rules) ActiveFirewalls(ctx context.Context) []string {
	var active []string
	for _, fw := range []string{"ufw", "firewalld"} {
		if out, _ := r.run(ctx, nil, "systemctl", "is-active", fw); strings.TrimSpace(string(out)) == "active" {
			active = append(active, fw)
		}
	}
	return active
}

// ruleSpec 是工具的 ip rule：带标记的包查主路由表。
func ruleSpec() []string {
	m := fmt.Sprintf("%#x", Mark)
	return []string{"fwmark", m + "/" + m, "lookup", "main", "priority", strconv.Itoa(RulePriority)}
}

// deleteRules 删除 family 中工具的 ip rule（可能因重复添加而有多条）。
func (r *Rules) deleteRules(ctx context.Context, family string) error {
	rules, err := r.listRules(ctx, family)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if rule.priority != RulePriority || !rule.ours() {
			continue
		}
		if _, err := r.run(ctx, nil, "ip", append([]string{family, "rule", "del"}, ruleSpec()...)...); err != nil {
			return err
		}
	}
	return nil
}

type ipRule struct {
	priority int
	text     string
}

// ours 报告规则是否是工具添加的。ip rule show 显示为 “fwmark 0x10000000/0x10000000 lookup main”。
func (r ipRule) ours() bool {
	m := fmt.Sprintf("%#x", Mark)
	return strings.Contains(r.text, "fwmark "+m+"/"+m) && strings.HasSuffix(r.text, "lookup main")
}

// listRules 解析 ip rule show 的输出，每行形如 “8999:	from all fwmark 0x10000000/0x10000000 lookup main”。
func (r *Rules) listRules(ctx context.Context, family string) ([]ipRule, error) {
	out, err := r.run(ctx, nil, "ip", family, "rule", "show")
	if err != nil {
		return nil, err
	}
	var rules []ipRule
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		p, text, ok := strings.Cut(sc.Text(), ":")
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if !ok || err != nil {
			continue
		}
		rules = append(rules, ipRule{n, strings.TrimSpace(text)})
	}
	return rules, nil
}

func (r *Rules) families() []string {
	ipv6 := r.IPv6
	if ipv6 == nil {
		ipv6 = func() bool {
			_, err := os.Stat("/proc/net/if_inet6")
			return err == nil
		}
	}
	if ipv6() {
		return []string{"-4", "-6"}
	}
	return []string{"-4"}
}

func (r *Rules) run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	if r.Run != nil {
		return r.Run(ctx, stdin, name, args...)
	}
	return Exec(ctx, stdin, name, args...)
}
