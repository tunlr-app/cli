package setup

import (
	"context"
	"errors"
	"io/fs"
	"sync"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// Status 汇总 tunlr 的当前状态。版本等一律以内核实际情况为准，工具的记录只作为已知可用的历史。
type Status struct {
	// Installed 报告内核服务是否已安装（tunlr init）。
	Installed bool `json:"installed"`
	// Service 是 tunlr-core.service 的状态，服务未安装时为 nil。
	Service *service.Status `json:"service,omitempty"`
	// Running 是经 API 查询到的运行中的内核版本，内核没有响应时为空。
	Running string `json:"running,omitempty"`
	// Kernel 是内核的安装情况和已知可用版本。
	Kernel *mihomo.KernelStatus `json:"kernel"`
	// Subscription 是启用的订阅。
	Subscription string `json:"subscription,omitempty"`
	// SubDNS 是让启用的订阅采用它自己的 DNS 的 mixin（tunlr.sub-dns: true），用 tunlr 的 DNS 时为空。
	SubDNS string `json:"sub_dns,omitempty"`
	// IPv6Bypass 报告 IPv6 不经 mihomo（tunlr ipv6 bypass）：经 IPv6 地址的连接直连，doctor 不算泄露。
	IPv6Bypass bool `json:"ipv6_bypass,omitempty"`
	// Revision 是与生效配置一致的历史修订，没有发布过或手工改过时为 nil。
	Revision *config.Revision `json:"revision,omitempty"`
	// Updates 是各定时更新任务的周期和执行情况。
	Updates []Schedule `json:"updates"`
	// Tun 是透明代理的状态。
	Tun *TunStatus `json:"tun"`
	// PanelLAN 是面板的局域网访问开关。
	PanelLAN bool `json:"panel_lan"`
	// ProxyLAN 是代理端口的局域网访问开关。
	ProxyLAN bool `json:"proxy_lan"`
	// LANAccess 是代理端口和面板放行的入口，两个开关都关闭时为 nil。
	LANAccess *LANAccess `json:"lan_access,omitempty"`
	// Drift 是生效的配置与现在发布会得到的配置是否一致，没有发布过时为 nil。
	Drift *pipeline.Drift `json:"drift,omitempty"`
	// Devices 汇总使用本机的局域网设备，网关和代理端口的局域网访问都关闭时为 nil。
	Devices *DeviceSummary `json:"devices,omitempty"`
	// DNSLeaks 是可能的 DNS 泄露（doctor 中 dns-* 的 warn 项），没有时为空列表。
	DNSLeaks []DNSLeak `json:"dns_leaks"`
	// Filter 是全局过滤项，还没有状态文件时为 nil。
	Filter *FilterStatus `json:"filter,omitempty"`
	// UpdateCheck 是最近一次版本查询的结果和据此可用的更新，还没有查询过时为 nil。
	UpdateCheck *UpdateCheck `json:"update_check,omitempty"`
}

// driftCache 缓存最近一次计算的漂移：合成一次配置要解析整份订阅，主菜单每 10 秒刷新一次状态，
// 输入没有变化（见 pipeline.DriftKey）时不重新合成。
var driftCache struct {
	sync.Mutex
	key   string
	drift *pipeline.Drift
}

// drift 返回漂移，输入没有变化时用缓存的结果。
func (i *Installer) drift(ctx context.Context) (*pipeline.Drift, error) {
	p := i.pipeline(nil)
	key, err := p.DriftKey()
	if err != nil {
		return nil, err
	}
	driftCache.Lock()
	defer driftCache.Unlock()
	if key == driftCache.key {
		return driftCache.drift, nil
	}
	d, err := p.Drift(ctx)
	if err != nil {
		return nil, err
	}
	driftCache.key, driftCache.drift = key, d
	return d, nil
}

// CoreStatus 是内核服务的状态（tunlr core），字段与 Status 中的同名字段相同。
type CoreStatus struct {
	Installed bool                 `json:"installed"`
	Service   *service.Status      `json:"service,omitempty"`
	Running   string               `json:"running,omitempty"`
	Kernel    *mihomo.KernelStatus `json:"kernel"`
}

// CoreStatus 返回内核服务的状态。
func (i *Installer) CoreStatus(ctx context.Context) (*CoreStatus, error) {
	sd := i.systemd()
	s := &CoreStatus{Installed: sd.Installed()}
	if s.Installed {
		st, err := sd.Status(ctx, service.CoreUnit)
		if err != nil {
			return nil, err
		}
		s.Service = &st
	}
	if v, err := mihomo.NewClient(i.Layout.CoreSocket()).Version(ctx); err == nil {
		s.Running = v
	}
	var err error
	if s.Kernel, err = i.kernels(nil, mihomo.Arch{}).Status(); err != nil {
		return nil, err
	}
	return s, nil
}

// ConfigStatus 是生效配置的概况（tunlr config），字段与 Status 中的同名字段相同。
type ConfigStatus struct {
	Revision *config.Revision `json:"revision,omitempty"`
	Drift    *pipeline.Drift  `json:"drift,omitempty"`
}

// ConfigStatus 返回生效配置对应的修订和漂移。
func (i *Installer) ConfigStatus(ctx context.Context) (*ConfigStatus, error) {
	s := &ConfigStatus{}
	revs, current, err := i.pipeline(nil).Revisions()
	if err != nil {
		return nil, err
	}
	for _, r := range revs {
		if r.Number == current {
			s.Revision = &r
			break
		}
	}
	if s.Drift, err = i.drift(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Status 返回当前状态。
func (i *Installer) Status(ctx context.Context) (*Status, error) {
	cs, err := i.CoreStatus(ctx)
	if err != nil {
		return nil, err
	}
	s := &Status{Installed: cs.Installed, Service: cs.Service, Running: cs.Running, Kernel: cs.Kernel}
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if st != nil {
		s.PanelLAN, s.ProxyLAN, s.IPv6Bypass = st.PanelLAN != nil, st.ProxyLAN != nil, st.IPv6Bypass
		if s.PanelLAN || s.ProxyLAN {
			// 不列网卡：status 常被菜单刷新，选过的入口按保存的顺序就够了。
			access := lanAccess(st, nil)
			s.LANAccess = &access
		}
		s.Filter = filterStatus(st)
		s.UpdateCheck = updateCheck(st.UpdateCheck, i.Version, cs)
	}
	list, err := subscription.LoadList(i.Layout.SubscriptionList())
	if err != nil {
		return nil, err
	}
	s.Subscription = list.Active
	// mixin 写错时不在这里报错：漂移（合成配置）会报出来。
	if from, file, err := i.SubscriptionDNS(list.Active); list.Active != "" && err == nil && from == DNSFromSubscription {
		s.SubDNS = file
	}
	if s.Updates, err = i.Schedules(); err != nil {
		return nil, err
	}
	if s.Tun, err = i.TunStatus(ctx); err != nil {
		return nil, err
	}
	cfg, err := i.ConfigStatus(ctx)
	if err != nil {
		return nil, err
	}
	s.Revision, s.Drift = cfg.Revision, cfg.Drift
	s.Devices = i.deviceSummary(ctx, st)
	s.DNSLeaks = i.DNSLeaks(ctx, st)
	return s, nil
}
