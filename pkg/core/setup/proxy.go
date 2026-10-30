package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// ProxyLANPlan 是开启代理端口局域网访问前的检查结果。
type ProxyLANPlan struct {
	// On 报告开关已经开启。
	On bool `json:"on"`
	// Port 是生效配置中的 mixed-port（局域网设备填写的代理端口），没有时为 0。
	Port int `json:"port,omitempty"`
	// Interfaces 是本机可用的网卡和它们的 IPv4 地址（见 LANAddresses），Allowed 的放行；放行的网卡有公网
	// 地址（PublicAddresses）时服务器很可能直接在公网上，开启前要再确认一次。
	Interfaces []LANInterface `json:"interfaces,omitempty"`
	// Access 是放行的入口，与面板共用。
	Access LANAccess `json:"access"`
}

// ProxyLANPreflight 检查能否开启代理端口的局域网访问（服务已安装），并给出放行的入口、其他设备要填的
// 地址和端口。sshConnection 是 $SSH_CONNECTION，可为空。不满足时返回满足 errors.Is(err, core.ErrCheckFailed)
// 的错误。
func (i *Installer) ProxyLANPreflight(ctx context.Context, sshConnection string) (*ProxyLANPlan, error) {
	if err := i.lanAccessPreflight(ctx); err != nil {
		return nil, err
	}
	return i.ProxyLANInfo(ctx, sshConnection), nil
}

// ProxyLANInfo 同 ProxyLANPreflight，但不检查能否开启：tunlr proxy 只显示现状，没有安装时也能显示。
func (i *Installer) ProxyLANInfo(ctx context.Context, sshConnection string) *ProxyLANPlan {
	p := &ProxyLANPlan{}
	st, err := state.Load(i.Layout.StateFile())
	if err != nil {
		st = nil
	}
	p.On = st != nil && st.ProxyLAN != nil
	if cfg, err := i.pipeline(nil).Current(); err == nil && cfg != nil {
		if l, err := config.Listen(cfg); err == nil {
			p.Port = l.MixedPort
		}
	}
	p.Interfaces = i.lanAddresses(ctx, st, sshConnection)
	p.Access = lanAccess(st, p.Interfaces)
	return p
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
		return nil, fmt.Errorf("%w: no configuration has been published yet (tunlr config apply)", core.ErrCheckFailed)
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
// 确认）。开启时 access 是放行的入口（ResolveLANAccess 的结果，为 nil 时不变），inet tunlr_access 先于发布
// 加载，端口对外的任何时刻都只放行这些入口；关闭时发布之后再从表中去掉端口。内核的 lan-allowed-ips 另外
// 只放行私有地址。发布失败时开关和入口恢复原状，配置由流水线回滚。
func (i *Installer) SetProxyLAN(ctx context.Context, on bool, plan *pipeline.Plan, access *state.LANAccess) (*ProxyLANResult, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return nil, err
	}
	previous, previousAccess := st.ProxyLAN, st.LANAccess
	switch {
	case !on:
		st.ProxyLAN = nil
	case st.ProxyLAN == nil:
		st.ProxyLAN = &state.ProxyLAN{Since: i.now().UTC()}
	}
	if on && access != nil {
		st.LANAccess = access
	}
	err = state.Save(i.Layout.StateFile(), st)
	if err == nil && on {
		err = i.syncAccess(ctx, st, plan.Config)
	}
	var applied *pipeline.ApplyResult
	if err == nil {
		applied, err = i.pipeline(nil).ApplyLocked(ctx, plan)
	}
	if err != nil {
		st.ProxyLAN, st.LANAccess = previous, previousAccess
		return nil, errors.Join(err, state.Save(i.Layout.StateFile(), st), i.syncAccess(ctx, st, nil))
	}
	res := &ProxyLANResult{}
	if applied != nil {
		res.Revision = applied.Revision
	}
	if !on {
		// 端口已经只监听本机，表中多留一个端口不会多放行什么；_check 会再改正。
		if err := i.syncAccess(ctx, st, nil); err != nil {
			i.log().Warn("update the nftables table for LAN access", "err", err)
		}
	}
	i.syncCheckTimer(ctx, st)
	// 设备列表是附带的：记录设备的表加载失败不影响开关，_prestart 会再补。
	if err := i.syncDevices(ctx, st); err != nil {
		i.log().Warn("update the nftables table that records LAN devices", "err", err)
	}
	i.log().Debug("proxy lan", "on", on)
	return res, nil
}
