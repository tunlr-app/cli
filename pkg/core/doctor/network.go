package doctor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"time"
)

// 网络检查的参数。
const (
	// probeTimeout 是每项网络检查的时限；各项并行，总时长约为它。
	probeTimeout = 8 * time.Second
	// skewWarn 以上的时钟偏差会让 VMess 等协议失败，skewFail 以上 TLS 也会失败。
	skewWarn = 90 * time.Second
	skewFail = 24 * time.Hour
	// ProbeURL 是经代理访问的测试地址，返回 204。
	ProbeURL = "https://www.gstatic.com/generate_204"
)

// NetworkOptions 是网络检查的参数。
type NetworkOptions struct {
	// APIBase 是 GitHub API 的地址（下载时取校验值的来源），为空时为 https://api.github.com。
	APIBase string
	// Mirror 是 GitHub 下载镜像的前缀，为空时不检查。
	Mirror string
	// Proxy 是内核的 mixed-port 地址（如 127.0.0.1:7890），为空时不检查经代理的访问；
	// ProxySkip 是不检查的原因（如内核没有运行）。
	Proxy, ProxySkip string
	// ProbeURL 为空时为 ProbeURL，测试时替换。
	ProbeURL string
	// HTTP 为空时使用默认的传输（遵循 HTTPS_PROXY 等环境变量），测试时替换。
	HTTP *http.Client
	// LookupHost 为空时使用系统的解析器。
	LookupHost func(ctx context.Context, host string) ([]string, error)
	// Now 为空时使用 time.Now。
	Now func() time.Time
}

// Network 检查网络（第四组）：域名解析、GitHub API、镜像、时钟偏差、经内核代理的访问。各项并行进行。
func Network(ctx context.Context, opts NetworkOptions) []Check {
	c := &Checker{Group: GroupNetwork}
	api := cmp.Or(opts.APIBase, "https://api.github.com")
	apiURL, err := url.Parse(api)
	if err != nil {
		c.Add("github", Fail, "GitHub API", err.Error(), "")
		return c.List
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	client := opts.HTTP
	if client == nil {
		client = &http.Client{}
	}

	var (
		wg                        sync.WaitGroup
		dnsErr, apiErr, mirrorErr error
		proxyErr                  error
		apiDate                   time.Time
		apiStatus, proxyStatus    int
		dnsAddrs                  []string
		mirrorStatus              int
	)
	wg.Go(func() {
		lookup := opts.LookupHost
		if lookup == nil {
			lookup = net.DefaultResolver.LookupHost
		}
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		dnsAddrs, dnsErr = lookup(ctx, apiURL.Hostname())
	})
	wg.Go(func() {
		var resp *http.Response
		resp, apiErr = get(ctx, client, api+"/zen")
		if resp != nil {
			apiStatus = resp.StatusCode
			apiDate, _ = http.ParseTime(resp.Header.Get("Date"))
		}
	})
	if opts.Mirror != "" {
		wg.Go(func() {
			var resp *http.Response
			resp, mirrorErr = get(ctx, client, opts.Mirror)
			if resp != nil {
				mirrorStatus = resp.StatusCode
			}
		})
	}
	if opts.Proxy != "" {
		wg.Go(func() {
			pc := *client
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = http.ProxyURL(&url.URL{Scheme: "http", Host: opts.Proxy})
			pc.Transport = tr
			var resp *http.Response
			resp, proxyErr = get(ctx, &pc, cmp.Or(opts.ProbeURL, ProbeURL))
			if resp != nil {
				proxyStatus = resp.StatusCode
			}
		})
	}
	wg.Wait()

	host := apiURL.Hostname()
	if dnsErr != nil {
		c.Add("dns", Fail, "DNS", fmt.Sprintf("cannot resolve %s: %v", host, dnsErr), "check /etc/resolv.conf and the DNS server")
	} else {
		c.Add("dns", OK, "DNS", fmt.Sprintf("%s resolves to %s", host, dnsAddrs[0]), "")
	}

	switch {
	case apiErr != nil:
		c.Add("github", Fail, "GitHub API", fmt.Sprintf("cannot reach %s: %v", api, apiErr),
			"tunlr reads the official checksums from the GitHub API; downloads fail without it")
	case apiStatus >= 500:
		c.Add("github", Warn, "GitHub API", fmt.Sprintf("%s answered HTTP %d", api, apiStatus), "")
	default:
		c.Add("github", OK, "GitHub API", "reachable", "")
	}

	if opts.Mirror == "" {
		c.Add("mirror", Skip, "GitHub mirror", "no mirror configured", "")
	} else if mirrorErr != nil {
		c.Add("mirror", Fail, "GitHub mirror", fmt.Sprintf("cannot reach %s: %v", opts.Mirror, mirrorErr),
			"change it with tunlr init --mirror <url>, or --mirror '' to download directly")
	} else {
		c.Add("mirror", OK, "GitHub mirror", fmt.Sprintf("%s answered HTTP %d", opts.Mirror, mirrorStatus), "")
	}

	switch skew := now().Sub(apiDate).Abs(); {
	case apiDate.IsZero():
		c.Add("clock", Skip, "Clock", "GitHub did not report the time", "")
	case skew > skewFail:
		c.Add("clock", Fail, "Clock", fmt.Sprintf("the system clock is off by %s; TLS fails", skew.Round(time.Second)),
			"enable time sync: timedatectl set-ntp true")
	case skew > skewWarn:
		c.Add("clock", Warn, "Clock", fmt.Sprintf("the system clock is off by %s; VMess and similar protocols fail", skew.Round(time.Second)),
			"enable time sync: timedatectl set-ntp true")
	default:
		c.Add("clock", OK, "Clock", fmt.Sprintf("off by %s", skew.Round(time.Second)), "")
	}

	probe := cmp.Or(opts.ProbeURL, ProbeURL)
	switch {
	case opts.Proxy == "":
		c.Add("proxy", Skip, "Through mihomo", cmp.Or(opts.ProxySkip, "mihomo is not running"), "")
	case proxyErr != nil || proxyStatus >= 400:
		msg, hint := describeProxyFailure(opts.Proxy, proxyErr, proxyStatus)
		c.Add("proxy", Warn, "Through mihomo", fmt.Sprintf("%s (%s via %s)", msg, probe, opts.Proxy), hint)
	default:
		c.Add("proxy", OK, "Through mihomo", fmt.Sprintf("%s via %s: HTTP %d", probe, opts.Proxy, proxyStatus), "")
	}
	return c.List
}

// selectedProxyHint 是经内核访问失败、而原因在节点一侧时的修复建议。
const selectedProxyHint = "test the latency in the panel and select a working proxy; an auto-select (url-test) group " +
	"keeps a dead proxy until its next test"

// describeProxyFailure 把经内核代理访问失败的原因说成人话：Go 报告的 EOF、超时、502 等，
// 实际都是内核连不上所选的节点（或节点连不上目标）。
func describeProxyFailure(proxy string, err error, status int) (msg, hint string) {
	var ne net.Error
	var op *net.OpError
	switch {
	case err == nil && (status == http.StatusBadGateway || status == http.StatusGatewayTimeout || status == http.StatusServiceUnavailable):
		return fmt.Sprintf("mihomo answered HTTP %d: the selected proxy could not reach the site", status), selectedProxyHint
	case err == nil:
		return fmt.Sprintf("the site answered HTTP %d through mihomo", status), "a rule may send it to a proxy that the site blocks; check the connection in the panel"
	case errors.As(err, &op) && op.Op == "proxyconnect":
		// 连不上 mixed-port 本身（Go 把连接代理的错误标为 proxyconnect）。
		return "cannot connect to mihomo's mixed-port " + proxy, "check tunlr status and tunlr logs; tunlr restart"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET):
		return "mihomo closed the connection: the selected proxy did not respond", selectedProxyHint
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return fmt.Sprintf("no answer within %s: the selected proxy is down or very slow", probeTimeout), selectedProxyHint
	}
	return "the request through mihomo failed: " + err.Error(), selectedProxyHint
}

// get 在 probeTimeout 内发出 GET 请求，只读响应头。
func get(ctx context.Context, client *http.Client, u string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tunlr-doctor")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	return resp, nil
}
