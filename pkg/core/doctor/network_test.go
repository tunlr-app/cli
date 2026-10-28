package doctor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var netNow = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

// githubAt 是一个假的 GitHub API，响应头中的时间为 date。
func githubAt(t *testing.T, date time.Time) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", date.Format(http.TimeFormat))
		if r.URL.Path != "/zen" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func resolveTo(addr string) func(context.Context, string) ([]string, error) {
	return func(context.Context, string) ([]string, error) { return []string{addr}, nil }
}

func networkStatuses(t *testing.T, opts NetworkOptions) map[string]Check {
	t.Helper()
	if opts.Now == nil {
		opts.Now = func() time.Time { return netNow }
	}
	if opts.LookupHost == nil {
		opts.LookupHost = resolveTo("127.0.0.1")
	}
	m := map[string]Check{}
	for _, c := range Network(context.Background(), opts) {
		if c.Group != GroupNetwork {
			t.Errorf("%s in group %s", c.ID, c.Group)
		}
		m[c.ID] = c
	}
	return m
}

func TestNetworkHealthy(t *testing.T) {
	gh := githubAt(t, netNow.Add(10*time.Second))
	mirror := httptest.NewServer(http.NotFoundHandler())
	defer mirror.Close()
	// 代理：收到形如 GET http://probe/ 的请求时返回 204。
	var proxied string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = r.URL.String()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()

	got := networkStatuses(t, NetworkOptions{
		APIBase: gh.URL, Mirror: mirror.URL + "/",
		Proxy: strings.TrimPrefix(proxy.URL, "http://"), ProbeURL: "http://probe.test/generate_204",
	})
	for _, id := range []string{"network.dns", "network.github", "network.mirror", "network.clock", "network.proxy"} {
		if got[id].Status != OK {
			t.Errorf("%s = %s: %s", id, got[id].Status, got[id].Message)
		}
	}
	if proxied != "http://probe.test/generate_204" {
		t.Errorf("proxied %q", proxied)
	}
	if got["network.clock"].Message != "off by 10s" {
		t.Errorf("clock: %q", got["network.clock"].Message)
	}
}

func TestNetworkProblems(t *testing.T) {
	gh := githubAt(t, netNow.Add(-5*time.Minute))
	got := networkStatuses(t, NetworkOptions{
		APIBase:    gh.URL,
		LookupHost: func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") },
		ProxySkip:  "mihomo is not running",
	})
	for id, want := range map[string]string{
		"network.dns": Fail, "network.github": OK, "network.mirror": Skip, "network.clock": Warn, "network.proxy": Skip,
	} {
		if got[id].Status != want {
			t.Errorf("%s = %s, want %s: %s", id, got[id].Status, want, got[id].Message)
		}
	}
	if got["network.proxy"].Message != "mihomo is not running" || got["network.clock"].Hint == "" {
		t.Errorf("proxy %+v, clock %+v", got["network.proxy"], got["network.clock"])
	}

	// 时钟差了两天：TLS 也会失败。
	gh = githubAt(t, netNow.Add(48*time.Hour))
	if c := networkStatuses(t, NetworkOptions{APIBase: gh.URL})["network.clock"]; c.Status != Fail {
		t.Errorf("clock = %+v", c)
	}
}

func TestNetworkUnreachable(t *testing.T) {
	// 已关闭的服务器：连接被拒绝，不等超时。
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer broken.Close()

	got := networkStatuses(t, NetworkOptions{
		APIBase: dead.URL, Mirror: dead.URL + "/",
		Proxy: strings.TrimPrefix(broken.URL, "http://"), ProbeURL: "http://probe.test/",
	})
	for id, want := range map[string]string{
		"network.github": Fail, "network.mirror": Fail, "network.clock": Skip, "network.proxy": Warn,
	} {
		if got[id].Status != want {
			t.Errorf("%s = %s, want %s: %s", id, got[id].Status, want, got[id].Message)
		}
	}
	if c := got["network.proxy"]; !strings.Contains(c.Message, "mihomo answered HTTP 502: the selected proxy could not reach the site") ||
		c.Hint != selectedProxyHint {
		t.Errorf("proxy: %+v", c)
	}
}

// 内核连不上节点时直接断开 CONNECT，Go 报告 EOF（tunlr-dev 上实际遇到的情况）。
func TestNetworkProxyClosed(t *testing.T) {
	gh := githubAt(t, netNow)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer proxy.Close()
	addr := strings.TrimPrefix(proxy.URL, "http://")
	c := networkStatuses(t, NetworkOptions{APIBase: gh.URL, Proxy: addr, ProbeURL: "https://probe.test/generate_204"})["network.proxy"]
	want := "mihomo closed the connection: the selected proxy did not respond (https://probe.test/generate_204 via " + addr + ")"
	if c.Status != Warn || c.Message != want || c.Hint != selectedProxyHint {
		t.Errorf("proxy = %+v", c)
	}

	// mixed-port 上没有程序在监听。
	proxy.Close()
	c = networkStatuses(t, NetworkOptions{APIBase: gh.URL, Proxy: addr, ProbeURL: "http://probe.test/"})["network.proxy"]
	if !strings.HasPrefix(c.Message, "cannot connect to mihomo's mixed-port "+addr) || !strings.Contains(c.Hint, "tunlr core restart") {
		t.Errorf("refused: %+v", c)
	}
}

func TestDescribeProxyFailure(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		want   string
	}{
		{context.DeadlineExceeded, 0, "no answer within 8s: the selected proxy is down or very slow"},
		{errors.New("proxyconnect tcp: tls handshake failure"), 0, "the request through mihomo failed: proxyconnect tcp: tls handshake failure"},
		{nil, http.StatusGatewayTimeout, "mihomo answered HTTP 504: the selected proxy could not reach the site"},
		{nil, http.StatusForbidden, "the site answered HTTP 403 through mihomo"},
	} {
		if msg, hint := describeProxyFailure("127.0.0.1:7890", tc.err, tc.status); msg != tc.want || hint == "" {
			t.Errorf("%v/%d: %q, hint %q", tc.err, tc.status, msg, hint)
		}
	}
}
