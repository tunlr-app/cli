package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
	"github.com/tunlr-app/cli/templates"
)

// DNSMixinFile 是 tunlr dns 写入的共用 mixin：DNS 模式、问谁和你加进 fake-ip-filter 的条目都在这里，除非
// 别的 mixin 已经写了那个键（命令改的是现在决定它的文件）。排在多数共用文件之后，它追加的 fake-ip-filter 条目
// 不会被那些文件用 mihomo: 整份替换的列表盖掉。
const DNSMixinFile = "90-dns.yaml"

// dnsHead 是 DNSMixinFile 开头的注释。
const dnsHead = "DNS settings for all subscriptions, written by tunlr dns: the mode (dns.enhanced-mode), the servers\n" +
	"(dns.nameserver, the geosite:cn entry of dns.nameserver-policy, dns.proxy-server-nameserver) and the names\n" +
	"added to dns.fake-ip-filter. You may edit it too; tunlr dns shows what you write here. What is not here\n" +
	"comes from tunlr's fallbacks, or the subscription's with tunlr sub set <sub> --dns sub (tunlr config shows them)."

// SubscriptionDNSMixinFile 是 SetSubscriptionDNS 在订阅的 mixin 目录中写入的文件名。
const SubscriptionDNSMixinFile = "20-dns.yaml"

// 订阅用谁的 DNS（tunlr sub set <订阅> --dns）。
const (
	// DNSFromTunlr 用 tunlr 的 DNS 和嗅探（兜底值，默认）。
	DNSFromTunlr = "tunlr"
	// DNSFromSubscription 采用订阅写的 dns、sniffer（tunlr.sub-dns: true），订阅没写的用 tunlr 的。
	DNSFromSubscription = "sub"
)

// DNSWrite 是 SetSubscriptionDNS 写了什么、写到了哪里。
type DNSWrite struct {
	// From 是 DNSFromTunlr 或 DNSFromSubscription。
	From string `json:"from"`
	// Changes 是改了的 mixin 文件，已经是这样时为空。
	Changes      []*MixinChange `json:"changes"`
	Subscription string         `json:"subscription"`
	// Active 报告订阅是否为启用的订阅：是时调用方随后发布配置，否则修改在它启用时生效。
	Active bool `json:"active"`
}

// SubscriptionDNS 返回订阅 sub 启用时用谁的 DNS：它的 mixin（共用的和它自己的）中最后写了 tunlr.sub-dns 的为 true
// 时为 DNSFromSubscription，否则为 DNSFromTunlr；file 是那个文件，都没写时为空。
func (i *Installer) SubscriptionDNS(sub string) (from, file string, err error) {
	mixins, err := config.LoadMixinsFor(i.Layout.MixinDir(), sub)
	if err != nil {
		return "", "", err
	}
	from = DNSFromTunlr
	for _, m := range mixins {
		set, on, err := config.MixinSubDNS(m.Name, m.Data)
		if err != nil {
			return "", "", err
		}
		if set {
			from, file = DNSFromTunlr, m.Name
			if on {
				from = DNSFromSubscription
			}
		}
	}
	return from, file, nil
}

// SetSubscriptionDNS 设置订阅 sub 用谁的 DNS，只在它启用时生效，只改它自己的 mixin（共用的不动）：
// DNSFromSubscription 在决定它的那个订阅 mixin 中写 tunlr.sub-dns: true，没有时写进 mixin.d/<sub>/
// SubscriptionDNSMixinFile；DNSFromTunlr 从它的 mixin 中删去 tunlr.sub-dns，共用的 mixin 仍让它采用订阅的时
// 写 false。只写文件，配置由调用方随后发布。
func (i *Installer) SetSubscriptionDNS(ctx context.Context, sub, from string) (*DNSWrite, error) {
	if from != DNSFromSubscription && from != DNSFromTunlr {
		return nil, fmt.Errorf("%w: --dns takes %s or %s, got %q", core.ErrInvalidArgument, DNSFromTunlr, DNSFromSubscription, from)
	}
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	list, err := subscription.LoadList(i.Layout.SubscriptionList())
	if err != nil {
		return nil, err
	}
	if list.Find(sub) == nil {
		return nil, list.NotFound(sub)
	}
	dir := i.Layout.MixinDir()
	mixins, err := config.LoadMixinsFor(dir, sub)
	if err != nil {
		return nil, err
	}
	// 共用的 mixin 在前，订阅自己的在后；last 是最后写了 sub-dns 的，shared 是共用的 mixin 合起来的结果。
	var last string
	var shared bool
	for _, m := range mixins {
		set, on, err := config.MixinSubDNS(m.Name, m.Data)
		if err != nil {
			return nil, err
		}
		if set {
			last = m.Name
			if !strings.Contains(m.Name, "/") {
				shared = on
			}
		}
	}
	own := sub + "/" + SubscriptionDNSMixinFile
	values := map[string]map[string]any{}
	if from == DNSFromSubscription {
		target := own
		if strings.HasPrefix(last, sub+"/") {
			target = last
		}
		values[target] = map[string]any{subDNSKey: true}
	} else {
		for _, m := range mixins {
			if set, _, _ := config.MixinSubDNS(m.Name, m.Data); set && strings.HasPrefix(m.Name, sub+"/") {
				values[m.Name] = map[string]any{subDNSKey: nil}
			}
		}
		if shared {
			values[own] = map[string]any{subDNSKey: false}
		}
	}
	head := "DNS settings for subscription " + sub + ", applied only while it is active. sub-dns: true takes the\n" +
		"dns and sniffer the subscription sets; without it tunlr's are used (tunlr:defaults, listed by tunlr config).\n" +
		"Written by tunlr sub set " + sub + " --dns; you may edit it too."
	w := &DNSWrite{From: from, Changes: []*MixinChange{}, Subscription: sub, Active: sub == list.Active}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		c := &MixinChange{Name: name, Path: config.MixinPath(dir, name)}
		if j := slices.IndexFunc(mixins, func(m config.Mixin) bool { return m.Name == name }); j >= 0 {
			c.original, c.existed = mixins[j].Data, true
		}
		h := ""
		if name == own {
			h = head
		}
		if c.data, err = config.SetTunlrKeys(c.original, h, values[name]); err != nil {
			return nil, fmt.Errorf("mixin %s: %w", name, err)
		}
		if c.existed && c.data != nil && string(c.data) == string(c.original) {
			continue
		}
		c.Removed = c.data == nil
		if v := values[name][subDNSKey]; v != nil {
			c.Set = []string{"tunlr." + subDNSKey}
		} else {
			c.Unset = []string{"tunlr." + subDNSKey}
		}
		w.Changes = append(w.Changes, c)
	}
	if err := writeChanges(dir, w.Changes); err != nil {
		return nil, err
	}
	return w, nil
}

// subDNSKey 是 mixin 的 tunlr: 中采用订阅的 DNS 的开关（config 中的同名键）。
const subDNSKey = "sub-dns"

// DNS 模式（dns.enhanced-mode）的取值，与内核相同。
const (
	DNSFakeIP    = "fake-ip"
	DNSRedirHost = "redir-host"
)

// dnsModeKey 是 DNS 模式在配置中的键。
const dnsModeKey = "dns.enhanced-mode"

// DNSMode 是按启用的订阅合成的配置中的 DNS 模式和决定它的地方。
type DNSMode struct {
	// Mode 是 dns.enhanced-mode，没有写时为空（内核按 redir-host 处理）。
	Mode string `json:"mode"`
	// From、Fallback 同 config.KeySource：最后写它的 mixin 文件（如 90-dns.yaml、home/30-x.yaml）、subscription，
	// 或兜底值所在的层（通常是 tunlr:defaults）。
	From     string `json:"from,omitempty"`
	Fallback bool   `json:"fallback,omitempty"`
	// Subscription 是启用的订阅：From 是它的 mixin 时，模式只在它启用时这样。
	Subscription string `json:"subscription,omitempty"`
	// Files 是共用的和启用的订阅的 mixin 中写了它的文件，按应用的顺序；dns mode default 从这些文件中删除它。
	Files []string `json:"files,omitempty"`
}

// Pinned 报告模式由 mixin 写定（tunlr dns mode fake-ip|redir-host 写的，或手写的），而不是来自订阅或兜底值。
func (d *DNSMode) Pinned() bool { return !d.Fallback && slices.Contains(d.Files, d.From) }

// DNSMode 返回按启用的订阅合成的配置中的 DNS 模式和决定它的地方。
func (i *Installer) DNSMode(ctx context.Context) (*DNSMode, error) {
	d, _, err := i.dnsMode(ctx)
	return d, err
}

// dnsMode 是 DNSMode，另外返回共用的和启用的订阅的 mixin。
func (i *Installer) dnsMode(ctx context.Context) (*DNSMode, []config.Mixin, error) {
	plan, mixins, err := i.composeActive(ctx)
	if err != nil {
		return nil, nil, err
	}
	return modeOf(plan, mixins), mixins, nil
}

// modeOf 返回 plan（按启用的订阅合成的）中的 DNS 模式，mixins 是共用的和它的 mixin。
func modeOf(plan *pipeline.Plan, mixins []config.Mixin) *DNSMode {
	src := plan.KeySource(dnsModeKey)
	d := &DNSMode{Mode: src.Value, From: src.From, Fallback: src.Fallback, Subscription: plan.Subscription}
	for _, m := range mixins {
		if config.MixinSets(m.Data, dnsModeKey) {
			d.Files = append(d.Files, m.Name)
		}
	}
	return d
}

// DNSStatus 是 tunlr dns 显示的 DNS 设置，按启用的订阅合成。
type DNSStatus struct {
	Mode    DNSMode    `json:"mode"`
	Servers DNSServers `json:"servers"`
	// FakeIPFilter 是你加进 dns.fake-ip-filter 的条目（DNSMixinFile 中的 tunlr.fake-ip-filter.append）。
	FakeIPFilter []string `json:"fake_ip_filter"`
	// SubDNS 是让启用的订阅采用它自己的 DNS 的 mixin（tunlr.sub-dns: true），用 tunlr 的时为空。
	SubDNS string `json:"sub_dns,omitempty"`
	// Enabled 报告内核的 DNS 是否开启（dns.enable，tunlr 的兜底值开启它）；关闭时这些设置都用不到。
	Enabled bool `json:"enabled"`
	// Tun 报告透明代理（TUN）的开关是否打开；关着时模式和 fake-ip-filter 用不到，问谁照样用到。
	Tun bool `json:"tun"`
}

// DNSStatus 返回 DNS 模式、问谁和你加进 fake-ip-filter 的条目，只合成一次配置。
func (i *Installer) DNSStatus(ctx context.Context) (*DNSStatus, error) {
	servers, plan, mixins, err := i.dnsServers(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := i.FakeIPFilterEntries()
	if err != nil {
		return nil, err
	}
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return &DNSStatus{Mode: *modeOf(plan, mixins), Servers: *servers, FakeIPFilter: orEmpty(entries), SubDNS: plan.SubDNS,
		Enabled: config.DNSEnabled(plan.Config), Tun: st != nil && st.TunEnabled()}, nil
}

// composeActive 按启用的订阅合成配置（pipeline.Compose），并读出共用的和它的 mixin。
func (i *Installer) composeActive(ctx context.Context) (*pipeline.Plan, []config.Mixin, error) {
	plan, err := i.pipeline(nil).Compose(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	mixins, err := config.LoadMixinsFor(i.Layout.MixinDir(), plan.Subscription)
	if err != nil {
		return nil, nil, err
	}
	return plan, mixins, nil
}

// fromOf 返回 values 中各键现在由哪个 mixin 决定（plan.KeySource），兜底值的不列，见 planKeys。
func fromOf(plan *pipeline.Plan, values map[string]any) map[string]string {
	from := map[string]string{}
	for k := range values {
		if s := plan.KeySource(k); !s.Fallback {
			from[k] = s.From
		}
	}
	return from
}

// DNSModeEdit 是 PlanDNSMode 准备的修改，由 SaveDNSMode 写入。
type DNSModeEdit struct {
	Before DNSMode `json:"before"`
	// Mode 是要设的模式；为空时从 Before.Files 中删除它，交给订阅或兜底值。
	Mode string `json:"mode"`
	// Changes 是要改的 mixin 文件；为空时已经是这样。
	Changes []*MixinChange `json:"changes"`
	// SubDNS 是采用订阅的 DNS 的 mixin（tunlr.sub-dns），Lacks 是改为 redir-host 时它缺的设置（config.DNSLacks）；
	// 用 tunlr 的 DNS 或不是改为 redir-host 时都为空。
	SubDNS string   `json:"sub_dns,omitempty"`
	Lacks  []string `json:"lacks,omitempty"`
}

// PlanDNSMode 准备把 DNS 模式设为 mode（DNSFakeIP、DNSRedirHost；空字符串为交给订阅或兜底值），返回修改和按它
// 合成的计划（pipeline.PlanWithMixins）。模式写进现在决定它的 mixin，没有时写进 DNSMixinFile；已经是这样时
// Changes 为空，计划为 nil。
func (i *Installer) PlanDNSMode(ctx context.Context, mode string) (*DNSModeEdit, *pipeline.Plan, error) {
	if mode != "" && mode != DNSFakeIP && mode != DNSRedirHost {
		return nil, nil, fmt.Errorf("%w: unknown DNS mode %q (%s, %s or default)", core.ErrInvalidArgument, mode, DNSFakeIP, DNSRedirHost)
	}
	before, mixins, err := i.dnsMode(ctx)
	if err != nil {
		return nil, nil, err
	}
	e := &DNSModeEdit{Before: *before, Mode: mode, Changes: []*MixinChange{}}
	if (mode != "" && mode == before.Mode && before.Pinned()) || (mode == "" && len(before.Files) == 0) {
		return e, nil, nil
	}
	values := map[string]any{dnsModeKey: nil}
	if mode != "" {
		values[dnsModeKey] = mode
	}
	from := map[string]string{}
	if !before.Fallback {
		from[dnsModeKey] = before.From
	}
	if e.Changes, err = planKeys(i.Layout.MixinDir(), mixins, from, values, DNSMixinFile, dnsHead); err != nil {
		return nil, nil, err
	}
	plan, err := i.pipeline(nil).PlanWithMixins(ctx, mixinsOf(e.Changes))
	if err != nil {
		return nil, nil, err
	}
	if mode == DNSRedirHost && plan.SubDNS != "" {
		if e.Lacks = config.DNSLacks(plan.Config); len(e.Lacks) > 0 {
			e.SubDNS = plan.SubDNS
		}
	}
	return e, plan, nil
}

// SaveDNSMode 写入 e（PlanDNSMode 的结果，已经用户确认），再发布 plan（配置没有变化或还没有发布过时只写入）。
// 发布失败时文件恢复原状，配置由流水线回滚。
func (i *Installer) SaveDNSMode(ctx context.Context, e *DNSModeEdit, plan *pipeline.Plan) (*config.Revision, error) {
	return i.publishChanges(ctx, e.Changes, plan)
}

// 问谁的设置在配置中的键：中国的域名问 nameserver-policy 中 geosite:cn 的（经直连），节点的域名也问它们（经直连），
// 其他的问 nameserver（按规则）。
const (
	chinaPolicyKey = "dns.nameserver-policy." + config.ChinaPolicy
	proxyServerKey = "dns.proxy-server-nameserver"
	nameserverKey  = "dns.nameserver"
)

// directSuffix 是让 DNS 服务器经直连访问的写法。
const directSuffix = "#DIRECT"

// DNSServerList 是一组 DNS 服务器和它们来自哪里。
type DNSServerList struct {
	Servers []string `json:"servers"`
	// From、Fallback 同 config.KeySource。
	From     string `json:"from,omitempty"`
	Fallback bool   `json:"fallback,omitempty"`
	// Default 是 tunlr 的兜底值。
	Default []string `json:"default"`
}

// DNSServers 是按启用的订阅合成的配置中问谁。
type DNSServers struct {
	// China 问中国的域名（nameserver-policy 中 geosite:cn 的，去掉了 #DIRECT），也解析节点的域名
	// （proxy-server-nameserver）；Other 问其他的（nameserver）。
	China DNSServerList `json:"china"`
	Other DNSServerList `json:"other"`
}

// DNSServers 返回按启用的订阅合成的配置中问谁，以及 tunlr 的兜底值。
func (i *Installer) DNSServers(ctx context.Context) (*DNSServers, error) {
	s, _, _, err := i.dnsServers(ctx)
	return s, err
}

// dnsServers 是 DNSServers，另外返回合成的计划和共用的、启用的订阅的 mixin。
func (i *Installer) dnsServers(ctx context.Context) (*DNSServers, *pipeline.Plan, []config.Mixin, error) {
	plan, mixins, err := i.composeActive(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	defaults, err := config.Compose(config.Layers{Template: templates.Config, Managed: []config.Managed{pipeline.DefaultsManaged()}})
	if err != nil {
		return nil, nil, nil, err
	}
	china, other := config.DNSServers(plan.Config)
	dChina, dOther := config.DNSServers(defaults.Config)
	list := func(servers, def []string, key string) DNSServerList {
		src := plan.KeySource(key)
		return DNSServerList{Servers: orEmpty(servers), From: src.From, Fallback: src.Fallback, Default: orEmpty(def)}
	}
	s := &DNSServers{China: list(withoutDirect(china), withoutDirect(dChina), chinaPolicyKey), Other: list(other, dOther, nameserverKey)}
	return s, plan, mixins, nil
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// withoutDirect 去掉各项末尾的 #DIRECT（问中国的域名的服务器都经直连，文本框中不写它）。
func withoutDirect(servers []string) []string {
	out := make([]string, len(servers))
	for i, s := range servers {
		out[i] = strings.TrimSuffix(s, directSuffix)
	}
	return out
}

// withDirect 给没有写 # 的服务器加上 #DIRECT；system、dhcp:// 用本机的设置，不加。
func withDirect(servers []string) []string {
	out := make([]string, len(servers))
	for i, s := range servers {
		out[i] = s
		if !strings.Contains(s, "#") && s != "system" && !strings.HasPrefix(s, "dhcp://") {
			out[i] = s + directSuffix
		}
	}
	return out
}

// dnsSchemes 是 DNS 服务器地址可用的协议。
var dnsSchemes = []string{"udp", "tcp", "tls", "https", "quic", "h3", "dhcp"}

// ParseDNSServers 把一行一个的文字拆成 DNS 服务器：去掉首尾空白和空行，重复的只留第一个。每一个是 IP（可带端口）、
// system，或 udp://、tcp://、tls://、https://、quic://、h3://、dhcp:// 开头的地址，可以带 # 后的参数；写错时的
// 错误可以直接给用户看（文本框中的提示）。
func ParseDNSServers(text string) ([]string, error) {
	var out []string
	for line := range strings.Lines(text) {
		s := strings.TrimSpace(line)
		if s == "" || slices.Contains(out, s) {
			continue
		}
		if !validDNSServer(s) {
			return nil, fmt.Errorf("%q is not a DNS server; one per line, such as 223.5.5.5, tls://1.1.1.1 or https://dns.google/dns-query", s)
		}
		out = append(out, s)
	}
	return out, nil
}

func validDNSServer(s string) bool {
	addr, _, _ := strings.Cut(s, "#")
	if addr == "system" {
		return true
	}
	if _, err := netip.ParseAddr(addr); err == nil {
		return true
	}
	if _, err := netip.ParseAddrPort(addr); err == nil {
		return true
	}
	u, err := url.Parse(addr)
	return err == nil && slices.Contains(dnsSchemes, u.Scheme) && u.Host != ""
}

// DNSServersChange 是对问谁的修改。China、Other 为 nil 时不改，为空时删除你的设置（回到订阅或兜底值）；Reset 删除
// 你对两者的设置。
type DNSServersChange struct {
	China, Other []string
	Reset        bool
}

// DNSServersEdit 是 PlanDNSServers 准备的修改，由 SaveDNSServers 写入。
type DNSServersEdit struct {
	Before DNSServers `json:"before"`
	// Changes 是要改的 mixin 文件；为空时已经是这样。
	Changes []*MixinChange `json:"changes"`
}

// PlanDNSServers 准备按 ch 修改问谁，返回修改和按它合成的计划（pipeline.PlanWithMixins）。China 写进
// nameserver-policy 的 geosite:cn（加上 #DIRECT）和 proxy-server-nameserver，Other 写进 nameserver，各自写进
// 现在决定它的 mixin，没有时写进 DNSMixinFile；与现在的相同时不写。已经是这样时 Changes 为空，计划为 nil。
func (i *Installer) PlanDNSServers(ctx context.Context, ch DNSServersChange) (*DNSServersEdit, *pipeline.Plan, error) {
	before, cur, mixins, err := i.dnsServers(ctx)
	if err != nil {
		return nil, nil, err
	}
	values := map[string]any{}
	switch {
	case ch.Reset:
		values[chinaPolicyKey], values[proxyServerKey], values[nameserverKey] = nil, nil, nil
	default:
		switch {
		case ch.China == nil:
		case len(ch.China) == 0:
			values[chinaPolicyKey], values[proxyServerKey] = nil, nil
		case !slices.Equal(ch.China, before.China.Servers):
			values[chinaPolicyKey], values[proxyServerKey] = withDirect(ch.China), ch.China
		}
		switch {
		case ch.Other == nil:
		case len(ch.Other) == 0:
			values[nameserverKey] = nil
		case !slices.Equal(ch.Other, before.Other.Servers):
			values[nameserverKey] = ch.Other
		}
	}
	e := &DNSServersEdit{Before: *before, Changes: []*MixinChange{}}
	if len(values) == 0 {
		return e, nil, nil
	}
	if e.Changes, err = planKeys(i.Layout.MixinDir(), mixins, fromOf(cur, values), values, DNSMixinFile, dnsHead); err != nil {
		return nil, nil, err
	}
	if len(e.Changes) == 0 {
		return e, nil, nil
	}
	plan, err := i.pipeline(nil).PlanWithMixins(ctx, mixinsOf(e.Changes))
	if err != nil {
		return nil, nil, err
	}
	return e, plan, nil
}

// SaveDNSServers 写入 e（PlanDNSServers 的结果，已经用户确认），再发布 plan，同 SaveDNSMode。
func (i *Installer) SaveDNSServers(ctx context.Context, e *DNSServersEdit, plan *pipeline.Plan) (*config.Revision, error) {
	return i.publishChanges(ctx, e.Changes, plan)
}

// RevertDNSServers 把 e（已经由 SaveDNSServers 写入并发布）改的文件改回原样，再发布按它们合成的配置。用于发布后
// 解析不通时放回原来的服务器。
func (i *Installer) RevertDNSServers(ctx context.Context, e *DNSServersEdit) (*config.Revision, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	if err := undoChanges(i.Layout.MixinDir(), e.Changes); err != nil {
		return nil, err
	}
	p := i.pipeline(nil)
	plan, err := p.Plan(ctx)
	if err != nil {
		return nil, err
	}
	applied, err := p.ApplyLocked(ctx, plan)
	if err != nil || applied == nil {
		return nil, err
	}
	return applied.Revision, nil
}

// DNSCheck 是经内核解析一个域名的结果。
type DNSCheck struct {
	// Kind 是 china（问中国的域名的服务器）或 other。
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Addresses []string `json:"addresses,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// dnsCheckNames 是检查问谁时解析的域名：一个在 geosite:cn 中，一个不在。
var dnsCheckNames = []struct{ kind, name string }{{"china", "www.baidu.com"}, {"other", "www.google.com"}}

// ErrDNSOff 表示生效的配置关闭了内核的 DNS（dns.enable），问谁的设置用不到，也无法经内核检查。
var ErrDNSOff = errors.New("mihomo's DNS is off (dns.enable is false)")

// CheckDNS 经运行中的内核解析一个中国的域名和一个境外的域名（GET /dns/query，按配置中的 nameserver-policy 和规则
// 问上游，不返回 fake-ip），检查问谁的设置能用。内核没有运行时返回 mihomo.ErrNotRunning，生效的配置没有开启 DNS
// 时返回 ErrDNSOff。
func (i *Installer) CheckDNS(ctx context.Context) ([]DNSCheck, error) {
	c := mihomo.NewClient(i.Layout.CoreSocket())
	if _, err := c.Version(ctx); err != nil {
		return nil, err
	}
	cfg, err := os.ReadFile(i.Layout.CoreConfig())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if !config.DNSEnabled(cfg) {
		return nil, ErrDNSOff
	}
	var out []DNSCheck
	for _, n := range dnsCheckNames {
		r := DNSCheck{Kind: n.kind, Name: n.name}
		addrs, err := c.Resolve(ctx, n.name)
		if err != nil {
			if errors.Is(err, mihomo.ErrNotRunning) {
				return nil, err
			}
			r.Error = err.Error()
		}
		r.Addresses = addrs
		out = append(out, r)
	}
	return out, nil
}

// readMixinFile 读出 mixin name 的内容，不存在时为 nil。
func (i *Installer) readMixinFile(name string) ([]byte, error) {
	data, err := os.ReadFile(config.MixinPath(i.Layout.MixinDir(), name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}
