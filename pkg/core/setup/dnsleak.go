package setup

import (
	"context"
	"fmt"
	"strings"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/doctor"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// DNS 泄露检查的 ID（doctor 的 install 组，status 的 dns_leaks）。
const (
	// LeakServers：生效的配置以明文直连问 DNS（config.DNSLeaks）。
	LeakServers = "dns-servers"
	// LeakTunIPv6：TUN 开着，本机的 IPv6 却不经 TUN。
	LeakTunIPv6 = "dns-tun-ipv6"
	// LeakGatewayIPv6：网关开着，局域网有 IPv6，设备可能经路由器问 DNS、连接。
	LeakGatewayIPv6 = "dns-gateway-ipv6"
	// LeakGatewayDevices：设备经网关上网，却没有经它问 DNS。
	LeakGatewayDevices = "dns-gateway-devices"
)

// DNSLeak 是一项可能的 DNS 泄露（tunlr status）：问了哪些域名可能被运营商看到、改写。
type DNSLeak struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// DNSLeaks 返回可能的 DNS 泄露，即 checkDNSLeaks 中为 warn 的项；没有时为空列表。
func (i *Installer) DNSLeaks(ctx context.Context, st *state.State) []DNSLeak {
	c := &doctor.Checker{Group: doctor.GroupInstall}
	i.checkDNSLeaks(ctx, c, st)
	leaks := []DNSLeak{}
	for _, ch := range c.List {
		if ch.Status == doctor.Warn {
			leaks = append(leaks, DNSLeak{ID: strings.TrimPrefix(ch.ID, c.Group+"."), Message: ch.Message, Hint: ch.Hint})
		}
	}
	return leaks
}

// checkDNSLeaks 检查可能的 DNS 泄露，发现的都是 warn：不联网，只看配置、路由和网关记下的流量，主菜单刷新
// status 时也用。问哪些上游总会检查；IPv6 的只在 TUN 开着时，网关的只在网关开着时。
func (i *Installer) checkDNSLeaks(ctx context.Context, c *doctor.Checker, st *state.State) {
	i.checkPlainDNS(ctx, c)
	if st == nil {
		return
	}
	if st.TunEnabled() {
		i.checkTunIPv6(ctx, c)
	}
	if st.Gateway != nil {
		iface, _ := i.lookupInterface(st.Gateway.Interface)
		i.checkGatewayIPv6(c, st.Gateway.Interface, iface)
		i.checkGatewayDNS(ctx, c, iface)
	}
}

// checkPlainDNS 检查生效的配置会不会以明文直连问上游；没有发布过时不出现。现在发布就能改掉时（例如还没有
// 发布改用 tunlr 的 DNS）提示发布。
func (i *Installer) checkPlainDNS(ctx context.Context, c *doctor.Checker) {
	const title = "DNS servers"
	cfg, err := i.pipeline(nil).Current()
	if err != nil || cfg == nil {
		return
	}
	plain, off := config.DNSLeaks(cfg)
	if !off && len(plain) == 0 {
		c.Add(LeakServers, doctor.OK, title, "the published configuration asks no DNS server directly in plain text", "")
		return
	}
	var hint string
	if d, err := i.drift(ctx); err == nil && d != nil && d.State != pipeline.DriftNone && d.State != pipeline.DriftError && !d.DNSLeaks {
		hint = "tunlr config apply publishes a configuration without them"
	} else {
		sub, subDNS := "", ""
		if list, err := subscription.LoadList(i.Layout.SubscriptionList()); err == nil && list.Active != "" {
			sub = list.Active
			if from, file, err := i.SubscriptionDNS(sub); err == nil && from == DNSFromSubscription {
				subDNS = file
			}
		}
		hint = pipeline.DNSLeakHint(off, sub, subDNS)
	}
	c.Add(LeakServers, doctor.Warn, title, config.DNSLeakText(plain, off), hint)
}

// checkTunIPv6 检查 TUN 开着时本机访问 IPv6 公网是否经过 TUN：不经过时，经 IPv6 问的 DNS 和连接都绕过 mihomo。
// 没有 IPv6 路由时为 OK。
func (i *Installer) checkTunIPv6(ctx context.Context, c *doctor.Checker) {
	const title = "IPv6 and TUN"
	switch dev := i.rules().IPv6Egress(ctx); dev {
	case "":
		c.Add(LeakTunIPv6, doctor.OK, title, "this server has no IPv6 route to the internet", "")
	case netfilter.Device:
		c.Add(LeakTunIPv6, doctor.OK, title, "IPv6 traffic goes through "+netfilter.Device, "")
	default:
		c.Add(LeakTunIPv6, doctor.Warn, title,
			"IPv6 traffic leaves through "+dev+", not "+netfilter.Device+": names asked and connections made over IPv6 bypass mihomo",
			"turn IPv6 off on this server, or check that mihomo's TUN has an IPv6 address (tun.inet6-address)")
	}
}

// checkGatewayIPv6 检查网关所在的局域网有没有 IPv6：网关只转发 IPv4，路由器经 IPv6 发给设备的 DNS 服务器和路由
// 都绕过这台服务器。iface 是网卡的情况，找不到时为 nil（checkGateway 报告）。
func (i *Installer) checkGatewayIPv6(c *doctor.Checker, name string, iface *netfilter.Interface) {
	const title = "IPv6 on the LAN"
	if iface == nil {
		return
	}
	if len(iface.IPv6) == 0 {
		c.Add(LeakGatewayIPv6, doctor.OK, title, name+" has no routable IPv6 address, so the LAN likely has no IPv6", "")
		return
	}
	c.Add(LeakGatewayIPv6, doctor.Warn, title,
		"the LAN on "+name+" has IPv6, which the gateway does not forward: devices may ask the router's IPv6 DNS server and connect over IPv6 directly",
		"turn IPv6 off on the router, or at least the IPv6 DNS it hands out (RDNSS, DHCPv6)")
}

// checkGatewayDNS 按 inet tunlr_gateway 记下的最近几分钟的新连接，找出经网关上网、却没有经它问 DNS 的设备：
// 它们多半直接问路由器或别的服务器。用 DoH、DoT 的设备，或这段时间里用的都是缓存的回答时也是这样，所以只说可能。
func (i *Installer) checkGatewayDNS(ctx context.Context, c *doctor.Checker, iface *netfilter.Interface) {
	const title = "DNS of LAN devices"
	uses, err := i.rules().GatewayUses(ctx)
	if err != nil {
		return
	}
	asked := map[string]bool{}
	for _, u := range uses {
		if u.Path == netfilter.PathDNS {
			asked[u.Device] = true
		}
	}
	var silent []string
	for _, d := range recentDevices(uses) {
		if !asked[d] {
			silent = append(silent, d)
		}
	}
	if len(silent) == 0 {
		c.Add(LeakGatewayDevices, doctor.OK, title, "every LAN device that used the gateway in "+recentText+" asked it for DNS", "")
		return
	}
	noun := "device"
	if len(silent) > 1 {
		noun = "devices"
	}
	server := "this server"
	if iface != nil && len(iface.IPv4) > 0 {
		server += " (" + iface.IPv4[0] + ")"
	}
	c.Add(LeakGatewayDevices, doctor.Warn, title,
		fmt.Sprintf("%d LAN %s used the gateway in %s but asked it no DNS (%s): they may ask the router or another server directly",
			len(silent), noun, recentText, strings.Join(silent, ", ")),
		"make "+server+" their DNS server, in the router's DHCP settings or on the device; a device that uses DoH or DoT is fine")
}
