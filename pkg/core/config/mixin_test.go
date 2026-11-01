package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/templates"
)

// uncommentExample 取消示例 mixin 中各段示例的注释（从“# mihomo:”开始）。
func uncommentExample(example []byte) string {
	var b strings.Builder
	started := false
	for line := range strings.Lines(string(example)) {
		if strings.HasPrefix(line, "# mihomo:") {
			started = true
		}
		if started {
			line = strings.TrimPrefix(line, "# ")
		}
		b.WriteString(line)
	}
	return b.String()
}

// TestMixinExample 确认新建 mixin 时的示例全是注释（不改变配置），
// 取消注释后能用于一份典型的机场配置，且没有警告。
func TestMixinExample(t *testing.T) {
	sub := []byte(`proxies:
  - {name: 剩余流量：10GB, type: ss, server: x.example.net, port: 1, cipher: aes-128-gcm, password: p}
  - {name: 香港 01, type: ss, server: hk.example.net, port: 1, cipher: aes-128-gcm, password: p}
  - {name: 香港 02 倍率 3, type: ss, server: hk2.example.net, port: 1, cipher: aes-128-gcm, password: p}
proxy-groups:
  - {name: 节点选择, type: select, proxies: [自动选择, 剩余流量：10GB, 香港 01]}
  - {name: 自动选择, type: url-test, url: http://www.gstatic.com/generate_204, interval: 300, proxies: [香港 01]}
rules:
  - MATCH,节点选择
`)
	base := Layers{Template: templates.Config, Subscription: sub, Managed: []Managed{
		builtinDefaults,
		{Name: "tunlr:tun", Data: []byte("mihomo:\n  tun:\n    enable: true\n    auto-route: true\n")},
		{Name: "tunlr:panel", Data: []byte("mihomo:\n  external-controller: 127.0.0.1:9090\n  secret: s\n")},
	}}
	want, err := Compose(base)
	if err != nil {
		t.Fatal(err)
	}

	commented := base
	commented.Mixins = []Mixin{{Name: "10-example.yaml", Data: templates.Mixin}}
	res, err := Compose(commented)
	if err != nil || string(res.Config) != string(want.Config) {
		t.Fatalf("the commented example changes the configuration: %v\n%s", err, res.Config)
	}

	example := uncommentExample(templates.Mixin)
	uncommented := base
	uncommented.Mixins = []Mixin{{Name: "10-example.yaml", Data: []byte(example)}}
	res, err = Compose(uncommented)
	if err != nil {
		t.Fatalf("uncommented example: %v\n%s", err, example)
	}
	if len(res.Warnings) > 0 {
		t.Errorf("warnings: %v", res.Warnings)
	}
	cfg := string(res.Config)
	for _, want := range []string{
		"proxies: [家宽, 自动选择, 香港 01]",
		"interval: 600",
		"  - DOMAIN-SUFFIX,corp.example.com,家宽\n  - GEOIP,LAN,DIRECT\n  - MATCH,节点选择\n",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "剩余流量") || strings.Contains(cfg, "倍率") {
		t.Errorf("info node or filtered proxy not removed:\n%s", cfg)
	}
	if f := FakeIPFilter(res.Config); !slices.Contains(f, "+.corp.example.com") || slices.Contains(f, "stun.*.*") || !slices.Contains(f, "+.lan") {
		t.Errorf("fake-ip-filter: %v", f)
	}
}

// TestBuiltinDefaults 确认内置的兜底值 tunlr:defaults 有效，给出全部 geo 数据设置、DNS 和嗅探，模板中没有这些键。
func TestBuiltinDefaults(t *testing.T) {
	for _, k := range []string{"\ngeox-url:", "\ngeo-auto-update:", "\ngeo-update-interval:", "\ndns:", "\nsniffer:"} {
		if strings.Contains(string(templates.Config), k) {
			t.Errorf("template still has %s", strings.TrimSpace(k))
		}
	}
	res, err := Compose(Layers{Template: templates.Config, Managed: []Managed{builtinDefaults}})
	if err != nil || len(res.Warnings) > 0 {
		t.Fatalf("%v %v", err, res.Warnings)
	}
	for _, s := range res.GeoSources {
		if s.From != builtinDefaults.Name || !s.Fallback || s.Value == "" {
			t.Errorf("%+v", s)
		}
	}
	for _, s := range res.DNSSources {
		if s.From != builtinDefaults.Name || !s.Fallback || !s.Set || len(s.ChangedBy) > 0 {
			t.Errorf("%+v", s)
		}
	}
	// 中国的域名经直连问中国的 DoH，其他的按规则；节点的域名经直连解析（respect-rules 要求写它）。
	for _, want := range []string{"\ndns:\n  enable: true\n  ipv6: false\n", "\n  enhanced-mode: fake-ip\n", "\n  respect-rules: true\n",
		"\n    geosite:cn:\n      - https://223.5.5.5/dns-query#DIRECT\n", "\n  proxy-server-nameserver:\n    - https://223.5.5.5/dns-query\n",
		"\nsniffer:\n  enable: true\n"} {
		if !strings.Contains(string(res.Config), want) {
			t.Errorf("config lacks %q:\n%s", want, res.Config)
		}
	}
	if !res.Facts.Geo.GeoSite {
		t.Error("geosite:cn in the DNS does not need GeoSite")
	}
}

func TestMixinFileName(t *testing.T) {
	for in, want := range map[string]string{"20-rules": "20-rules.yaml", "dns.yaml": "dns.yaml", "a_b.c": "a_b.c.yaml",
		"home/10-filter": "home/10-filter.yaml", "home/10-filter.yaml": "home/10-filter.yaml"} {
		if got, err := MixinFileName(in); err != nil || got != want {
			t.Errorf("MixinFileName(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", ".yaml", ".hidden", "../x", "/x", "a/", "a/b/c", "a/../b", ".a/b", "a b", strings.Repeat("a", 65)} {
		if _, err := MixinFileName(in); err == nil {
			t.Errorf("MixinFileName(%q) accepted", in)
		}
	}
}

func TestOrderWarnings(t *testing.T) {
	var mixins []Mixin
	for _, name := range []string{"010-x.yaml", "10-a.yaml", "20-b.yaml", "9-c.yaml", "dns.yaml"} {
		mixins = append(mixins, Mixin{Name: name})
	}
	var got []string
	for _, w := range orderWarnings(mixins) {
		got = append(got, w.String())
	}
	want := []string{"9-c.yaml: applied after 010-x.yaml because names sort character by character; to apply it before, rename it to 009-c.yaml"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("warnings:\n%s", strings.Join(got, "\n"))
	}
	if compareNumbers("0010", "9") <= 0 || compareNumbers("007", "7") != 0 || compareNumbers("123456789012345678901234567890", "2") <= 0 {
		t.Error("compareNumbers")
	}
}

func TestLoadMixins(t *testing.T) {
	dir := t.TempDir()
	if m, err := LoadMixins(filepath.Join(dir, "missing")); err != nil || m != nil {
		t.Fatalf("missing dir: %v %v", m, err)
	}
	for name, content := range map[string]string{
		"20-b.yaml": "b", "10-a.yaml": "a", ".10-a.yaml.swp": "x", "notes.yml": "x", "README": "x",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "main.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMixins(dir)
	if err != nil || len(m) != 2 || m[0].Name != "10-a.yaml" || string(m[0].Data) != "a" || m[1].Name != "20-b.yaml" {
		t.Fatalf("mixins = %+v, %v", m, err)
	}

	// 订阅 mixin：LoadMixinsFor 只带上这个订阅的，排在共用的之后；LoadAllMixins 带上所有子目录的。
	for _, name := range []string{"home/05-x.yaml", "home/deep/1.yaml", "work/10-y.yaml", ".git/1.yaml"} {
		path := MixinPath(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	names := func(ms []Mixin) string {
		var s []string
		for _, m := range ms {
			s = append(s, m.Name)
		}
		return strings.Join(s, ",")
	}
	if m, err := LoadMixinsFor(dir, "home"); err != nil || names(m) != "10-a.yaml,20-b.yaml,home/05-x.yaml" {
		t.Errorf("LoadMixinsFor = %s, %v", names(m), err)
	}
	if m, err := LoadMixinsFor(dir, "none"); err != nil || names(m) != "10-a.yaml,20-b.yaml" {
		t.Errorf("LoadMixinsFor(none) = %s, %v", names(m), err)
	}
	if m, err := LoadAllMixins(dir); err != nil || names(m) != "10-a.yaml,20-b.yaml,home/05-x.yaml,work/10-y.yaml" {
		t.Errorf("LoadAllMixins = %s, %v", names(m), err)
	}
}

func TestFilterMixin(t *testing.T) {
	data := FilterMixin("home", "香港`(?i)hk", "")
	res, err := Compose(Layers{Template: []byte("a: 1\n"),
		Subscription: []byte("proxies:\n  - {name: 香港 1}\n  - {name: HK 2}\n  - {name: 日本 3}\n"),
		Mixins:       []Mixin{{Name: "home/" + FilterMixinFile, Data: data}}})
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if res.ProxyFilter == nil || res.ProxyFilter.Kept != 2 {
		t.Errorf("ProxyFilter = %+v\n%s", res.ProxyFilter, data)
	}
	if !strings.Contains(string(data), "\n    filter: 香港`(?i)hk\n    # exclude-filter: ") || !strings.Contains(string(data), "\n    # exclude-type: ") {
		t.Errorf("unset keys are not shown as examples:\n%s", data)
	}
	// 都为空时（config edit 新建）整段是注释，三个字段都有示例。
	empty := FilterMixin("home", "", "")
	res, err = Compose(Layers{Template: []byte("a: 1\n"), Subscription: []byte("proxies:\n  - {name: 香港 1}\n"),
		Mixins: []Mixin{{Name: "home/" + FilterMixinFile, Data: empty}}})
	if err != nil || res.ProxyFilter != nil {
		t.Errorf("empty filter: %+v, %v\n%s", res, err, empty)
	}
	for _, k := range proxyFilterKeys {
		if !strings.Contains(string(empty), "\n#     "+k+": ") {
			t.Errorf("no example of %s:\n%s", k, empty)
		}
	}
	if q := FilterMixin("home", "`a", ""); !strings.Contains(string(q), "    filter: '`a'\n") {
		t.Errorf("value not quoted:\n%s", q)
	}
	if CheckPatterns("a`(") == nil || CheckPatterns("a`b") != nil {
		t.Error("CheckPatterns")
	}
}
