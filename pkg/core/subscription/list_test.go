package subscription

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
)

func TestParseListRejects(t *testing.T) {
	for _, tt := range []struct{ name, data, want string }{
		{"unknown key", "subscriptions:\n  - {name: a, url: https://x.example/, updat: 1d}\n", "updat"},
		{"newer", "version: 9\n", "newer than this tunlr"},
		{"bad name", "subscriptions:\n  - {name: ../a, url: https://x.example/}\n", "subscription name"},
		{"yaml suffix", "subscriptions:\n  - {name: home.YAML, url: https://x.example/}\n", "not ending in .yaml"},
		{"twice", "subscriptions:\n  - {name: a, url: https://x.example/}\n  - {name: a, path: /a.yaml}\n", "listed twice"},
		{"case", "subscriptions:\n  - {name: home, url: https://x.example/}\n  - {name: Home, path: /a.yaml}\n", `"home" and "Home" differ only in case`},
		{"no source", "subscriptions:\n  - {name: a}\n", "exactly one of url and path"},
		{"both", "subscriptions:\n  - {name: a, url: https://x.example/, path: /a.yaml}\n", "exactly one of url and path"},
		{"bad url", "subscriptions:\n  - {name: a, url: ftp://secret-token@x.example/}\n", "http or https"},
		{"relative", "subscriptions:\n  - {name: a, path: a.yaml}\n", "path must be absolute"},
		{"agent for stdin", "subscriptions:\n  - {name: a, path: '-', user-agent: x}\n", "only to a subscription URL"},
		{"period", "subscriptions:\n  - {name: a, url: https://x.example/, update: 1w}\n", "invalid update period"},
		{"stdin period", "subscriptions:\n  - {name: a, path: '-', update: 1d}\n", "standard input"},
		{"active missing", "active: b\nsubscriptions:\n  - {name: a, url: https://x.example/}\n", `"b" is not in the list`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseList([]byte(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Errorf("error leaks the URL: %v", err)
			}
		})
	}
	if l, err := ParseList([]byte("# only comments\n")); err != nil || l.Version != ListVersion || len(l.Subscriptions) != 0 {
		t.Errorf("empty list = %+v, %v", l, err)
	}
}

// TestSaveListKeepsComments 验证工具写订阅列表时保留用户的注释和顺序，只改变化的值。
func TestSaveListKeepsComments(t *testing.T) {
	m := newManager(t)
	path := m.Layout.SubscriptionList()
	if err := os.MkdirAll(m.Layout.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	hand := "# my subscriptions\nactive: a # the main one\nsubscriptions:\n" +
		"  # the airport\n  - name: a\n    url: https://a.example/sub\n    update: 6h # fast\n" +
		"  - name: b\n    path: " + filepath.ToSlash(filepath.Join(t.TempDir(), "b.yaml")) + "\n"
	if err := os.WriteFile(path, []byte(hand), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := LoadList(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Active = "b"
	l.Subscriptions[0].UserAgent = "x/1.0"
	l.Subscriptions[0].Update = ""
	l.Subscriptions = append(l.Subscriptions, Def{Name: "c", URL: "https://c.example/"})
	if err := SaveList(path, l); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	for _, want := range []string{"# my subscriptions", "active: b # the main one", "# the airport", "user-agent: x/1.0", "name: c"} {
		if !strings.Contains(got, want) {
			t.Errorf("saved list lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "update:") || strings.Index(got, "name: a") > strings.Index(got, "name: b") {
		t.Errorf("saved list:\n%s", got)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("permissions = %v", fi.Mode().Perm())
	}
	// 删掉一项。
	l.Subscriptions = l.Subscriptions[1:]
	if err := SaveList(path, l); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "name: a") || !strings.Contains(string(data), "# my subscriptions") {
		t.Errorf("after removing a:\n%s", data)
	}
}

// TestHandAddedSubscription 验证手写进订阅列表的订阅：没有抓取记录，立即到期；抓取后有记录。
// 手动删掉的订阅，下次保存时记录也去掉。
func TestHandAddedSubscription(t *testing.T) {
	p := newProvider(t)
	m := newManager(t)
	ctx := context.Background()
	if _, err := m.Add(ctx, "a", p.URL+"/ok", AddOptions{}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(m.Layout.SubscriptionList())
	hand := strings.Replace(string(data), "  - name: a\n", "  - name: b\n    url: "+p.URL+"/ok\n  - name: a\n", 1)
	if err := os.WriteFile(m.Layout.SubscriptionList(), []byte(hand), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := m.List()
	if err != nil || len(entries) != 2 || entries[0].Name != "b" {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if !entries[0].FetchedAt.IsZero() || !NextUpdate(&entries[0].Subscription).IsZero() && NextUpdate(&entries[0].Subscription).After(m.now()) {
		t.Errorf("hand-added subscription is not due: %+v", entries[0].Subscription)
	}
	if _, err := m.Update(ctx, []string{"b"}, UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := m.List(); entries[0].FetchedAt.IsZero() {
		t.Errorf("no record after fetching: %+v", entries[0].Subscription)
	}

	// 手动删掉 a（同时删掉 active，就没有启用的订阅）。
	if err := os.WriteFile(m.Layout.SubscriptionList(), []byte("active: a\nsubscriptions: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.List(); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("active not in the list: %v", err)
	}
}
