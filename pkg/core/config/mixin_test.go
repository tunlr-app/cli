package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/templates"
)

// uncommentExample 取消示例 mixin 中各段示例的注释（从第一个“# ----”段落标题开始）。
func uncommentExample(example []byte) string {
	var b strings.Builder
	started := false
	for line := range strings.Lines(string(example)) {
		if strings.HasPrefix(line, "# ----") {
			started = true
			continue
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
proxy-groups:
  - {name: 节点选择, type: select, proxies: [自动选择, 剩余流量：10GB, 香港 01]}
  - {name: 自动选择, type: url-test, url: http://www.gstatic.com/generate_204, interval: 300, proxies: [香港 01]}
rules:
  - MATCH,节点选择
`)
	base := Layers{Template: templates.Config, Defaults: []byte("secret: s\n"), Subscription: sub}
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
	if strings.Contains(cfg, "剩余流量") {
		t.Errorf("info node not deleted:\n%s", cfg)
	}
}

func TestMixinFileName(t *testing.T) {
	for in, want := range map[string]string{"20-rules": "20-rules.yaml", "dns.yaml": "dns.yaml", "a_b.c": "a_b.c.yaml"} {
		if got, err := MixinFileName(in); err != nil || got != want {
			t.Errorf("MixinFileName(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", ".yaml", ".hidden", "../x", "a/b", "a b", strings.Repeat("a", 65)} {
		if _, err := MixinFileName(in); err == nil {
			t.Errorf("MixinFileName(%q) accepted", in)
		}
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
}
