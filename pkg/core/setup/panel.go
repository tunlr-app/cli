package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// PanelOptions 是生成面板访问方式的参数，取自运行 tunlr 的环境。
type PanelOptions struct {
	// SSHConnection 是 $SSH_CONNECTION（“客户端 IP 客户端端口 本机 IP 本机端口”），为空时不在 SSH 会话中。
	SSHConnection string
	// User 是登录服务器的用户（$SUDO_USER，没有则 $USER）。
	User string
	// Label 是 zashboard 后端列表中的名称，为空时取主机名。
	Label string
}

// Panel 是打开面板（zashboard）的方式。
type Panel struct {
	// Controller 是生效配置中的 external-controller。
	Controller string `json:"controller"`
	// Port 是 API 的端口，隧道两端都用它。
	Port int `json:"port"`
	// LAN 是局域网访问开关；LANRules 报告开关开启时 inet tunlr_panel 表是否在。
	LAN      bool `json:"lan"`
	LANRules bool `json:"lan_rules"`
	// Public 报告 API 是否不只监听本机（局域网访问开启，或 mixin 改过）；这时 LANURLs 是直接打开的链接。
	Public  bool     `json:"public"`
	LANURLs []string `json:"lan_urls,omitempty"`
	// LANBookmarks 是与 LANURLs 对应、不带参数的面板地址：浏览器记住了后端之后直接打开它。
	LANBookmarks []string `json:"lan_bookmarks,omitempty"`
	// Label 是链接中给 zashboard 的后端名称（经隧道的后端另加 “(SSH tunnel)”）。zashboard 只在
	// 各项完全相同时复用已有的后端，所以名称保持固定，重复打开链接不会多出一个后端。
	Label string `json:"label"`
	// SSHCommand 是在用户电脑上建立隧道的命令；SSHHostGuessed 报告其中的服务器地址取自
	// $SSH_CONNECTION（经别名、跳板机或 NAT 连接时不对），为 false 时地址是占位符 <server>。
	SSHCommand     string `json:"ssh_command"`
	SSHHostGuessed bool   `json:"ssh_host_guessed"`
	// LocalForward 是写进 ~/.ssh/config 的等价设置。
	LocalForward string `json:"local_forward"`
	// URL 是经隧道在浏览器中打开的链接，带着 secret，打开即连上后端；Bookmark 是不带参数的地址。
	URL      string `json:"url"`
	Bookmark string `json:"bookmark"`
	// Secret 是 API secret，为空时 API 不需要认证。
	Secret string `json:"secret"`
	// UIDir 是面板的目录，UIInstalled 报告它是否存在。
	UIDir       string `json:"ui_dir,omitempty"`
	UIInstalled bool   `json:"ui_installed"`
	// Running 是运行中的内核版本，内核没有响应时为空。
	Running string `json:"running,omitempty"`
}

// Panel 从生效的配置得出打开面板的方式。没有发布过配置、或配置中没有 external-controller 时返回满足
// errors.Is(err, core.ErrCheckFailed) 的错误。
func (i *Installer) Panel(ctx context.Context, opts PanelOptions) (*Panel, error) {
	cfg, err := i.pipeline(nil).Current()
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, fmt.Errorf("%w: no configuration has been published yet; add a subscription (tunlr sub add), then tunlr apply", core.ErrCheckFailed)
	}
	api, err := config.API(cfg)
	if err != nil {
		return nil, err
	}
	if api.Controller == "" {
		return nil, fmt.Errorf("%w: the configuration has no external-controller, so the panel is off; a mixin removed it", core.ErrCheckFailed)
	}
	host, portText, err := net.SplitHostPort(api.Controller)
	port, perr := strconv.Atoi(portText)
	if err != nil || perr != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("%w: external-controller %q is not host:port", core.ErrCheckFailed, api.Controller)
	}
	p := &Panel{Controller: api.Controller, Port: port, Secret: api.Secret, Label: opts.Label}
	if p.Label == "" {
		p.Label, _ = os.Hostname()
	}
	if st, err := state.Load(i.Layout.StateFile()); err == nil && st.PanelLAN != nil {
		p.LAN = true
		if tables, err := i.rules().Tables(ctx); err == nil {
			p.LANRules = tables[netfilter.PanelTable]
		}
	}

	// 隧道的目标：监听所有地址或 localhost 时经 127.0.0.1，否则就是监听的地址。
	serverIP, sshPort := sshServer(opts.SSHConnection)
	unspecified := host == ""
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		unspecified = true
	}
	target := host
	if unspecified || host == "localhost" {
		target = "127.0.0.1"
	}
	if strings.Contains(target, ":") {
		target = "[" + target + "]"
	}
	forward := fmt.Sprintf("%d:%s:%d", port, target, port)
	server := "<server>"
	if serverIP != "" {
		server, p.SSHHostGuessed = serverIP, true
		if opts.User != "" {
			server = opts.User + "@" + serverIP
		}
	}
	cmd := "ssh -N -L " + forward
	if sshPort != "" && sshPort != "22" {
		cmd += " -p " + sshPort
	}
	p.SSHCommand = cmd + " " + server
	p.LocalForward = fmt.Sprintf("LocalForward %d %s:%d", port, target, port)
	p.URL = panelURL("127.0.0.1", port, api.Secret, strings.TrimSpace(p.Label+" (SSH tunnel)"))
	p.Bookmark = panelHome("127.0.0.1", port)

	if host != "localhost" && !isLoopbackIP(host) {
		p.Public = true
		hosts := []string{host}
		if unspecified {
			hosts = i.lanAddresses(ctx)
			if serverIP != "" && !slices.Contains(hosts, serverIP) && net.ParseIP(serverIP).To4() != nil {
				hosts = append(hosts, serverIP)
			}
			if len(hosts) == 0 {
				hosts = []string{"<server>"}
			}
		}
		for _, h := range hosts {
			p.LANURLs = append(p.LANURLs, panelURL(h, port, api.Secret, p.Label))
			p.LANBookmarks = append(p.LANBookmarks, panelHome(h, port))
		}
	}

	if facts, err := config.Inspect(cfg); err == nil {
		if dir, _ := facts.ExternalUI(i.Layout.CoreDir()); dir != "" {
			p.UIDir = dir
			_, err := os.Stat(dir)
			p.UIInstalled = err == nil
		}
	}
	if v, err := mihomo.NewClient(i.Layout.CoreSocket()).Version(ctx); err == nil {
		p.Running = v
	}
	return p, nil
}

// lanAddresses 返回 IPv4 默认路由所在网卡的地址，取不到时为空。
func (i *Installer) lanAddresses(ctx context.Context) []string {
	iface, err := i.rules().DefaultInterface(ctx)
	if err != nil {
		return nil
	}
	ifi, err := i.lookupInterface(iface)
	if err != nil {
		return nil
	}
	return ifi.IPv4
}

// PanelLANPlan 是开启面板局域网访问前的检查结果。
type PanelLANPlan struct {
	// On 报告开关已经开启。
	On bool `json:"on"`
	// Interface 是 IPv4 默认路由所在的网卡，Addresses 是它的 IPv4 地址（局域网设备用来打开面板），取不到时为空。
	Interface string   `json:"interface,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	// PublicAddresses 是其中不属于私有地址段的地址：服务器很可能直接在公网上，开启前要再确认一次。
	PublicAddresses []string `json:"public_addresses,omitempty"`
}

// PanelLANPreflight 检查能否开启面板的局域网访问：服务已安装（_prestart 负责在重启后重新加载规则）、
// nft 可用。不满足时返回满足 errors.Is(err, core.ErrCheckFailed) 的错误。
func (i *Installer) PanelLANPreflight(ctx context.Context) (*PanelLANPlan, error) {
	if !i.systemd().Installed() {
		return nil, fmt.Errorf("%w: %w", core.ErrCheckFailed, mihomo.ErrServiceNotInstalled)
	}
	if _, err := i.rules().Tables(ctx); err != nil {
		return nil, fmt.Errorf("%w: nftables is not usable: %w", core.ErrCheckFailed, err)
	}
	p := &PanelLANPlan{}
	if st, err := state.Load(i.Layout.StateFile()); err == nil {
		p.On = st.PanelLAN != nil
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

// privateNets 是面板对局域网开放时允许的来源，与 templates/nft/panel.nft 中的 IPv4 地址段一致。
var privateNets = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
}

func privateAddress(s string) bool {
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(privateNets, func(p netip.Prefix) bool { return p.Contains(ip.Unmap()) })
}

// PanelLANResult 是开关面板局域网访问的结果。
type PanelLANResult struct {
	// Revision 是发布的配置修订；配置已经是目标状态时为 nil。
	Revision *config.Revision `json:"revision,omitempty"`
}

// PanelLANOn 开启面板的局域网访问并发布 plan（pipeline.PlanPanelLAN(ctx, true) 的结果，已经用户确认）：
// 加锁 → 加载 nft 表（先于 API 对外监听）→ 开关记为开启 → 发布。发布失败时开关恢复原状、撤掉新加载的表，
// 配置由流水线回滚。
func (i *Installer) PanelLANOn(ctx context.Context, plan *pipeline.Plan) (*PanelLANResult, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return nil, err
	}
	previous := st.PanelLAN
	if err := i.rules().PanelUp(ctx, pipeline.APIPort); err != nil {
		return nil, err
	}
	if st.PanelLAN == nil {
		st.PanelLAN = &state.PanelLAN{Since: i.now().UTC()}
	}
	err = state.Save(i.Layout.StateFile(), st)
	var applied *pipeline.ApplyResult
	if err == nil {
		applied, err = i.pipeline(nil).ApplyLocked(ctx, plan)
	}
	if err != nil {
		st.PanelLAN = previous
		uerr := state.Save(i.Layout.StateFile(), st)
		if previous == nil {
			uerr = errors.Join(uerr, i.rules().PanelDown(ctx))
		}
		return nil, errors.Join(err, uerr)
	}
	res := &PanelLANResult{}
	if applied != nil {
		res.Revision = applied.Revision
	}
	i.log().Debug("panel lan on")
	return res, nil
}

// PanelLANOff 关闭面板的局域网访问并发布 plan（pipeline.PlanPanelLAN(ctx, false) 的结果，已经用户确认），
// 发布之后再撤掉 nft 表。发布失败时开关恢复原状，配置由流水线回滚。
func (i *Installer) PanelLANOff(ctx context.Context, plan *pipeline.Plan) (*PanelLANResult, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return nil, err
	}
	previous := st.PanelLAN
	st.PanelLAN = nil
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		return nil, err
	}
	applied, err := i.pipeline(nil).ApplyLocked(ctx, plan)
	if err != nil {
		st.PanelLAN = previous
		return nil, errors.Join(err, state.Save(i.Layout.StateFile(), st))
	}
	res := &PanelLANResult{}
	if applied != nil {
		res.Revision = applied.Revision
	}
	if err := i.rules().PanelDown(ctx); err != nil {
		return res, fmt.Errorf("the panel only listens on this server now, but removing the nftables table inet %s failed: %w", netfilter.PanelTable, err)
	}
	i.log().Debug("panel lan off")
	return res, nil
}

// repairPanel 在面板对局域网开放时补上缺少的 nft 表（_prestart、_check），返回采取的动作。
func (i *Installer) repairPanel(ctx context.Context, st *state.State) (string, error) {
	if st == nil || st.PanelLAN == nil {
		return "", nil
	}
	tables, err := i.rules().Tables(ctx)
	if err != nil || tables[netfilter.PanelTable] {
		return "", err
	}
	if err := i.rules().PanelUp(ctx, pipeline.APIPort); err != nil {
		return "", err
	}
	return "reloaded the nftables table inet " + netfilter.PanelTable, nil
}

// panelURL 是 zashboard 的设置链接：打开后按 hostname、port、secret 添加名为 label 的后端并连接
// （已有完全相同的后端时直接选中它）。
func panelURL(host string, port int, secret, label string) string {
	q := url.Values{}
	q.Set("hostname", host)
	q.Set("port", strconv.Itoa(port))
	if secret != "" {
		q.Set("secret", secret)
	}
	if label != "" {
		q.Set("label", label)
	}
	// url.Values 按键名排序编码，zashboard 不在乎顺序；空格写成 %20（原有的 + 已编码为 %2B），
	// 不依赖解析方把 + 当作空格。
	return panelHome(host, port) + "#/setup?" + strings.ReplaceAll(q.Encode(), "+", "%20")
}

// panelHome 是面板的地址（模板中 external-ui 挂在 /ui）。
func panelHome(host string, port int) string {
	hostport := net.JoinHostPort(host, strconv.Itoa(port))
	if host == "<server>" {
		hostport = host + ":" + strconv.Itoa(port)
	}
	return "http://" + hostport + "/ui/"
}

// sshServer 从 $SSH_CONNECTION 中取出本机一端的地址和端口。
func sshServer(conn string) (ip, port string) {
	f := strings.Fields(conn)
	if len(f) != 4 || net.ParseIP(f[2]) == nil {
		return "", ""
	}
	return f[2], f[3]
}

func isLoopbackIP(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
