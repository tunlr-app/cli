package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// IPv6 的设置（tunlr ipv6），见 DESIGN.md 8.3。
const (
	// IPv6Off：TUN 接管 IPv6，内核的 DNS 不回答 IPv6 地址（dns.ipv6: false，tunlr 的兜底值），程序只经 IPv4 连接；
	// 写死的 IPv6 DNS 服务器和地址照样经过 mihomo。
	IPv6Off = "off"
	// IPv6On：也回答 IPv6 地址（dns.ipv6: true）。
	IPv6On = "on"
	// IPv6Bypass：mihomo 不接管 IPv6（顶层 ipv6: false），自己也不用 IPv6、不回答 IPv6 地址（dns.ipv6 为 true 也
	// 一样，8.7 实测）。TUN 开着时拒绝本机经 IPv6 的 DNS 查询，解析改问 IPv4 的服务器、经 TUN 由 mihomo 回答，所以
	// 域名仍只解析出 IPv4 地址；只有直接连 IPv6 地址的连接按系统路由直连，不经代理。
	IPv6Bypass = "bypass"
	// IPv6Default 只用于修改：不 bypass，是否回答 IPv6 地址交给订阅或兜底值。
	IPv6Default = "default"
)

// IPv6 的回答在配置中的键：dns.ipv6 让内核的 DNS 也返回 IPv6 地址（AAAA），fake-ip 模式下 IPv6 的假地址取自
// dns.fake-ip-range6。tunlr ipv6 on|off|default 一起改这两个键。
const (
	dnsIPv6Key      = "dns.ipv6"
	fakeIPRange6Key = "dns.fake-ip-range6"
	fakeIPRangeKey  = "dns.fake-ip-range"
)

// FakeIPRange6 是 ipv6 on 在配置中没有 IPv6 假地址段时写的：mihomo 文档中的地址段，mihomo 给 TUN 配的
// IPv6 地址（fdfe:dcba:9876::1/126）也在其中。
const FakeIPRange6 = "fdfe:dcba:9876::1/64"

// DNSIPv6 是按启用的订阅合成的配置中的 dns.ipv6 和决定它的地方。
type DNSIPv6 struct {
	// On 是 dns.ipv6；Set 报告配置中写了它，没有写时内核不返回 IPv6 地址。
	On  bool `json:"on"`
	Set bool `json:"set"`
	// From、Fallback、Subscription 同 DNSMode。
	From         string `json:"from,omitempty"`
	Fallback     bool   `json:"fallback,omitempty"`
	Subscription string `json:"subscription,omitempty"`
	// Files 是共用的和启用的订阅的 mixin 中写了 dns.ipv6 或 dns.fake-ip-range6 的文件，按应用的顺序；
	// ipv6 default 从这些文件中删除它们。
	Files []string `json:"files,omitempty"`
}

// Pinned 报告 dns.ipv6 由 mixin 写定（tunlr ipv6 on|off 写的，或手写的），而不是来自订阅或兜底值。
func (d *DNSIPv6) Pinned() bool { return !d.Fallback && slices.Contains(d.Files, d.From) }

// IPv6Status 是 IPv6 的设置。
type IPv6Status struct {
	// Mode 是 IPv6Bypass，或按 Answers 的 IPv6On、IPv6Off。
	Mode string `json:"mode"`
	// Answers 是 dns.ipv6 和决定它的地方；Mode 为 bypass 时它不起作用（顶层 ipv6: false 时内核不回答 IPv6 地址），
	// 换回 off、on 时再用到。
	Answers DNSIPv6 `json:"answers"`
	// Tun 报告透明代理（TUN）的开关是否打开；关着时本机的程序不经 mihomo，设置只影响 mihomo 自己的连接。
	Tun bool `json:"tun"`
}

// ipv6Of 返回 plan（按启用的订阅合成的）中的 IPv6 设置，mixins 是共用的和它的 mixin，st 是状态（还没有时为 nil）。
func ipv6Of(plan *pipeline.Plan, mixins []config.Mixin, st *state.State) *IPv6Status {
	src := plan.KeySource(dnsIPv6Key)
	d := DNSIPv6{On: src.Value == "true", Set: src.Value != "", From: src.From, Fallback: src.Fallback, Subscription: plan.Subscription}
	for _, m := range mixins {
		if config.MixinSets(m.Data, dnsIPv6Key) || config.MixinSets(m.Data, fakeIPRange6Key) {
			d.Files = append(d.Files, m.Name)
		}
	}
	s := &IPv6Status{Mode: IPv6Off, Answers: d, Tun: st != nil && st.TunEnabled()}
	switch {
	case st != nil && st.IPv6Bypass:
		s.Mode = IPv6Bypass
	case d.On:
		s.Mode = IPv6On
	}
	return s
}

// IPv6 返回 IPv6 的设置。
func (i *Installer) IPv6(ctx context.Context) (*IPv6Status, error) {
	plan, mixins, err := i.composeActive(ctx)
	if err != nil {
		return nil, err
	}
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return ipv6Of(plan, mixins, st), nil
}

// IPv6Edit 是 PlanIPv6 准备的修改，由 SaveIPv6 写入。
type IPv6Edit struct {
	Before IPv6Status `json:"before"`
	// Set 是要设的：IPv6Off、IPv6On、IPv6Bypass 或 IPv6Default。
	Set string `json:"set"`
	// Changes 是要改的 mixin 文件。bypass 不改文件：dns.ipv6 留着，换回 off、on 时再用到。
	Changes []*MixinChange `json:"changes"`
	// Bypass 是改后的开关；Changed 报告有要改的（文件或开关），没有时已经是这样。
	Bypass  bool `json:"bypass"`
	Changed bool `json:"changed"`
	// After 报告按修改合成的配置是否回答 IPv6 地址，DNSMode 是其中的 DNS 模式（没有写时为空）。
	After   bool   `json:"after"`
	DNSMode string `json:"dns_mode,omitempty"`
	// Gateway 是网关开着、After 为 true 时的提醒（gatewayIPv6Warning），否则为空。
	Gateway string `json:"gateway,omitempty"`
	// NoRoute 报告 After 为 true、模式不是 fake-ip，而本机没有 IPv6 出口（netfilter.Rules.IPv6Route）：程序拿到真实
	// 的 IPv6 地址，mihomo 直连它们时连不上。
	NoRoute bool `json:"no_route,omitempty"`
	// Route 报告改为 bypass 时本机有 IPv6 出口：经 IPv6 地址的连接从这里直连出去。
	Route bool `json:"route,omitempty"`
}

// Tun 报告透明代理（TUN）开着，同 IPv6Status.Tun。
func (e *IPv6Edit) Tun() bool { return e.Before.Tun }

// PlanIPv6 准备把 IPv6 设为 set（IPv6Off、IPv6On、IPv6Bypass、IPv6Default），返回修改和按它合成的计划
// （pipeline.PlanIPv6）。off、on、default 同时取消 bypass：on 写 dns.ipv6: true，配置中没有 IPv6 假地址段时也写
// dns.fake-ip-range6: FakeIPRange6；off 写 dns.ipv6: false，并删除 mixin 中的 fake-ip-range6（用不到）；default 删去
// 两者。各键写进现在决定它的 mixin，没有时写进 DNSMixinFile。已经是这样时 Changed 为 false，计划为 nil。
func (i *Installer) PlanIPv6(ctx context.Context, set string) (*IPv6Edit, *pipeline.Plan, error) {
	if !slices.Contains([]string{IPv6Off, IPv6On, IPv6Bypass, IPv6Default}, set) {
		return nil, nil, fmt.Errorf("%w: ipv6 takes %s, %s, %s or %s, got %q", core.ErrInvalidArgument, IPv6Off, IPv6On, IPv6Bypass, IPv6Default, set)
	}
	cur, mixins, err := i.composeActive(ctx)
	if err != nil {
		return nil, nil, err
	}
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	bypassNow := st != nil && st.IPv6Bypass
	e := &IPv6Edit{Before: *ipv6Of(cur, mixins, st), Set: set, Changes: []*MixinChange{}, Bypass: set == IPv6Bypass}
	if set != IPv6Bypass {
		values := map[string]any{dnsIPv6Key: nil, fakeIPRange6Key: nil}
		switch set {
		case IPv6On:
			values[dnsIPv6Key] = true
			if r := cur.KeySource(fakeIPRange6Key); r.Value != "" {
				// 已有的地址段不动（订阅或 mixin 写的）。
				delete(values, fakeIPRange6Key)
			} else {
				values[fakeIPRange6Key] = FakeIPRange6
			}
		case IPv6Off:
			values[dnsIPv6Key] = false
		}
		if e.Changes, err = planKeys(i.Layout.MixinDir(), mixins, fromOf(cur, values), values, DNSMixinFile, dnsHead); err != nil {
			return nil, nil, err
		}
	}
	if e.Changed = len(e.Changes) > 0 || e.Bypass != bypassNow; !e.Changed {
		return e, nil, nil
	}
	plan, err := i.pipeline(nil).PlanIPv6(ctx, mixinsOf(e.Changes), e.Bypass)
	if err != nil {
		return nil, nil, err
	}
	e.After, e.DNSMode = config.DNSIPv6(plan.Config), config.DNSMode(plan.Config)
	if e.After {
		if st != nil && st.Gateway != nil {
			e.Gateway = gatewayIPv6Warning(plan.Config)
		}
		e.NoRoute = e.DNSMode != DNSFakeIP && i.rules().IPv6Route(ctx) == ""
	}
	if e.Bypass {
		e.Route = i.rules().IPv6Route(ctx) != ""
	}
	return e, plan, nil
}

// SaveIPv6 写入 e（PlanIPv6 的结果，已经用户确认）：改 mixin 文件和状态中的开关，再发布 plan（配置没有变化或还没有
// 发布过时只写入），按开关重新加载 inet tunlr。发布失败时文件和开关恢复原状，配置由流水线回滚。
//
// 改为 bypass 而 TUN 开着时，经 IPv6 的 DNS 查询从此被拒绝，随后检查系统的解析：只配了 IPv6 的 DNS 服务器的机器
// 上解析会失败，这时改回原来的设置并发布，返回满足 errors.Is(err, core.ErrRolledBack) 的错误。
func (i *Installer) SaveIPv6(ctx context.Context, e *IPv6Edit, plan *pipeline.Plan) (*config.Revision, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	dir := i.Layout.MixinDir()
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return nil, err
	}
	previous, before := st.IPv6Bypass, tableOptions(st)
	if err := writeChanges(dir, e.Changes); err != nil {
		return nil, err
	}
	st.IPv6Bypass = e.Bypass
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		return nil, errors.Join(err, undoChanges(dir, e.Changes))
	}
	undo := func() error {
		st.IPv6Bypass = previous
		return errors.Join(undoChanges(dir, e.Changes), state.Save(i.Layout.StateFile(), st))
	}
	var rev *config.Revision
	p := i.pipeline(nil)
	if plan != nil && plan.Current != nil && plan.Changed() {
		applied, err := p.ApplyLocked(ctx, plan)
		if err != nil {
			return nil, errors.Join(err, undo())
		}
		if applied != nil {
			rev = applied.Revision
		}
	}
	if tableOptions(st) == before {
		return rev, nil
	}
	if err := i.refreshRules(ctx, st); err != nil {
		return rev, fmt.Errorf("reloading the nftables table inet %s failed: %w", netfilter.Table, err)
	}
	if !e.Bypass {
		return rev, nil
	}
	c := i.checkResolve(ctx)
	if c.Status == StepOK {
		return rev, nil
	}
	err = undo()
	if err == nil {
		var back *pipeline.Plan
		if back, err = p.Plan(ctx); err == nil {
			_, err = p.ApplyLocked(ctx, back)
		}
	}
	err = errors.Join(err, i.refreshRules(ctx, st))
	if err != nil {
		return nil, fmt.Errorf("this server could not look up names with its IPv6 DNS queries rejected (%s), and going back failed: %w", c.Message, err)
	}
	return nil, fmt.Errorf("%w: this server could not look up names with its IPv6 DNS queries rejected (%s); it may have IPv6 DNS servers only, so IPv6 goes through mihomo again",
		core.ErrRolledBack, c.Message)
}

// checkResolve 经系统的解析器解析一个域名（checkDNS），失败时 2 秒后再试一次：刚发布的配置可能还在重建 TUN。
// CheckResolve 不为空时用它（测试时替换）。
func (i *Installer) checkResolve(ctx context.Context) TunCheck {
	check := i.CheckResolve
	if check == nil {
		check = checkDNS
	}
	c := check(ctx)
	if c.Status != StepOK && sleep(ctx, 2*time.Second) == nil {
		c = check(ctx)
	}
	return c
}

// ipv6Bypassed 报告状态中 IPv6 不经 mihomo（tunlr ipv6 bypass）。
func (i *Installer) ipv6Bypassed() bool {
	st, err := state.Load(i.Layout.StateFile())
	return err == nil && st.IPv6Bypass
}

// gatewayIPv6Warning 说明网关开着、配置 cfg 让内核的 DNS 返回 IPv6 地址时的问题，不返回时为空。网关只转发
// IPv4：经路由器有 IPv6 的设备把 IPv6 的连接发给路由器，假地址（fake-ip）连不上、等一会儿才改用 IPv4，真实的
// 地址（redir-host）绕过 mihomo。局域网没有 IPv6 时设备没有 IPv6 路由，立即改用 IPv4。
func gatewayIPv6Warning(cfg []byte) string {
	if !config.DNSIPv6(cfg) {
		return ""
	}
	const head = "mihomo's DNS answers IPv6 addresses (dns.ipv6), but the gateway forwards only IPv4: LAN devices that have IPv6 " +
		"through the router send IPv6 connections to the router, "
	if config.DNSMode(cfg) == DNSFakeIP {
		return head + "where the fake addresses lead nowhere, and fall back to IPv4 only after a delay"
	}
	return head + "which connects them directly, bypassing mihomo"
}

// publishedGatewayIPv6Warning 是按生效的配置的 gatewayIPv6Warning，还没有发布过时为空。
func (i *Installer) publishedGatewayIPv6Warning() string {
	cfg, err := os.ReadFile(i.Layout.CoreConfig())
	if err != nil {
		return ""
	}
	return gatewayIPv6Warning(cfg)
}
