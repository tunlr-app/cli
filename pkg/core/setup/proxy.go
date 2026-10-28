package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// ProxyLANPlan 是开启代理端口局域网访问前的检查结果。
type ProxyLANPlan struct {
	// On 报告开关已经开启。
	On bool `json:"on"`
	// Port 是生效配置中的 mixed-port（局域网设备填写的代理端口），没有时为 0。
	Port int `json:"port,omitempty"`
	// Interface 是 IPv4 默认路由所在的网卡，Addresses 是它的 IPv4 地址（局域网设备填写的代理地址），
	// 取不到时为空。
	Interface string   `json:"interface,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	// PublicAddresses 是其中不属于私有地址段的地址：服务器很可能直接在公网上，开启前要再确认一次。
	PublicAddresses []string `json:"public_addresses,omitempty"`
}

// ProxyLANPreflight 检查能否开启代理端口的局域网访问（服务已安装），并给出局域网设备要填的地址和端口。
// 不满足时返回满足 errors.Is(err, core.ErrCheckFailed) 的错误。
func (i *Installer) ProxyLANPreflight(ctx context.Context) (*ProxyLANPlan, error) {
	if !i.systemd().Installed() {
		return nil, fmt.Errorf("%w: %w", core.ErrCheckFailed, mihomo.ErrServiceNotInstalled)
	}
	p := &ProxyLANPlan{}
	if st, err := state.Load(i.Layout.StateFile()); err == nil {
		p.On = st.ProxyLAN != nil
	}
	if cfg, err := i.pipeline(nil).Current(); err == nil && cfg != nil {
		if l, err := config.Listen(cfg); err == nil {
			p.Port = l.MixedPort
		}
	}
	if iface, err := i.rules().DefaultInterface(ctx); err == nil {
		p.Interface = iface
		if ifi, err := i.lookupInterface(iface); err == nil {
			p.Addresses = ifi.IPv4
		}
	}
	for _, a := range p.Addresses {
		if !privateAddress(a) {
			p.PublicAddresses = append(p.PublicAddresses, a)
		}
	}
	return p, nil
}

// EnvVar 是一个环境变量。
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ProxyEnvNames 是 ProxyEnv 设置的变量名，按输出的顺序；--unset 时去掉的也是它们。
var ProxyEnvNames = []string{"http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "no_proxy", "NO_PROXY"}

// noProxy 是不经代理的地址：本机与私有地址段。不认 CIDR 的工具（wget、Python）忽略其中的网段。
const noProxy = "localhost,127.0.0.1,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"

// ErrConfigUnreadable 表示当前用户读不到生效的配置（它含有节点的密码和 API secret，只有 root 和内核用户
// 能读）。
var ErrConfigUnreadable = errors.New("cannot read the configuration as this user")

// ProxyEnv 返回让命令行程序使用本机代理端口（生效配置的 mixed-port）的环境变量：http_proxy、https_proxy
// 为 HTTP 代理，all_proxy 为 SOCKS5（socks5h，域名交给代理解析），大小写各一份（有的工具只认其中一种），
// no_proxy 为本机与私有地址段。没有发布过配置、没有 mixed-port 或代理端口需要认证时返回满足
// errors.Is(err, core.ErrCheckFailed) 的错误；读不到配置时返回 ErrConfigUnreadable。
func (i *Installer) ProxyEnv() ([]EnvVar, error) {
	cfg, err := i.pipeline(nil).Current()
	if errors.Is(err, fs.ErrPermission) {
		return nil, ErrConfigUnreadable
	}
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, fmt.Errorf("%w: no configuration has been published yet (tunlr apply)", core.ErrCheckFailed)
	}
	l, err := config.Listen(cfg)
	if err != nil {
		return nil, err
	}
	switch {
	case l.MixedPort == 0:
		return nil, fmt.Errorf("%w: the configuration has no mixed-port", core.ErrCheckFailed)
	case l.Auth:
		return nil, fmt.Errorf("%w: the proxy port requires authentication; set the variables by hand", core.ErrCheckFailed)
	}
	addr := "127.0.0.1:" + strconv.Itoa(l.MixedPort)
	values := []string{"http://" + addr, "http://" + addr, "socks5h://" + addr}
	vars := make([]EnvVar, 0, len(ProxyEnvNames))
	for k, name := range ProxyEnvNames {
		v := noProxy
		if k < 6 {
			v = values[k%3]
		}
		vars = append(vars, EnvVar{Name: name, Value: v})
	}
	return vars, nil
}

// ProxyLANResult 是开关代理端口局域网访问的结果。
type ProxyLANResult struct {
	// Revision 是发布的配置修订；配置已经是目标状态时为 nil。
	Revision *config.Revision `json:"revision,omitempty"`
}

// SetProxyLAN 保存代理端口的局域网访问开关并发布 plan（pipeline.PlanProxyLAN(ctx, on) 的结果，已经用户
// 确认）。来源由内核的 lan-allowed-ips 限制，不需要 nft 规则。发布失败时开关恢复原状，配置由流水线回滚。
func (i *Installer) SetProxyLAN(ctx context.Context, on bool, plan *pipeline.Plan) (*ProxyLANResult, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return nil, err
	}
	previous := st.ProxyLAN
	switch {
	case !on:
		st.ProxyLAN = nil
	case st.ProxyLAN == nil:
		st.ProxyLAN = &state.ProxyLAN{Since: i.now().UTC()}
	}
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		return nil, err
	}
	applied, err := i.pipeline(nil).ApplyLocked(ctx, plan)
	if err != nil {
		st.ProxyLAN = previous
		return nil, errors.Join(err, state.Save(i.Layout.StateFile(), st))
	}
	res := &ProxyLANResult{}
	if applied != nil {
		res.Revision = applied.Revision
	}
	// 设备列表是附带的：记录设备的表加载失败不影响开关，_prestart 会再补。
	if err := i.syncDevices(ctx, st); err != nil {
		i.log().Warn("update the nftables table that records LAN devices", "err", err)
	}
	i.log().Debug("proxy lan", "on", on)
	return res, nil
}
