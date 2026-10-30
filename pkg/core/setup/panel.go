package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
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
	// LAN 是局域网访问开关；开关开启时 LANRules 报告 inet tunlr_access 表是否在，Access 是放行的入口。
	LAN      bool       `json:"lan"`
	LANRules bool       `json:"lan_rules"`
	Access   *LANAccess `json:"access,omitempty"`
	// Public 报告 API 是否不只监听本机（局域网访问开启，或配置在工具之外被改过）；这时 LANLinks 是其他设备
	// 直接打开的链接，每个地址一条。
	Public   bool        `json:"public"`
	LANLinks []PanelLink `json:"lan_links,omitempty"`
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

// PanelLink 是其他设备经本机的一个地址直接打开面板的链接。
type PanelLink struct {
	// Interface 是地址所在的网卡，Default、Gateway、SSH 同 LANInterface；API 只监听一个具体地址时
	// Interface 为空。
	Interface string `json:"interface,omitempty"`
	Default   bool   `json:"default,omitempty"`
	Gateway   bool   `json:"gateway,omitempty"`
	SSH       bool   `json:"ssh,omitempty"`
	Address   string `json:"address"`
	// URL 带着 secret，打开即连上后端；Bookmark 是不带参数的地址，浏览器记住后端之后直接打开它。
	URL      string `json:"url"`
	Bookmark string `json:"bookmark"`
}

// Panel 从生效的配置得出打开面板的方式。没有发布过配置、或配置中没有 external-controller 时返回满足
// errors.Is(err, core.ErrCheckFailed) 的错误。
func (i *Installer) Panel(ctx context.Context, opts PanelOptions) (*Panel, error) {
	cfg, err := i.pipeline(nil).Current()
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, fmt.Errorf("%w: no configuration has been published yet; add a subscription (tunlr sub add), then tunlr config apply", core.ErrCheckFailed)
	}
	api, err := config.API(cfg)
	if err != nil {
		return nil, err
	}
	if api.Controller == "" {
		return nil, fmt.Errorf("%w: the configuration has no external-controller, so the panel is off (edited outside tunlr?); tunlr config apply publishes it again", core.ErrCheckFailed)
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
	st, err := state.Load(i.Layout.StateFile())
	if err != nil {
		st = nil
	}
	cands := i.lanAddresses(ctx, st, opts.SSHConnection)
	if st != nil && st.PanelLAN != nil {
		p.LAN = true
		if tables, err := i.rules().Tables(ctx); err == nil {
			p.LANRules = tables[netfilter.AccessTable]
		}
		access := lanAccess(st, cands)
		p.Access = &access
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
		links := []PanelLink{{Address: host}}
		if unspecified {
			links = nil
			for _, c := range cands {
				// 开关开着时只给放行的网卡上的地址；配置在工具之外被改过时 API 对所有网卡开放，都列出。
				if p.LAN && !c.Allowed {
					continue
				}
				for _, a := range c.Addresses {
					links = append(links, PanelLink{Interface: c.Name, Default: c.Default, Gateway: c.Gateway, SSH: c.SSH, Address: a})
				}
			}
			if len(links) == 0 {
				links = []PanelLink{{Address: "<server>"}}
			}
		}
		for k := range links {
			links[k].URL = panelURL(links[k].Address, port, api.Secret, p.Label)
			links[k].Bookmark = panelHome(links[k].Address, port)
		}
		p.LANLinks = links
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

// PanelLANPlan 是开启面板局域网访问前的检查结果。
type PanelLANPlan struct {
	// On 报告开关已经开启。
	On bool `json:"on"`
	// Interfaces 是本机可用的网卡和它们的 IPv4 地址（见 LANAddresses），Allowed 的放行；放行的网卡有公网
	// 地址（PublicAddresses）时服务器很可能直接在公网上，开启前要再确认一次。
	Interfaces []LANInterface `json:"interfaces,omitempty"`
	// Access 是放行的入口，与代理端口共用。
	Access LANAccess `json:"access"`
}

// PanelLANPreflight 检查能否开启面板的局域网访问：服务已安装（_prestart 负责在重启后重新加载规则）、
// nft 可用；并给出放行的入口。sshConnection 是 $SSH_CONNECTION，可为空。不满足时返回满足
// errors.Is(err, core.ErrCheckFailed) 的错误。
func (i *Installer) PanelLANPreflight(ctx context.Context, sshConnection string) (*PanelLANPlan, error) {
	if err := i.lanAccessPreflight(ctx); err != nil {
		return nil, err
	}
	p := &PanelLANPlan{}
	st, err := state.Load(i.Layout.StateFile())
	if err != nil {
		st = nil
	}
	p.On = st != nil && st.PanelLAN != nil
	p.Interfaces = i.lanAddresses(ctx, st, sshConnection)
	p.Access = lanAccess(st, p.Interfaces)
	return p, nil
}

// lanAccessPreflight 检查能否对其他设备开放代理端口或面板：服务已安装（_prestart 负责在重启后重新加载
// 规则）、nft 可用（访问限制靠它）。
func (i *Installer) lanAccessPreflight(ctx context.Context) error {
	if !i.systemd().Installed() {
		return fmt.Errorf("%w: %w", core.ErrCheckFailed, mihomo.ErrServiceNotInstalled)
	}
	if _, err := i.rules().Tables(ctx); err != nil {
		return fmt.Errorf("%w: nftables is not usable: %w", core.ErrCheckFailed, err)
	}
	return nil
}

// privateAddress 报告地址 s 是否在私有地址段中，见 netfilter.PrivateIPv4。
func privateAddress(s string) bool { return netfilter.PrivateAddress(s) }

// PanelLANResult 是开关面板局域网访问的结果。
type PanelLANResult struct {
	// Revision 是发布的配置修订；配置已经是目标状态时为 nil。
	Revision *config.Revision `json:"revision,omitempty"`
}

// PanelLANOn 开启面板的局域网访问并发布 plan（pipeline.PlanPanelLAN(ctx, true) 的结果，已经用户确认）：
// 加锁 → 开关记为开启、保存放行的入口 access（ResolveLANAccess 的结果，为 nil 时不变）→ 加载
// inet tunlr_access（先于 API 对外监听）→ 发布。配置已经是开启的样子时（plan 没有变化）只补上表。
// 发布失败时开关和入口恢复原状、表随之恢复，配置由流水线回滚。
func (i *Installer) PanelLANOn(ctx context.Context, plan *pipeline.Plan, access *state.LANAccess) (*PanelLANResult, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return nil, err
	}
	previous, previousAccess := st.PanelLAN, st.LANAccess
	if st.PanelLAN == nil {
		st.PanelLAN = &state.PanelLAN{Since: i.now().UTC()}
	}
	if access != nil {
		st.LANAccess = access
	}
	err = state.Save(i.Layout.StateFile(), st)
	if err == nil {
		err = i.syncAccess(ctx, st, nil)
	}
	var applied *pipeline.ApplyResult
	if err == nil && plan.Changed() {
		applied, err = i.pipeline(nil).ApplyLocked(ctx, plan)
	}
	if err != nil {
		st.PanelLAN, st.LANAccess = previous, previousAccess
		return nil, errors.Join(err, state.Save(i.Layout.StateFile(), st), i.syncAccess(ctx, st, nil))
	}
	i.syncCheckTimer(ctx, st)
	res := &PanelLANResult{}
	if applied != nil {
		res.Revision = applied.Revision
	}
	i.log().Debug("panel lan on")
	return res, nil
}

// PanelLANOff 关闭面板的局域网访问并发布 plan（pipeline.PlanPanelLAN(ctx, false) 的结果，已经用户确认），
// 发布之后再从 inet tunlr_access 中去掉 API 端口。发布失败时开关恢复原状，配置由流水线回滚。
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
	i.syncCheckTimer(ctx, st)
	if err := i.syncAccess(ctx, st, nil); err != nil {
		return res, fmt.Errorf("the panel only listens on this server now, but updating the nftables table inet %s failed: %w", netfilter.AccessTable, err)
	}
	i.log().Debug("panel lan off")
	return res, nil
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
