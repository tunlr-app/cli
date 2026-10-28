package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/templates"
)

func TestCheckExposure(t *testing.T) {
	// 模板本身（加上生成的 secret）是安全的。
	safe := strings.Replace(string(templates.Config), "\nsecret: \"\"\n", "\nsecret: s\n", 1)
	if safe == string(templates.Config) {
		t.Fatal("template has no empty secret")
	}
	if err := CheckExposure([]byte(safe)); err != nil {
		t.Fatalf("template: %v", err)
	}

	for _, tc := range []struct {
		name, cfg string
		want      []string // 每个问题的开头；为空表示安全
	}{
		{"loopback only", "allow-lan: false\nexternal-controller: 127.0.0.1:9090\nsecret: s\ndns: {enable: true, listen: 127.0.0.1:1053}\n", nil},
		{"allow-lan", "allow-lan: true\n", []string{"allow-lan is on"}},
		{"allow-lan with auth", "allow-lan: true\nauthentication: [\"u:p\"]\n", nil},
		{"allow-lan empty auth", "allow-lan: true\nauthentication: []\n", []string{"allow-lan is on"}},
		{"empty secret", "external-controller: 127.0.0.1:9090\nsecret: \"\"\n", []string{"secret is empty"}},
		{"no controller", "secret: \"\"\n", nil},
		{"dns public", "dns: {enable: true, listen: 0.0.0.0:53}\n", []string{"dns.listen is 0.0.0.0:53"}},
		{"dns ipv6 loopback", "dns: {enable: true, listen: \"[::1]:53\"}\n", nil},
		{"dns disabled", "dns: {enable: false, listen: 0.0.0.0:53}\n", nil},
		{"listeners", `listeners:
  - {name: open, type: mixed, port: 1}
  - {name: local, type: socks, port: 2, listen: 127.0.0.1}
  - {name: users, type: http, port: 3, users: [{username: u, password: p}]}
  - {name: tunnel, type: tunnel, port: 4, target: example.com:80}
  - {name: v6, type: socks, port: 5, listen: "::"}
`, []string{`the listener "open" (mixed)`, `the listener "v6" (socks)`}},
		{"listener inherits auth", "authentication: [\"u:p\"]\nlisteners:\n  - {name: a, type: mixed, port: 1}\n  - {name: b, type: mixed, port: 2, users: []}\n",
			[]string{`the listener "b" (mixed)`}},
	} {
		err := CheckExposure([]byte(tc.cfg))
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		var ee *ExposureError
		if !errors.As(err, &ee) || !errors.Is(err, core.ErrCheckFailed) {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(ee.Problems) != len(tc.want) {
			t.Errorf("%s: %q", tc.name, ee.Problems)
			continue
		}
		for i, w := range tc.want {
			if !strings.HasPrefix(ee.Problems[i], w) {
				t.Errorf("%s: problem %d = %q, want prefix %q", tc.name, i, ee.Problems[i], w)
			}
		}
	}
}
