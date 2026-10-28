package setup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// 设备的状态。
const (
	// DeviceOnline 是 DeviceOnlineWindow 内经过本机有流量。
	DeviceOnline = "online"
	// DeviceIdle 是 netfilter.DeviceTimeout 内来过。
	DeviceIdle = "idle"
	// DeviceAway 是记录下来、但最近没有来过的设备。
	DeviceAway = "away"
	// DeviceLAN 是邻居表中、没有使用本机的设备（只在列出全部时出现）。
	DeviceLAN = "lan"
)

// DeviceOnlineWindow 是算作在线的时间。
const DeviceOnlineWindow = 5 * time.Minute

// 设备的走法。
const (
	RouteProxy  = "proxy"
	RouteDirect = state.RouteDirect
)

// Device 是一台局域网设备。
type Device struct {
	Name string `json:"name,omitempty"`
	// NameFromDNS 报告名称来自路由器的反向解析，而不是用户起的。
	NameFromDNS bool   `json:"name_from_dns,omitempty"`
	IP          string `json:"ip"`
	MAC         string `json:"mac,omitempty"`
	// Uses 是使用本机的方式（netfilter.UseGateway、UseProxy）。
	Uses     []string  `json:"uses,omitempty"`
	State    string    `json:"state"`
	LastSeen time.Time `json:"last_seen,omitzero"`
	// Connections 是内核中来自这个地址的当前连接数。
	Connections int `json:"connections"`
	// Route 是 RouteProxy 或 RouteDirect。
	Route string `json:"route"`
	// Saved 报告状态文件中有它的记录。
	Saved bool `json:"saved"`
	// RuleIP 是记录中的地址与现在的地址不同时记录中的地址：直连的规则仍按它匹配。
	RuleIP string `json:"rule_ip,omitempty"`
}

// DeviceList 是 tunlr devices 的结果。
type DeviceList struct {
	// Gateway、ProxyLAN 是网关和代理端口局域网访问的开关；都关闭时没有设备会使用本机。
	Gateway  bool `json:"gateway"`
	ProxyLAN bool `json:"proxy_lan"`
	// Tracking 报告记录设备的 nft 表 inet tunlr_devices 是否在。
	Tracking bool     `json:"tracking"`
	Devices  []Device `json:"devices"`
}

// DeviceOptions 是列出设备的参数。
type DeviceOptions struct {
	// All 同时列出邻居表中没有使用本机的设备。
	All bool
	// Names 向路由器反向解析没有名称的设备。
	Names bool
}

// Devices 列出使用本机的局域网设备（DESIGN.md 8.12）：inet tunlr_devices 记下的、状态文件中记录的，
// opts.All 时加上邻居表中的其他设备。在线的在前，其次按最后出现的时间。
func (i *Installer) Devices(ctx context.Context, opts DeviceOptions) (*DeviceList, error) {
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return nil, err
	}
	now := i.now()
	list := &DeviceList{Gateway: st.Gateway != nil, ProxyLAN: st.ProxyLAN != nil}
	seen, present, err := i.rules().SeenDevices(ctx, now)
	if err != nil && (list.Gateway || list.ProxyLAN) {
		return nil, err
	}
	list.Tracking = present
	neighbors, _ := i.rules().Neighbors(ctx)
	conns, _ := i.connections(ctx)

	byMAC := map[string]*Device{}
	var order []*Device
	for _, s := range seen {
		d := byMAC[s.MAC]
		if d == nil {
			d = &Device{MAC: s.MAC, IP: s.IP, LastSeen: s.LastSeen, Route: RouteProxy}
			byMAC[s.MAC] = d
			order = append(order, d)
		}
		if s.LastSeen.After(d.LastSeen) {
			d.IP, d.LastSeen = s.IP, s.LastSeen
		}
		if !slices.Contains(d.Uses, s.Use) {
			d.Uses = append(d.Uses, s.Use)
		}
	}
	for _, d := range order {
		d.State = DeviceIdle
		if now.Sub(d.LastSeen) <= DeviceOnlineWindow {
			d.State = DeviceOnline
		}
		slices.Sort(d.Uses)
	}
	for _, r := range st.Devices {
		d := findDevice(order, r)
		if d == nil {
			d = &Device{IP: r.IP, MAC: r.MAC, State: DeviceAway}
			if d.MAC == "" {
				if n := neighborByIP(neighbors, r.IP); n != nil {
					d.MAC = n.MAC
				}
			}
			order = append(order, d)
		}
		d.Saved, d.Name, d.Route = true, r.Name, routeOf(r)
		if r.IP != d.IP {
			d.RuleIP = r.IP
		}
	}
	if opts.All {
		for _, n := range neighbors {
			if byMAC[n.MAC] != nil || !lanNeighbor(n) || slices.ContainsFunc(order, func(d *Device) bool { return d.MAC == n.MAC }) {
				continue
			}
			order = append(order, &Device{IP: n.IP, MAC: n.MAC, State: DeviceLAN, Route: RouteProxy})
		}
	}
	for _, d := range order {
		d.Connections = conns[d.IP]
	}
	if opts.Names {
		var ips []string
		for _, d := range order {
			if d.Name == "" {
				ips = append(ips, d.IP)
			}
		}
		if len(ips) > 0 {
			names := i.lookupNames(ctx, ips)
			for _, d := range order {
				if d.Name == "" && names[d.IP] != "" {
					d.Name, d.NameFromDNS = names[d.IP], true
				}
			}
		}
	}
	rank := map[string]int{DeviceOnline: 0, DeviceIdle: 1, DeviceAway: 2, DeviceLAN: 3}
	slices.SortStableFunc(order, func(a, b *Device) int {
		return cmp.Or(cmp.Compare(rank[a.State], rank[b.State]), b.LastSeen.Compare(a.LastSeen), compareIP(a.IP, b.IP))
	})
	list.Devices = make([]Device, len(order))
	for k, d := range order {
		list.Devices[k] = *d
	}
	return list, nil
}

// findDevice 找到记录 r 对应的设备：按 MAC，记录没有 MAC 时按地址。
func findDevice(list []*Device, r state.Device) *Device {
	for _, d := range list {
		if r.MAC != "" && d.MAC == r.MAC || r.MAC == "" && d.IP == r.IP {
			return d
		}
	}
	return nil
}

func neighborByIP(list []netfilter.Neighbor, ip string) *netfilter.Neighbor {
	for k := range list {
		if list[k].IP == ip {
			return &list[k]
		}
	}
	return nil
}

// lanNeighbor 报告邻居是否是局域网中的设备，而不是本机的容器、TUN 等虚拟网卡另一端。
func lanNeighbor(n netfilter.Neighbor) bool {
	if n.Interface == netfilter.Device || !privateAddress(n.IP) {
		return false
	}
	for _, prefix := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "tailscale"} {
		if strings.HasPrefix(n.Interface, prefix) {
			return false
		}
	}
	return true
}

func routeOf(r state.Device) string {
	if r.Route == state.RouteDirect {
		return RouteDirect
	}
	return RouteProxy
}

func compareIP(a, b string) int {
	x, errx := netip.ParseAddr(a)
	y, erry := netip.ParseAddr(b)
	if errx != nil || erry != nil {
		return strings.Compare(a, b)
	}
	return x.Compare(y)
}

// FindDevice 在列表中找到 query（地址、MAC 或名称，名称不区分大小写）所指的设备。query 是列表中没有的
// IPv4 私有地址时返回只有地址的设备，用于预先设置还没来过的设备。找不到时返回满足
// errors.Is(err, core.ErrInvalidArgument) 的错误。
func (l *DeviceList) FindDevice(query string) (*Device, error) {
	q := strings.TrimSpace(query)
	if mac, err := net.ParseMAC(q); err == nil {
		q = mac.String()
	}
	for k := range l.Devices {
		d := &l.Devices[k]
		if d.IP == q || d.MAC != "" && d.MAC == q || d.Name != "" && !d.NameFromDNS && strings.EqualFold(d.Name, q) {
			return d, nil
		}
	}
	// 反向解析得到的名称只在没有同名的自定义名称时使用。
	for k := range l.Devices {
		d := &l.Devices[k]
		if d.NameFromDNS && strings.EqualFold(d.Name, q) {
			return d, nil
		}
	}
	if ip, err := netip.ParseAddr(q); err == nil && ip.Is4() {
		if !privateAddress(q) {
			return nil, fmt.Errorf("%w: %s is not a private address", core.ErrInvalidArgument, q)
		}
		return &Device{IP: q, State: DeviceAway, Route: RouteProxy}, nil
	}
	return nil, fmt.Errorf("%w: no device %q (tunlr devices --all lists them)", core.ErrInvalidArgument, query)
}

// ValidateDeviceName 检查设备名称：不超过 32 个字符，不含控制字符，不能是地址或 MAC。
func ValidateDeviceName(name string) error {
	switch {
	case strings.TrimSpace(name) != name || name == "":
		return errors.New("the name cannot be empty or start or end with spaces")
	case len([]rune(name)) > 32:
		return errors.New("the name is longer than 32 characters")
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return errors.New("the name contains control characters")
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return errors.New("the name cannot be an address")
	}
	if _, err := net.ParseMAC(name); err == nil {
		return errors.New("the name cannot be a MAC address")
	}
	return nil
}

// DeviceEdit 是对一台设备记录的修改，字段为 nil 时不变。
type DeviceEdit struct {
	// Name 为空字符串时去掉名称。
	Name *string
	// Route 是 RouteProxy 或 RouteDirect。
	Route *string
	// Forget 删除记录。
	Forget bool
}

// EditDevices 返回把 edit 用于设备 d 之后的记录，不修改 records。记录的地址更新为 d 现在的地址；没有名称、
// 走法为默认的记录被去掉。名称与其他记录重复时返回满足 errors.Is(err, core.ErrInvalidArgument) 的错误。
func EditDevices(records []state.Device, d *Device, edit DeviceEdit) ([]state.Device, error) {
	out := slices.Clone(records)
	k := slices.IndexFunc(out, func(r state.Device) bool {
		if r.MAC != "" {
			return r.MAC == d.MAC
		}
		return r.IP == d.IP
	})
	if edit.Forget {
		if k >= 0 {
			out = slices.Delete(out, k, k+1)
		}
		return out, nil
	}
	if k < 0 {
		out = append(out, state.Device{})
		k = len(out) - 1
	}
	r := &out[k]
	r.IP, r.MAC = d.IP, d.MAC
	if edit.Name != nil {
		if *edit.Name != "" {
			if err := ValidateDeviceName(*edit.Name); err != nil {
				return nil, fmt.Errorf("%w: %w", core.ErrInvalidArgument, err)
			}
			for j, other := range out {
				if j != k && strings.EqualFold(other.Name, *edit.Name) {
					return nil, fmt.Errorf("%w: another device is already called %q", core.ErrInvalidArgument, other.Name)
				}
			}
		}
		r.Name = *edit.Name
	}
	if edit.Route != nil {
		switch *edit.Route {
		case RouteDirect:
			r.Route = state.RouteDirect
		case RouteProxy:
			r.Route = ""
		default:
			return nil, fmt.Errorf("%w: the route must be proxy or direct, got %q", core.ErrInvalidArgument, *edit.Route)
		}
	}
	if r.Name == "" && r.Route == "" {
		out = slices.Delete(out, k, k+1)
	}
	return out, nil
}

// PlanDeviceEdit 把 edit 用于设备 d，返回新的记录和按它合成的计划（pipeline.PlanDevices）。
func (i *Installer) PlanDeviceEdit(ctx context.Context, d *Device, edit DeviceEdit) ([]state.Device, *pipeline.Plan, error) {
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return nil, nil, err
	}
	records, err := EditDevices(st.Devices, d, edit)
	if err != nil {
		return nil, nil, err
	}
	plan, err := i.pipeline(nil).PlanDevices(ctx, records)
	if err != nil {
		return nil, nil, err
	}
	return records, plan, nil
}

// SaveDevices 保存设备记录，并发布 plan（pipeline.PlanDevices(ctx, records) 的结果，已经用户确认；配置
// 没有变化时为 nil 或没有改动）。发布失败时记录恢复原状，配置由流水线回滚。
func (i *Installer) SaveDevices(ctx context.Context, records []state.Device, plan *pipeline.Plan) (*config.Revision, error) {
	return i.saveAndApply(ctx, plan, func(st *state.State) func() {
		previous := st.Devices
		st.Devices = records
		return func() { st.Devices = previous }
	})
}

// saveAndApply 在锁内用 change 修改并保存状态，再发布 plan（已经用户确认；配置没有变化时为 nil 或没有改动）。
// change 返回撤销修改的函数：发布失败时用它恢复状态，配置由流水线回滚。
func (i *Installer) saveAndApply(ctx context.Context, plan *pipeline.Plan, change func(*state.State) (undo func())) (*config.Revision, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return nil, err
	}
	undo := change(st)
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		return nil, err
	}
	if plan == nil || !plan.Changed() {
		return nil, nil
	}
	applied, err := i.pipeline(nil).ApplyLocked(ctx, plan)
	if err != nil {
		undo()
		return nil, errors.Join(err, state.Save(i.Layout.StateFile(), st))
	}
	if applied == nil {
		return nil, nil
	}
	return applied.Revision, nil
}

// DeviceSummary 是 status 中的设备摘要。
type DeviceSummary struct {
	Online int `json:"online"`
	Idle   int `json:"idle"`
}

// deviceSummary 在网关或代理端口的局域网访问开启时汇总设备，否则返回 nil。
func (i *Installer) deviceSummary(ctx context.Context, st *state.State) *DeviceSummary {
	if st == nil || st.Gateway == nil && st.ProxyLAN == nil {
		return nil
	}
	list, err := i.Devices(ctx, DeviceOptions{})
	if err != nil {
		return nil
	}
	s := &DeviceSummary{}
	for _, d := range list.Devices {
		switch d.State {
		case DeviceOnline:
			s.Online++
		case DeviceIdle:
			s.Idle++
		}
	}
	return s
}

// SyncDevices 让记录设备的 nft 表与开关一致：网关或代理端口的局域网访问开启时加载它，并更新其中的局域网
// 网卡和代理端口（端口取自生效的配置）；都关闭时删除。已记下的设备保留。
func (i *Installer) SyncDevices(ctx context.Context) error {
	st, err := state.Load(i.Layout.StateFile())
	if err != nil {
		return ignoreNotExist(err)
	}
	return i.syncDevices(ctx, st)
}

func (i *Installer) syncDevices(ctx context.Context, st *state.State) error {
	var ifaces []string
	var ports []int
	if st != nil && st.Gateway != nil {
		ifaces = append(ifaces, st.Gateway.Interface)
	}
	if st != nil && st.ProxyLAN != nil {
		if cfg, err := i.pipeline(nil).Current(); err == nil && cfg != nil {
			if l, err := config.Listen(cfg); err == nil && l.MixedPort > 0 {
				ports = append(ports, l.MixedPort)
			}
		}
	}
	return i.rules().DevicesSync(ctx, ifaces, ports)
}

// repairDevices 在网关或代理端口的局域网访问开启、而记录设备的表不见了时重新加载（_prestart、_check），
// 返回采取的动作。
func (i *Installer) repairDevices(ctx context.Context, st *state.State) (string, error) {
	if st == nil || st.Gateway == nil && st.ProxyLAN == nil {
		return "", nil
	}
	tables, err := i.rules().Tables(ctx)
	if err != nil || tables[netfilter.DevicesTable] {
		return "", err
	}
	if err := i.syncDevices(ctx, st); err != nil {
		return "", err
	}
	return "reloaded the nftables table inet " + netfilter.DevicesTable, nil
}

func (i *Installer) connections(ctx context.Context) (map[string]int, error) {
	if i.Connections != nil {
		return i.Connections(ctx)
	}
	return mihomo.NewClient(i.Layout.CoreSocket()).ConnectionsBySource(ctx)
}

// lookupNames 向路由器（IPv4 默认路由的下一跳）反向解析 ips，得到名称的第一段。查不到的不在结果中。
// 连接打上工具的标记，透明代理开启时不经过内核（否则内核会劫持 DNS）。
func (i *Installer) lookupNames(ctx context.Context, ips []string) map[string]string {
	if i.LookupNames != nil {
		return i.LookupNames(ctx, ips)
	}
	router := i.rules().DefaultGateway(ctx)
	if router == "" {
		return nil
	}
	dialer := netfilter.MarkedDialer()
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, net.JoinHostPort(router, "53"))
	}}
	// 路由器不提供 DNS 时每次都要等到超时，因此时限很短；家用路由器在局域网内的应答通常只要几毫秒。
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	names := map[string]string{}
	for _, ip := range ips {
		wg.Go(func() {
			list, err := r.LookupAddr(ctx, ip)
			if err != nil || len(list) == 0 {
				return
			}
			name, _, _ := strings.Cut(strings.TrimSuffix(list[0], "."), ".")
			if name == "" {
				return
			}
			mu.Lock()
			names[ip] = name
			mu.Unlock()
		})
	}
	wg.Wait()
	return names
}
