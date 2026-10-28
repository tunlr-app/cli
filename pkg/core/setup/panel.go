package setup

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
)

// PanelOptions 是生成面板访问方式的参数，取自运行 tunlr 的环境。
type PanelOptions struct {
	// SSHConnection 是 $SSH_CONNECTION（“客户端 IP 客户端端口 本机 IP 本机端口”），为空时不在 SSH 会话中。
	SSHConnection string
	// User 是登录服务器的用户（$SUDO_USER，没有则 $USER）。
	User string
}

// Panel 是打开面板（zashboard）的方式。
type Panel struct {
	// Controller 是生效配置中的 external-controller。
	Controller string `json:"controller"`
	// Port 是 API 的端口，隧道两端都用它。
	Port int `json:"port"`
	// Public 报告 API 是否对外监听（mixin 改过）；这时不需要隧道，SSHCommand 为空。
	Public bool `json:"public"`
	// SSHCommand 是在用户电脑上建立隧道的命令；SSHHostGuessed 报告其中的服务器地址取自
	// $SSH_CONNECTION（经别名、跳板机或 NAT 连接时不对），为 false 时地址是占位符 <server>。
	SSHCommand     string `json:"ssh_command,omitempty"`
	SSHHostGuessed bool   `json:"ssh_host_guessed"`
	// LocalForward 是写进 ~/.ssh/config 的等价设置。
	LocalForward string `json:"local_forward,omitempty"`
	// URL 是浏览器中打开的链接，带着 secret，打开即连上后端。
	URL string `json:"url"`
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
	p := &Panel{Controller: api.Controller, Port: port, Secret: api.Secret}

	serverIP, sshPort := sshServer(opts.SSHConnection)
	if host == "localhost" || isLoopbackIP(host) {
		target := host
		if target == "localhost" {
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
		p.URL = panelURL("127.0.0.1", port, api.Secret)
	} else {
		p.Public = true
		// 监听所有地址时，用 SSH 连进来的那个本机地址。
		if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
			host = serverIP
		}
		if host == "" {
			host = "<server>"
		}
		p.URL = panelURL(host, port, api.Secret)
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

// panelURL 是 zashboard 的设置链接：打开后按 hostname、port、secret 添加后端并连接。
func panelURL(host string, port int, secret string) string {
	q := url.Values{}
	q.Set("hostname", host)
	q.Set("port", strconv.Itoa(port))
	if secret != "" {
		q.Set("secret", secret)
	}
	hostport := net.JoinHostPort(host, strconv.Itoa(port))
	if host == "<server>" {
		hostport = host + ":" + strconv.Itoa(port)
	}
	// url.Values 按键名排序编码，zashboard 不在乎顺序；空格写成 %20（原有的 + 已编码为 %2B），
	// 不依赖解析方把 + 当作空格。
	return "http://" + hostport + "/ui/#/setup?" + strings.ReplaceAll(q.Encode(), "+", "%20")
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
