package subscription

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/state"
)

const sampleConfig = `port: 7890
proxies:
  - {name: a, type: ss, server: a.example.net, port: 1, cipher: aes-128-gcm, password: p}
proxy-groups:
  - {name: g, type: select, proxies: [a]}
rules:
  - MATCH,g
`

// provider 模拟机场：/ok 返回完整配置，/b64 返回节点列表，broken 置位后一律返回 500。
type provider struct {
	*httptest.Server
	broken    atomic.Bool
	userAgent atomic.Value
}

func newProvider(t *testing.T) *provider {
	p := &provider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.userAgent.Store(r.UserAgent())
		if p.broken.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Subscription-Userinfo", "upload=100; download=200; total=1000; expire=1798761600")
			w.Write([]byte(sampleConfig))
		case "/b64":
			w.Write([]byte("c3M6Ly9ZV1Z6TFRJMU5pMW5ZMjA2QGV4YW1wbGUubmV0OjQ0Mw==\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

func newManager(t *testing.T) *Manager {
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	return &Manager{
		Layout:  paths.Layout{Root: t.TempDir()},
		Version: "test",
		Now:     func() time.Time { return now },
	}
}

func TestAddListRemove(t *testing.T) {
	p := newProvider(t)
	m := newManager(t)
	ctx := context.Background()

	res, err := m.Add(ctx, "first", p.URL+"/ok?token=secret", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Activated || res.Summary.Proxies != 1 || res.Summary.Rules != 1 {
		t.Errorf("first add = %+v", res)
	}
	if ua, _ := p.userAgent.Load().(string); ua != "clash.meta mihomo tunlr/test" {
		t.Errorf("User-Agent = %q", ua)
	}
	res, err = m.Add(ctx, "second", p.URL+"/ok", AddOptions{UserAgent: "custom-agent/1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Activated {
		t.Error("second subscription should not become active")
	}
	if ua, _ := p.userAgent.Load().(string); ua != "custom-agent/1.0" {
		t.Errorf("User-Agent = %q", ua)
	}

	entries, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "first" || !entries[0].Active || entries[1].Active {
		t.Fatalf("entries = %+v", entries)
	}
	info := entries[0].UserInfo
	if info == nil || info.Upload != 100 || info.Download != 200 || info.Total != 1000 || info.Expire.Year() != 2027 {
		t.Errorf("userinfo = %+v", info)
	}
	if entries[0].Summary.Proxies != 1 || entries[0].Counts == nil {
		t.Errorf("summary = %+v, counts = %+v", entries[0].Summary, entries[0].Counts)
	}
	// 列出时用状态文件中记下的统计，不再解析内容；较早的状态文件没有统计时才解析。
	if err := os.WriteFile(m.Layout.SubscriptionFile("first"), []byte("proxies: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if entries, _ := m.List(); entries[0].Summary.Proxies != 1 {
		t.Errorf("summary from state = %+v", entries[0].Summary)
	}
	st, _ := state.Load(m.Layout.StateFile())
	st.Subscriptions[0].Counts = nil
	if err := state.Save(m.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	if entries, _ := m.List(); entries[0].Summary.Proxies != 0 {
		t.Errorf("summary without counts = %+v", entries[0].Summary)
	}

	if _, err := m.Add(ctx, "first", p.URL+"/ok", AddOptions{}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate add: err = %v", err)
	}
	if err := m.Remove(ctx, "first"); !errors.Is(err, ErrActive) {
		t.Errorf("remove active: err = %v", err)
	}
	if err := m.Remove(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.Layout.SubscriptionFile("second")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("content of removed subscription still exists: %v", err)
	}
	if err := m.Remove(ctx, "second"); !errors.Is(err, ErrNotFound) {
		t.Errorf("remove missing: err = %v", err)
	}
}

func TestAddRejects(t *testing.T) {
	p := newProvider(t)
	m := newManager(t)
	ctx := context.Background()
	tests := []struct {
		name, subName, url string
		check              func(error) bool
	}{
		{"bad name", "../etc", p.URL + "/ok", func(err error) bool { return errors.Is(err, core.ErrInvalidArgument) }},
		{"bad scheme", "a", "ftp://example.net/sub", func(err error) bool { return errors.Is(err, core.ErrInvalidArgument) }},
		{"node list", "a", p.URL + "/b64", func(err error) bool { return strings.Contains(err.Error(), "--user-agent") }},
		{"http error", "a", p.URL + "/missing?token=secret", func(err error) bool { return strings.Contains(err.Error(), "HTTP 404") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := m.Add(ctx, tt.subName, tt.url, AddOptions{})
			if err == nil || !tt.check(err) {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error leaks the URL token: %v", err)
			}
		})
	}
	if _, err := os.Stat(m.Layout.StateFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("failed adds created the state file: %v", err)
	}
}

func TestFetchErrorHidesToken(t *testing.T) {
	m := newManager(t)
	// 端口 1 上通常没有服务，连接会被拒绝；错误信息中不能出现令牌。
	_, err := m.Add(context.Background(), "a", "http://127.0.0.1:1/sub?token=secret", AddOptions{})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "http://127.0.0.1:1/...") {
		t.Errorf("error should name the redacted URL: %v", err)
	}
}

func TestUpdateKeepsLastGood(t *testing.T) {
	p := newProvider(t)
	m := newManager(t)
	ctx := context.Background()
	if _, err := m.Add(ctx, "a", p.URL+"/ok", AddOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(m.Layout.SubscriptionFile("a"))
	if err != nil {
		t.Fatal(err)
	}

	p.broken.Store(true)
	results, err := m.Update(ctx, nil, UpdateOptions{})
	if err == nil || len(results) != 1 || results[0].Err == nil {
		t.Fatalf("update while broken: results=%+v err=%v", results, err)
	}
	after, err := os.ReadFile(m.Layout.SubscriptionFile("a"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("content changed after failed update: %v", err)
	}
	st, err := state.Load(m.Layout.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	if s := st.Find("a"); s.LastError == "" || s.LastErrorAt.IsZero() || s.FetchedAt.IsZero() {
		t.Errorf("failure not recorded: %+v", s)
	}

	p.broken.Store(false)
	if _, err := m.Update(ctx, []string{"a"}, UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err = state.Load(m.Layout.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	if s := st.Find("a"); s.LastError != "" || !s.LastErrorAt.IsZero() {
		t.Errorf("error not cleared after success: %+v", s)
	}

	if _, err := m.Update(ctx, []string{"nope"}, UpdateOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown: err = %v", err)
	}
}

// TestUserAgent 验证 User-Agent 的优先级（订阅 > 工具设置 > 内置默认），
// 以及修改已有订阅的 User-Agent 只在抓取成功时保存。
func TestUserAgent(t *testing.T) {
	p := newProvider(t)
	m := newManager(t)
	ctx := context.Background()
	lastUA := func() string { ua, _ := p.userAgent.Load().(string); return ua }
	update := func(ua *string) error {
		_, err := m.Update(ctx, []string{"a"}, UpdateOptions{UserAgent: ua})
		return err
	}
	savedUA := func() string {
		st, err := state.Load(m.Layout.StateFile())
		if err != nil {
			t.Fatal(err)
		}
		return st.Find("a").UserAgent
	}

	if _, err := m.Add(ctx, "a", p.URL+"/ok", AddOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.Layout.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Layout.SettingsFile(), []byte("user-agent: global/2.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := update(nil); err != nil || lastUA() != "global/2.0" {
		t.Fatalf("settings default: UA %q, err %v", lastUA(), err)
	}

	ua := "own/3.0"
	if err := update(&ua); err != nil || lastUA() != "own/3.0" || savedUA() != "own/3.0" {
		t.Fatalf("own UA: sent %q, saved %q, err %v", lastUA(), savedUA(), err)
	}
	if err := update(nil); err != nil || lastUA() != "own/3.0" {
		t.Fatalf("own UA should win over settings: sent %q, err %v", lastUA(), err)
	}

	// 抓取失败时不保存新的 User-Agent。
	p.broken.Store(true)
	bad := "broken/1.0"
	if err := update(&bad); err == nil || savedUA() != "own/3.0" {
		t.Fatalf("failed fetch: saved %q, err %v", savedUA(), err)
	}
	p.broken.Store(false)

	// 空字符串恢复默认。
	empty := ""
	if err := update(&empty); err != nil || savedUA() != "" || lastUA() != "global/2.0" {
		t.Fatalf("reset: sent %q, saved %q, err %v", lastUA(), savedUA(), err)
	}

	if _, err := m.Update(ctx, nil, UpdateOptions{UserAgent: &ua}); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("UA without names: err = %v", err)
	}
	injected := "a\r\nX: 1"
	if err := update(&injected); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("control characters: err = %v", err)
	}
	if _, err := m.Add(ctx, "b", p.URL+"/ok", AddOptions{UserAgent: injected}); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("add with control characters: err = %v", err)
	}
}

func TestUse(t *testing.T) {
	p := newProvider(t)
	m := newManager(t)
	ctx := context.Background()
	for _, name := range []string{"a", "b"} {
		if _, err := m.Add(ctx, name, p.URL+"/ok", AddOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := m.Use(ctx, "b")
	if err != nil || !res.Activated {
		t.Fatalf("use b: %+v, %v", res, err)
	}
	if res, err := m.Use(ctx, "b"); err != nil || res.Activated {
		t.Fatalf("use b again: %+v, %v", res, err)
	}

	// a 的内容丢失且机场不可用时，不能切换过去。
	if err := os.Remove(m.Layout.SubscriptionFile("a")); err != nil {
		t.Fatal(err)
	}
	p.broken.Store(true)
	if _, err := m.Use(ctx, "a"); err == nil || !strings.Contains(err.Error(), "not switching to a") {
		t.Fatalf("use a while broken: err = %v", err)
	}
	if entries, _ := m.List(); !entries[1].Active {
		t.Error("active subscription changed after failed switch")
	}

	// 机场恢复后，切换时先抓取。
	p.broken.Store(false)
	if res, err := m.Use(ctx, "a"); err != nil || !res.Activated || res.Summary.Proxies != 1 {
		t.Fatalf("use a: %+v, %v", res, err)
	}
	if _, err := m.Use(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("use unknown: err = %v", err)
	}
}

func TestNotInstalled(t *testing.T) {
	m := newManager(t)
	if entries, err := m.List(); err != nil || entries != nil {
		t.Errorf("List = %v, %v", entries, err)
	}
	if _, err := m.Update(context.Background(), nil, UpdateOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update: err = %v", err)
	}
}

func TestParseUserInfo(t *testing.T) {
	tests := []struct {
		header string
		want   *state.UserInfo
	}{
		{"", nil},
		{"garbage", nil},
		{"upload=1; download=2; total=3", &state.UserInfo{Upload: 1, Download: 2, Total: 3}},
		{" Upload = 1.0 ;download=2;total=3e3;expire=", &state.UserInfo{Upload: 1, Download: 2, Total: 3000}},
		{"total=10; expire=1700000000; other=x", &state.UserInfo{Total: 10, Expire: time.Unix(1700000000, 0).UTC()}},
	}
	for _, tt := range tests {
		got := parseUserInfo(tt.header)
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("parseUserInfo(%q) = %+v, want %+v", tt.header, got, tt.want)
		}
	}
}

func TestRedact(t *testing.T) {
	if got := Redact("https://sub.example.com/api/v1/client/subscribe?token=abc"); got != "https://sub.example.com/..." {
		t.Errorf("Redact = %q", got)
	}
	if got := Redact("::"); got != "(invalid URL)" {
		t.Errorf("Redact = %q", got)
	}
}
