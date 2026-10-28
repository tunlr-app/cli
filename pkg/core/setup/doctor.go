package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/doctor"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// DoctorOptions 是诊断的参数。
type DoctorOptions struct {
	// Offline 跳过网络检查。
	Offline bool
	// Quick 只做快速的检查（status 用）：不联网、不运行 mihomo -t。
	Quick bool
	// Network 调整网络检查，测试时替换；APIBase、Mirror、Proxy 为空时由这里按设置和配置填入。
	Network doctor.NetworkOptions
}

// 订阅流量与到期的提醒阈值。
const (
	expireSoon   = 7 * 24 * time.Hour
	trafficAlert = 0.9
)

// Doctor 诊断本机和安装（见 DESIGN.md 4.1），按 doctor.Groups 的顺序返回全部检查。
// 有 fail 时 report.Err() 不为 nil；返回的错误只表示诊断本身无法进行。
func (i *Installer) Doctor(ctx context.Context, opts DoctorOptions) (*doctor.Report, error) {
	h := i.host()
	r := &doctor.Report{}
	r.Checks = append(r.Checks, h.Support(ctx)...)
	r.Checks = append(r.Checks, h.Tun(ctx, i.rules())...)
	r.Checks = append(r.Checks, i.installChecks(ctx, h, opts.Quick)...)
	if opts.Quick {
		return r, nil
	}
	if opts.Offline {
		c := &doctor.Checker{Group: doctor.GroupNetwork}
		c.Add("all", doctor.Skip, "Network", "skipped (--offline)", "")
		r.Checks = append(r.Checks, c.List...)
		return r, nil
	}
	r.Checks = append(r.Checks, doctor.Network(ctx, i.networkOptions(ctx, opts.Network))...)
	return r, nil
}

// CheckSupport 是 init 的前置检查：本机不满足支持条件时返回满足 errors.Is(err, core.ErrCheckFailed)
// 的错误，列出每个原因和修复建议。
func (i *Installer) CheckSupport(ctx context.Context) error {
	_, err := doctor.Failures(i.host().Support(ctx))
	return err
}

func (i *Installer) host() *doctor.Host {
	if i.Host != nil {
		return i.Host
	}
	return &doctor.Host{}
}

// networkOptions 按设置和生效的配置补全网络检查的参数：镜像取自设置，内核在运行且代理端口不要求
// 认证时经 mixed-port 访问测试地址。
func (i *Installer) networkOptions(ctx context.Context, opts doctor.NetworkOptions) doctor.NetworkOptions {
	d := i.Download
	if d == nil {
		d = &download.Client{}
	}
	if opts.APIBase == "" {
		opts.APIBase = d.APIBase
	}
	if opts.Mirror == "" {
		if set, err := settings.Load(i.Layout.SettingsFile()); err == nil {
			opts.Mirror = set.GitHubMirror
		}
	}
	if opts.Proxy != "" || opts.ProxySkip != "" {
		return opts
	}
	if _, err := mihomo.NewClient(i.Layout.CoreSocket()).Version(ctx); err != nil {
		opts.ProxySkip = "mihomo is not running"
		return opts
	}
	cfg, err := i.pipeline(nil).Current()
	if err != nil || cfg == nil {
		opts.ProxySkip = "no configuration has been published"
		return opts
	}
	l, err := config.Listen(cfg)
	switch {
	case err != nil:
		opts.ProxySkip = "cannot read the configuration: " + err.Error()
	case l.MixedPort == 0:
		opts.ProxySkip = "the configuration has no mixed-port"
	case l.Auth:
		opts.ProxySkip = "the proxy port requires authentication"
	default:
		opts.Proxy = "127.0.0.1:" + strconv.Itoa(l.MixedPort)
	}
	return opts
}

// installChecks 检查安装是否健康（第三组）。没有安装时只有一项 skip。
func (i *Installer) installChecks(ctx context.Context, h *doctor.Host, quick bool) []doctor.Check {
	c := &doctor.Checker{Group: doctor.GroupInstall}
	sd := i.systemd()
	if !sd.Installed() {
		c.Add("installed", doctor.Skip, "Installation", "tunlr is not installed; run tunlr init", "")
		return c.List
	}
	if !h.Root() {
		c.Add("installed", doctor.Skip, "Installation", "installed; checking it "+doctor.NeedsRoot, "")
		return c.List
	}

	i.checkUnits(c)
	i.checkTool(c)
	owner := i.checkUser(c)

	_, setErr := settings.Load(i.Layout.SettingsFile())
	st, stErr := state.Load(i.Layout.StateFile())
	if errors.Is(stErr, fs.ErrNotExist) {
		st, stErr = nil, nil
	}
	switch {
	case setErr != nil:
		c.Add("files", doctor.Fail, "Settings and state", setErr.Error(), "fix or remove "+i.Layout.SettingsFile())
	case stErr != nil:
		c.Add("files", doctor.Fail, "Settings and state", stErr.Error(), "")
	default:
		c.Add("files", doctor.OK, "Settings and state", "readable", "")
	}

	status, err := sd.Status(ctx, service.CoreUnit)
	running := ""
	if v, err := mihomo.NewClient(i.Layout.CoreSocket()).Version(ctx); err == nil {
		running = v
	}
	switch {
	case err != nil:
		c.Add("service", doctor.Fail, "Service", err.Error(), "")
	case status.ActiveState == "failed":
		c.Add("service", doctor.Fail, "Service", service.CoreUnit+" has failed", "see tunlr logs, then tunlr start")
	case !status.Active():
		c.Add("service", doctor.Warn, "Service", service.CoreUnit+" is "+status.ActiveState, "tunlr start")
	case running == "":
		c.Add("service", doctor.Fail, "Service", "the service is running but mihomo does not answer on its API socket", "see tunlr logs, or tunlr restart")
	case status.NRestarts > 0:
		c.Add("service", doctor.Warn, "Service", fmt.Sprintf("running %s, restarted %d times by systemd", running, status.NRestarts), "see tunlr logs")
	default:
		c.Add("service", doctor.OK, "Service", "running "+running, "")
	}
	if v, err := sd.Property(ctx, service.CoreUnit, "UnitFileState"); err == nil && v != "enabled" {
		c.Add("enabled", doctor.Warn, "Start at boot", service.CoreUnit+" is "+v, "tunlr init")
	} else if err == nil {
		c.Add("enabled", doctor.OK, "Start at boot", "enabled", "")
	}

	ks, err := i.kernels(owner, mihomo.Arch{}).Status()
	switch {
	case err != nil:
		c.Add("kernel", doctor.Fail, "Kernel", err.Error(), "")
	case !ks.Installed:
		c.Add("kernel", doctor.Fail, "Kernel", "mihomo is not installed", "tunlr init")
	case !ks.KnownGood:
		c.Add("kernel", doctor.Warn, "Kernel", "bin/mihomo is not a known-good version (replaced outside tunlr?)",
			"tunlr upgrade core installs a verified release")
	default:
		c.Add("kernel", doctor.OK, "Kernel", ks.Version+", known good", "")
	}

	i.checkConfig(ctx, c, owner, quick, ks != nil && ks.Installed)
	if setErr == nil && stErr == nil {
		i.checkMixins(ctx, c)
		i.checkTimers(ctx, c, st)
	}
	i.checkTun(ctx, c, st, running != "")
	i.checkGateway(ctx, c, st)
	i.checkExposure(c)
	i.checkPanelLAN(ctx, c, st)
	i.checkProxyLAN(c, st)
	i.checkPorts(ctx, h, c, running != "")
	i.checkSubscriptions(c, st)
	return c.List
}

// checkUnits 比较 unit 文件与当前版本的模板：换了新版本的 tunlr 而没有重新 init 时不一致。
func (i *Installer) checkUnits(c *doctor.Checker) {
	want, err := service.RenderUnits(i.Layout)
	if err != nil {
		c.Add("units", doctor.Fail, "systemd units", err.Error(), "")
		return
	}
	var stale []string
	for _, name := range service.Units {
		have, err := os.ReadFile(filepath.Join(i.Layout.UnitDir(), name))
		if err != nil || !bytes.Equal(have, want[name]) {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		c.Add("units", doctor.Warn, "systemd units", "missing or from another version: "+strings.Join(stale, ", "), "tunlr init")
		return
	}
	c.Add("units", doctor.OK, "systemd units", "up to date", "")
}

// checkTool 检查 unit 调用的 /usr/local/bin/tunlr 是否就是正在运行的这个。
func (i *Installer) checkTool(c *doctor.Checker) {
	self, err := selfPath()
	if err != nil {
		c.Add("tool", doctor.Warn, "tunlr binary", err.Error(), "")
		return
	}
	same, err := sameFile(self, i.Layout.ToolBinary())
	switch {
	case err != nil:
		c.Add("tool", doctor.Warn, "tunlr binary", err.Error(), "")
	case !same:
		c.Add("tool", doctor.Warn, "tunlr binary", i.Layout.ToolBinary()+" is missing or differs from the tunlr you are running",
			"run tunlr init with the version you want installed")
	default:
		c.Add("tool", doctor.OK, "tunlr binary", i.Layout.ToolBinary(), "")
	}
}

// checkUser 检查内核用户，以及 bin/、core/ 是否归它所有，返回内核用户（测试布局中为 nil）。
func (i *Installer) checkUser(c *doctor.Checker) *fsutil.Owner {
	owner, err := service.KernelUser(i.Layout)
	switch {
	case err != nil:
		c.Add("user", doctor.Fail, "Kernel user", err.Error(), "")
		return nil
	case owner == nil && i.Layout.Root == "":
		c.Add("user", doctor.Fail, "Kernel user", "the user "+service.CoreUser+" does not exist", "tunlr init")
		return nil
	case owner == nil:
		c.Add("user", doctor.Skip, "Kernel user", "test layout", "")
		return nil
	}
	var wrong []string
	for _, dir := range []string{i.Layout.BinDir(), i.Layout.CoreDir()} {
		fi, err := os.Stat(dir)
		if err != nil {
			wrong = append(wrong, dir+" is missing")
			continue
		}
		if have, ok := fsutil.FileOwner(fi); ok && have != *owner {
			wrong = append(wrong, dir+" is not owned by "+service.CoreUser)
		}
	}
	if len(wrong) > 0 {
		c.Add("user", doctor.Fail, "Kernel user", strings.Join(wrong, "; "), "tunlr init")
	} else {
		c.Add("user", doctor.OK, "Kernel user", service.CoreUser+" owns bin/ and core/", "")
	}
	return owner
}

// checkConfig 检查生效的配置：存在且通过 mihomo -t（quick 时不运行 -t）。
func (i *Installer) checkConfig(ctx context.Context, c *doctor.Checker, owner *fsutil.Owner, quick, kernel bool) {
	p := i.pipeline(owner)
	if quick || !kernel {
		cfg, err := p.Current()
		switch {
		case err != nil:
			c.Add("config", doctor.Fail, "Configuration", err.Error(), "")
		case cfg == nil:
			c.Add("config", doctor.Warn, "Configuration", "nothing has been published", "add a subscription (tunlr sub add), then tunlr apply")
		default:
			c.Add("config", doctor.Skip, "Configuration", "published; not checked with mihomo -t", "")
		}
		return
	}
	exists, err := p.CheckCurrent(ctx)
	switch {
	case !exists && err == nil:
		c.Add("config", doctor.Warn, "Configuration", "nothing has been published", "add a subscription (tunlr sub add), then tunlr apply")
	case err != nil:
		c.Add("config", doctor.Fail, "Configuration", "the published configuration fails mihomo -t: "+err.Error(),
			"tunlr apply publishes a fresh one; tunlr rollback goes back to an earlier one")
	default:
		c.Add("config", doctor.OK, "Configuration", "passes mihomo -t", "")
	}
}

// checkMixins 按现在的订阅和 mixin 合成一次配置（不校验），发现有错的 mixin，包括动了 tunlr 独占的键的
// （例如手动改的 mixin 写了 tun.enable）：下一次 apply 和定时更新都会因此拒绝发布。
func (i *Installer) checkMixins(ctx context.Context, c *doctor.Checker) {
	var me *config.MixinError
	switch err := i.pipeline(nil).CheckMixins(ctx); {
	case errors.As(err, &me):
		c.Add("mixins", doctor.Fail, "Mixins", err.Error(), "tunlr mixin edit "+me.File+", then tunlr apply")
	case err != nil:
		c.Add("mixins", doctor.Warn, "Mixins", "could not compose the configuration: "+err.Error(), "")
	default:
		c.Add("mixins", doctor.OK, "Mixins", "no conflict with the settings tunlr manages", "")
	}
}

// checkTimers 检查两个 timer 是否与设置和 TUN 开关一致，以及有没有遗留的回滚定时器。
func (i *Installer) checkTimers(ctx context.Context, c *doctor.Checker, st *state.State) {
	scheds, err := i.Schedules()
	if err != nil {
		c.Add("timers", doctor.Fail, "Timers", err.Error(), "")
		return
	}
	sd := i.systemd()
	wantUpdate := slices.ContainsFunc(scheds, func(s Schedule) bool { return !s.Next.IsZero() })
	tun := st != nil && st.TunEnabled()
	var wrong []string
	for _, t := range []struct {
		name string
		want bool
	}{{service.UpdateTimer, wantUpdate}, {service.CheckTimer, tun}} {
		if got := sd.TimerEnabled(ctx, t.name); got != t.want {
			wrong = append(wrong, fmt.Sprintf("%s is %s", t.name, map[bool]string{true: "enabled", false: "disabled"}[got]))
		}
	}
	if len(wrong) > 0 {
		c.Add("timers", doctor.Warn, "Timers", strings.Join(wrong, ", ")+", which does not match the settings", "tunlr init")
	} else {
		c.Add("timers", doctor.OK, "Timers", "match the settings", "")
	}
	if sd.GuardArmed(ctx) && (st == nil || st.Tun != state.TunPending) {
		c.Add("guard", doctor.Warn, "Rollback timer", service.GuardUnit+" is still armed although TUN is not being checked",
			"systemctl stop "+service.GuardUnit+".timer")
	}
}

// checkTun 检查 TUN 开关与内核实际状态是否一致，内核在运行时两张 nft 表是否都在。
func (i *Installer) checkTun(ctx context.Context, c *doctor.Checker, st *state.State, running bool) {
	ts, err := i.TunStatus(ctx)
	if err != nil {
		c.Add("tun", doctor.Fail, "Transparent proxy", err.Error(), "")
		return
	}
	on := st != nil && st.TunEnabled()
	switch {
	case !running:
		c.Add("tun", doctor.Skip, "Transparent proxy", "switch "+ts.Switch+"; mihomo is not running", "")
	case !ts.Rules:
		c.Add("tun", doctor.Fail, "Transparent proxy", "the nftables table inet "+"tunlr is missing", "tunlr restart")
	case on && ts.Running != nil && !*ts.Running:
		c.Add("tun", doctor.Fail, "Transparent proxy", "the switch is "+ts.Switch+" but mihomo has TUN off (turned off in the panel?)",
			"tunlr tun on, or tunlr tun off to match")
	case on && !ts.KernelRules:
		c.Add("tun", doctor.Fail, "Transparent proxy", "TUN is on but the nftables table inet mihomo is missing", "tunlr restart")
	case !on && ts.Running != nil && *ts.Running:
		c.Add("tun", doctor.Warn, "Transparent proxy", "TUN was turned on from the panel; tunlr does not protect it with checks and rollback",
			"use tunlr tun on instead")
	default:
		c.Add("tun", doctor.OK, "Transparent proxy", "switch "+ts.Switch+", matches mihomo", "")
	}
}

// checkGateway 在网关开启时检查各项是否到位（见 DESIGN.md 8.10）；网关关闭时不出现。
func (i *Installer) checkGateway(ctx context.Context, c *doctor.Checker, st *state.State) {
	if st == nil || st.Gateway == nil {
		return
	}
	iface := st.Gateway.Interface
	var fails, warns []string
	if st.Tun != state.TunOn {
		fails = append(fails, "TUN is off")
	}
	if _, err := i.lookupInterface(iface); err != nil {
		fails = append(fails, "the interface "+iface+" is missing")
	}
	if i.ipForward() != "1" {
		fails = append(fails, "IPv4 forwarding is off")
	}
	if _, err := os.Stat(i.Layout.SysctlFile()); err != nil {
		warns = append(warns, i.Layout.SysctlFile()+" is missing, so forwarding is off after a reboot")
	}
	rs, err := i.rules().Gateway(ctx, iface)
	switch {
	case err != nil:
		fails = append(fails, err.Error())
	case !rs.Table:
		fails = append(fails, "the nftables table inet "+netfilter.GatewayTable+" is missing (LAN devices bypass mihomo when it stops)")
	}
	if err == nil && rs.DockerChain && !rs.DockerRules {
		fails = append(fails, "the accept rules in "+netfilter.DockerChain+" are missing (Docker drops the forwarded traffic)")
	}
	if i.strictRPFilter(iface) {
		warns = append(warns, "rp_filter is strict (1) on "+iface+"; replies from TUN may be dropped (not tested yet)")
	}
	switch {
	case len(fails) > 0:
		c.Add("gateway", doctor.Fail, "Gateway", strings.Join(append(fails, warns...), "; "), "tunlr gateway on --interface "+iface+" restores it")
	case len(warns) > 0:
		c.Add("gateway", doctor.Warn, "Gateway", strings.Join(warns, "; "), "tunlr gateway on --interface "+iface)
	default:
		c.Add("gateway", doctor.OK, "Gateway", "on "+iface+": forwarding, nftables and Docker rules in place", "")
	}
}

// checkExposure 检查生效的配置会不会开放无需认证的端口（见 DESIGN.md 第 6 节“端口安全”）。
// 发布时已经拒绝这种配置，这里发现的是工具发布之外改动的或较早版本发布的配置。
func (i *Installer) checkExposure(c *doctor.Checker) {
	cfg, err := i.pipeline(nil).Current()
	if err != nil || cfg == nil {
		return
	}
	var ee *config.ExposureError
	switch err := config.CheckExposure(cfg); {
	case errors.As(err, &ee):
		c.Add("exposure", doctor.Fail, "Port security", strings.Join(ee.Problems, "; "), "fix it in a mixin (tunlr mixin edit), then tunlr apply")
	case err != nil:
		c.Add("exposure", doctor.Fail, "Port security", err.Error(), "")
	default:
		c.Add("exposure", doctor.OK, "Port security", "no port is open to other hosts without authentication", "")
	}
}

// checkPanelLAN 在面板对局域网开放时检查 nft 表是否在、生效的配置是否按开关监听；开关关闭时不出现。
func (i *Installer) checkPanelLAN(ctx context.Context, c *doctor.Checker, st *state.State) {
	if st == nil || st.PanelLAN == nil {
		return
	}
	tables, err := i.rules().Tables(ctx)
	switch {
	case err != nil:
		c.Add("panel", doctor.Fail, "Panel LAN access", err.Error(), "")
		return
	case !tables[netfilter.PanelTable]:
		c.Add("panel", doctor.Fail, "Panel LAN access", "the nftables table inet "+netfilter.PanelTable+
			" is missing, so the panel accepts connections from any address", "tunlr panel lan on restores it")
		return
	}
	cfg, err := i.pipeline(nil).Current()
	if err != nil || cfg == nil {
		return
	}
	if api, err := config.API(cfg); err == nil && api.Controller != pipeline.LANController {
		c.Add("panel", doctor.Warn, "Panel LAN access", "the switch is on but the published configuration has external-controller "+
			strconv.Quote(api.Controller)+" (edited outside tunlr?)", "tunlr apply")
		return
	}
	c.Add("panel", doctor.OK, "Panel LAN access", "on; port "+strconv.Itoa(pipeline.APIPort)+" only accepts private addresses", "")
}

// checkProxyLAN 在代理端口对局域网开放时检查生效的配置是否与开关一致；开关关闭时不出现（对外开放的配置由
// checkExposure 报告）。
func (i *Installer) checkProxyLAN(c *doctor.Checker, st *state.State) {
	if st == nil || st.ProxyLAN == nil {
		return
	}
	cfg, err := i.pipeline(nil).Current()
	if err != nil || cfg == nil {
		return
	}
	switch allow, private, err := config.LANAccess(cfg); {
	case err != nil:
		c.Add("proxy", doctor.Fail, "Proxy LAN access", err.Error(), "")
	case !allow:
		c.Add("proxy", doctor.Warn, "Proxy LAN access", "the switch is on but the published configuration has allow-lan off (edited outside tunlr?)", "tunlr apply")
	case !private:
		c.Add("proxy", doctor.Warn, "Proxy LAN access", "the switch is on but lan-allowed-ips in the published configuration is not limited to private addresses", "tunlr apply")
	default:
		c.Add("proxy", doctor.OK, "Proxy LAN access", "on; the proxy ports only accept private addresses", "")
	}
}

// checkPorts 检查配置中的端口有没有被其他程序占用：内核在运行时它们应当归内核，没有运行时应当空闲。
func (i *Installer) checkPorts(ctx context.Context, h *doctor.Host, c *doctor.Checker, running bool) {
	cfg, err := i.pipeline(nil).Current()
	if err != nil || cfg == nil {
		return
	}
	l, err := config.Listen(cfg)
	if err != nil || len(l.Listeners) == 0 {
		return
	}
	owners, err := h.PortOwners(ctx)
	if err != nil {
		c.Add("ports", doctor.Skip, "Ports", "cannot list the listening sockets: "+err.Error(), "")
		return
	}
	var taken []string
	for _, ln := range l.Listeners {
		procs, listening := owners[ln.Network+"/"+strconv.Itoa(ln.Port)]
		others := slices.DeleteFunc(slices.Clone(procs), func(p string) bool { return p == "mihomo" })
		switch {
		case len(others) > 0:
			taken = append(taken, fmt.Sprintf("%s %d (%s) is used by %s", ln.Network, ln.Port, ln.Key, strings.Join(others, ", ")))
		case listening && !running && len(procs) == 0:
			taken = append(taken, fmt.Sprintf("%s %d (%s) is in use", ln.Network, ln.Port, ln.Key))
		}
	}
	if len(taken) > 0 {
		c.Add("ports", doctor.Fail, "Ports", strings.Join(taken, "; "), "stop that program, or change the port in a mixin (tunlr mixin edit)")
		return
	}
	if running {
		c.Add("ports", doctor.OK, "Ports", fmt.Sprintf("%d ports, none used by another program", len(l.Listeners)), "")
	} else {
		c.Add("ports", doctor.OK, "Ports", fmt.Sprintf("%d ports, all free", len(l.Listeners)), "")
	}
}

// checkSubscriptions 检查订阅：最近一次抓取失败、已到期或快到期、流量用完或快用完。启用的订阅
// 到期或流量用完判 fail，其余判 warn。
func (i *Installer) checkSubscriptions(c *doctor.Checker, st *state.State) {
	if st == nil || len(st.Subscriptions) == 0 {
		c.Add("subscriptions", doctor.Warn, "Subscriptions", "no subscription", "tunlr sub add <name> <url>")
		return
	}
	if st.Active == "" {
		c.Add("subscriptions", doctor.Warn, "Subscriptions", "no active subscription", "tunlr sub use")
	}
	now := i.now()
	for _, s := range st.Subscriptions {
		id, name := "sub-"+s.Name, "Subscription "+s.Name
		active := s.Name == st.Active
		bad := doctor.Warn
		if active {
			bad = doctor.Fail
		}
		var problems []string
		status := doctor.OK
		if s.LastErrorAt.After(s.FetchedAt) {
			problems = append(problems, "the last update failed at "+s.LastErrorAt.Local().Format("2006-01-02 15:04")+": "+s.LastError)
			status = doctor.Warn
		}
		hint := "tunlr sub update " + s.Name
		if u := s.UserInfo; u != nil {
			switch {
			case !u.Expire.IsZero() && !now.Before(u.Expire):
				problems = append(problems, "expired on "+u.Expire.Local().Format("2006-01-02"))
				status, hint = bad, "renew it with the provider"
			case !u.Expire.IsZero() && u.Expire.Sub(now) < expireSoon:
				problems = append(problems, "expires on "+u.Expire.Local().Format("2006-01-02"))
				status, hint = worst(status, doctor.Warn), "renew it with the provider"
			}
			if u.Total > 0 {
				used := float64(u.Upload+u.Download) / float64(u.Total)
				switch {
				case used >= 1:
					problems = append(problems, "the traffic is used up")
					status, hint = worst(status, bad), "renew it with the provider"
				case used >= trafficAlert:
					problems = append(problems, fmt.Sprintf("%.0f%% of the traffic is used", used*100))
					status = worst(status, doctor.Warn)
				}
			}
		}
		if len(problems) == 0 {
			c.Add(id, doctor.OK, name, "fine", "")
			continue
		}
		c.Add(id, status, name, strings.Join(problems, "; "), hint)
	}
}

// worst 返回 a、b 中更严重的结果。
func worst(a, b string) string {
	rank := map[string]int{doctor.OK: 0, doctor.Skip: 0, doctor.Warn: 1, doctor.Fail: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
