package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/txtar"

	"github.com/tunlr-app/cli/templates"
)

var update = flag.Bool("update", false, "rewrite the want sections of golden files")

// TestComposeGolden 读取 testdata/compose/*.txtar。每个文件包含输入
// template.yaml（缺省时使用内置模板）、subscription.yaml、受管 mixin（managed/<名称> 独占键，
// default/<名称> 提供默认值）和 mixin.d/<文件名>（按出现顺序应用），以及期望输出 want.yaml、dropped（被丢弃的订阅键）、
// warnings 和 overrides（每行一条）。用 go test ./pkg/core/config -update 重新生成期望输出。
func TestComposeGolden(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "compose", "*.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no golden files")
	}
	for _, file := range files {
		t.Run(strings.TrimSuffix(filepath.Base(file), ".txtar"), func(t *testing.T) {
			ar, err := txtar.ParseFile(file)
			if err != nil {
				t.Fatal(err)
			}
			sections := map[string][]byte{}
			var mixins []Mixin
			var managed []Managed
			for _, f := range ar.Files {
				sections[f.Name] = f.Data
				if name, ok := strings.CutPrefix(f.Name, "mixin.d/"); ok {
					mixins = append(mixins, Mixin{Name: name, Data: f.Data})
				}
				if name, ok := strings.CutPrefix(f.Name, "managed/"); ok {
					managed = append(managed, Managed{Name: name, Data: f.Data})
				}
				if name, ok := strings.CutPrefix(f.Name, "default/"); ok {
					managed = append(managed, Managed{Name: name, Default: true, Data: f.Data})
				}
			}
			l := Layers{
				Template:     sections["template.yaml"],
				Subscription: sections["subscription.yaml"],
				Managed:      managed,
				Mixins:       mixins,
			}
			if _, ok := sections["template.yaml"]; !ok {
				l.Template = templates.Config
			}
			res, err := Compose(l)
			if err != nil {
				t.Fatal(err)
			}
			lines := func(items []string) []byte {
				if len(items) == 0 {
					return nil
				}
				return []byte(strings.Join(items, "\n") + "\n")
			}
			var warnings, overrides []string
			for _, w := range res.Warnings {
				warnings = append(warnings, w.String())
			}
			for _, o := range res.Overrides {
				overrides = append(overrides, fmt.Sprintf("%s: %s overrides %s", o.Path, o.File, o.Previous))
			}
			got := map[string][]byte{
				"want.yaml": res.Config,
				"dropped":   lines(res.Dropped),
				"warnings":  lines(warnings),
				"overrides": lines(overrides),
			}

			if *update {
				for _, name := range []string{"want.yaml", "dropped", "warnings", "overrides"} {
					setSection(ar, name, got[name])
				}
				if err := os.WriteFile(file, txtar.Format(ar), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			// 合成时提取的 Facts 与重新解析输出得到的相同。
			if f, err := Inspect(res.Config); err != nil || !reflect.DeepEqual(res.Facts, f) {
				t.Errorf("Facts = %+v, Inspect(Config) = %+v, %v", res.Facts, f, err)
			}
			if want := sections["want.yaml"]; !bytes.Equal(res.Config, want) {
				t.Errorf("config mismatch (run with -update to accept)\n--- got ---\n%s\n--- want ---\n%s", res.Config, want)
			}
			for _, name := range []string{"dropped", "warnings", "overrides"} {
				if want := sections[name]; !bytes.Equal(got[name], want) {
					t.Errorf("%s mismatch\n--- got ---\n%s--- want ---\n%s", name, got[name], want)
				}
			}
		})
	}
}

// setSection 替换归档中的一节；data 为空时删除该节。
func setSection(ar *txtar.Archive, name string, data []byte) {
	files := ar.Files[:0]
	for _, f := range ar.Files {
		if f.Name != name {
			files = append(files, f)
		}
	}
	ar.Files = files
	if len(data) > 0 {
		ar.Files = append(ar.Files, txtar.File{Name: name, Data: data})
	}
}

// TestTemplateRoundTrip 确认只有内置模板时输出与模板逐字一致（键顺序、注释、引号都保留），
// 也就是模板本身已是规范格式，生成的配置 diff 干净。
func TestTemplateRoundTrip(t *testing.T) {
	res, err := Compose(Layers{Template: templates.Config})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Config, templates.Config) {
		t.Errorf("template changed by round trip\n--- got ---\n%s\n--- want ---\n%s", res.Config, templates.Config)
	}
}

func TestComposeErrors(t *testing.T) {
	tests := []struct {
		name string
		l    Layers
		want string
	}{
		{
			name: "empty template",
			l:    Layers{Template: []byte("# only a comment\n")},
			want: "template: empty",
		},
		{
			name: "node-only subscription",
			l:    Layers{Template: []byte("a: 1\n"), Subscription: []byte("c3M6Ly9ZV1Z6TFRJMU5pMW5ZMjA2\n")},
			want: "subscription: not a YAML mapping (only full mihomo configs are supported as subscriptions)",
		},
		{
			name: "invalid mixin yaml",
			l:    mixinLayers("a: 1\n", "a: [\n"),
			want: "mixin 10-test.yaml: yaml: line 1:",
		},
		{
			name: "mixin not a mapping",
			l:    mixinLayers("a: 1\n", "- a\n"),
			want: "mixin 10-test.yaml: not a YAML mapping",
		},
		{
			name: "prepend to non-list",
			l:    mixinLayers("dns:\n  enable: true\n", "x: 1\nprepend-dns:\n  - x\n"),
			want: "mixin 10-test.yaml:2: prepend-dns: dns is not a list",
		},
		{
			name: "nested append to non-list",
			l:    mixinLayers("dns:\n  enable: true\n", "dns:\n  append-enable: [x]\n"),
			want: "mixin 10-test.yaml:2: append-enable: dns.enable is not a list",
		},
		{
			name: "append non-list value",
			l:    mixinLayers("rules: []\n", "append-rules: MATCH,DIRECT\n"),
			want: "mixin 10-test.yaml:1: append-rules: value must be a list",
		},
		{
			name: "operation without key",
			l:    mixinLayers("a: 1\n", "delete-: [a]\n"),
			want: "mixin 10-test.yaml:1: delete-: missing the key to operate on",
		},
		{
			name: "patch item without name",
			l:    mixinLayers("proxies: []\n", "patch-proxies:\n  - udp: true\n"),
			want: "mixin 10-test.yaml:2: patch-proxies: each item must be a mapping with a name",
		},
		{
			name: "patch non-list",
			l:    mixinLayers("dns:\n  enable: true\n", "patch-dns:\n  - name: x\n"),
			want: "mixin 10-test.yaml:1: patch-dns: dns is not a list",
		},
		{
			name: "delete scalar",
			l:    mixinLayers("mode: rule\n", "delete-mode: [rule]\n"),
			want: "mixin 10-test.yaml:1: delete-mode: mode is neither a list nor a mapping",
		},
		{
			name: "invalid regexp",
			l:    mixinLayers("proxies: []\n", "delete-proxies:\n  - /(/\n"),
			want: "mixin 10-test.yaml:2: delete-proxies: invalid regular expression /(/:",
		},
		{
			name: "delete with mapping matcher",
			l:    mixinLayers("proxies: []\n", "delete-proxies:\n  - {name: a}\n"),
			want: "mixin 10-test.yaml:2: delete-proxies: expected a name or /regular expression/",
		},
		{
			name: "error inside patch",
			l:    mixinLayers("proxy-groups:\n  - {name: g, proxies: [a]}\n", "patch-proxy-groups:\n  - name: g\n    prepend-proxies: a\n"),
			want: "mixin 10-test.yaml:3: prepend-proxies: value must be a list",
		},
		{
			name: "namespace not a mapping",
			l:    mixinLayers("a: 1\n", "tunlr: x\n"),
			want: "mixin 10-test.yaml:1: tunlr must be a mapping",
		},
		{
			name: "namespace unknown key",
			l:    mixinLayers("a: 1\n", "tunlr:\n  proxy-filters: {}\n"),
			want: "mixin 10-test.yaml:2: tunlr.proxy-filters: unknown key (known: proxy-filter)",
		},
		{
			name: "proxy filter unknown key",
			l:    mixinLayers("a: 1\n", "tunlr:\n  proxy-filter:\n    include: x\n"),
			want: "mixin 10-test.yaml:3: tunlr.proxy-filter.include: unknown key (known: filter, exclude-filter, exclude-type)",
		},
		{
			name: "proxy filter not a string",
			l:    mixinLayers("a: 1\n", "tunlr:\n  proxy-filter:\n    filter: [a, b]\n"),
			want: "mixin 10-test.yaml:3: tunlr.proxy-filter.filter must be a string",
		},
		{
			name: "proxy filter invalid regexp",
			l:    mixinLayers("a: 1\n", "tunlr:\n  proxy-filter:\n    exclude-filter: a`(\n"),
			want: `mixin 10-test.yaml:3: tunlr.proxy-filter.exclude-filter: invalid regular expression "("`,
		},
		{
			name: "proxy filter removes everything",
			l: Layers{Template: []byte("a: 1\n"), Subscription: []byte("proxies:\n  - {name: a, type: ss}\n"),
				Mixins: []Mixin{{Name: "home/10-f.yaml", Data: []byte("tunlr:\n  proxy-filter:\n    filter: b\n")}}},
			want: "mixin home/10-f.yaml: tunlr.proxy-filter removes all 1 proxies of the subscription",
		},
		{
			name: "rules use an emptied group",
			l: Layers{Template: []byte("a: 1\n"),
				Subscription: []byte("proxies:\n  - {name: hk, type: ss}\n  - {name: us, type: ss}\n" +
					"proxy-groups:\n  - {name: US, type: select, proxies: [us]}\n  - {name: all, type: select, proxies: [US, hk]}\n" +
					"rules:\n  - DOMAIN,a.example,US\n  - MATCH,all\n"),
				Mixins: []Mixin{{Name: "10-f.yaml", Data: []byte("tunlr:\n  proxy-filter:\n    exclude-filter: us\n")}}},
			want: "mixin 10-f.yaml: tunlr.proxy-filter leaves no proxies in group US, but rules still use it: DOMAIN,a.example,US; loosen the filter",
		},
		{
			name: "alias bomb",
			l:    Layers{Template: []byte("a: 1\n"), Subscription: aliasBomb()},
			want: "subscription: more than 1000000 nodes after expanding aliases",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Compose(tt.l)
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("err = %v, want prefix %q", err, tt.want)
			}
		})
	}
}

// TestProxyFilterResult 检查筛选的统计，以及规则引用被删的组时，mixin 改掉这些规则或把组加回来就可以发布。
func TestProxyFilterResult(t *testing.T) {
	sub := "proxies:\n  - {name: hk, type: ss}\n  - {name: us, type: ss}\n" +
		"proxy-groups:\n  - {name: US, type: select, proxies: [us]}\n  - {name: all, type: select, proxies: [US, hk]}\n" +
		"rules:\n  - DOMAIN,a.example,US\n  - MATCH,all\n"
	filter := Mixin{Name: "10-f.yaml", Data: []byte("tunlr:\n  proxy-filter:\n    exclude-filter: us\n")}
	for _, fix := range []string{
		"delete-rules:\n  - DOMAIN,a.example,US\n",
		"append-proxy-groups:\n  - {name: US, type: select, proxies: [DIRECT]}\n",
	} {
		res, err := Compose(Layers{Template: []byte("a: 1\n"), Subscription: []byte(sub),
			Mixins: []Mixin{filter, {Name: "20-fix.yaml", Data: []byte(fix)}}})
		if err != nil {
			t.Fatalf("%q: %v", fix, err)
		}
		want := ProxyFilterResult{Total: 2, Kept: 1, Groups: []string{"US"}}
		if res.ProxyFilter == nil || !reflect.DeepEqual(*res.ProxyFilter, want) {
			t.Errorf("ProxyFilter = %+v, want %+v", res.ProxyFilter, want)
		}
	}

	// 没有订阅时不筛选；空字符串等于没有设置。
	for _, l := range []Layers{
		{Template: []byte("a: 1\n"), Mixins: []Mixin{filter}},
		{Template: []byte("a: 1\n"), Subscription: []byte(sub), Mixins: []Mixin{{Name: "10-f.yaml", Data: []byte("tunlr:\n  proxy-filter:\n    filter: ''\n")}}},
	} {
		if res, err := Compose(l); err != nil || res.ProxyFilter != nil {
			t.Errorf("ProxyFilter = %+v, %v; want nil", res.ProxyFilter, err)
		}
	}
}

// TestRegexpLikeMihomo 确认 mixin 的正则与 mihomo 一样支持前后查找（regexp2）。
func TestRegexpLikeMihomo(t *testing.T) {
	res, err := Compose(mixinLayers("proxies:\n  - {name: 香港 01}\n  - {name: 香港 IPLC}\n",
		"delete-proxies:\n  - /^香港(?!.*IPLC)/\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "proxies:\n  - {name: 香港 IPLC}\n"; string(res.Config) != want {
		t.Errorf("config:\n%s\nwant:\n%s", res.Config, want)
	}
}

// TestComposeConflicts 检查用户的 mixin 动了受管 mixin 独占的键时报错：直接写（即使值相同）、用 null 删除、
// 经上级替换或删除；同一映射中的其他键和提供默认值的受管 mixin 的键可以改。
func TestComposeConflicts(t *testing.T) {
	managed := []Managed{
		{Name: "tunlr:tun", Command: "tunlr tun on/off", Data: []byte("tun:\n  enable: true\n  device: tunlr0\n")},
		{Name: "tunlr:geo", Command: "tunlr update set geo", Data: []byte("geo-auto-update: false\n"), Owns: []string{"geo-update-interval"}},
		{Name: "tunlr:core", Data: []byte("external-controller-unix: api.sock\n")},
		{Name: "tunlr:mirror", Default: true, Data: []byte("prepend-rules: [DOMAIN,m.example,DIRECT]\nexternal-ui-url: https://m.example/ui.zip\n")},
	}
	template := "tun:\n  enable: false\n  stack: mixed\nrules:\n  - MATCH,DIRECT\n"
	for _, tt := range []struct {
		name, mixin, want string
	}{
		{"same value", "tun:\n  enable: true\n", "mixin 10-test.yaml:2: tun.enable is managed by tunlr:tun (change it with: tunlr tun on/off); remove it from the mixin"},
		{"null", "x: 1\ntun:\n  device: null\n", "mixin 10-test.yaml:3: tun.device is managed by tunlr:tun"},
		{"replace parent", "tun: off\n", "mixin 10-test.yaml:1: tun.enable, tun.device are managed by tunlr:tun"},
		{"delete parent", "a: 1\ndelete-tun: [enable]\n", "mixin 10-test.yaml:2: tun.enable is managed by tunlr:tun"},
		{"owned but unset", "geo-update-interval: 1\n", "mixin 10-test.yaml:1: geo-update-interval is managed by tunlr:geo (change it with: tunlr update set geo)"},
		{"required", "external-controller-unix: x.sock\n", "mixin 10-test.yaml:1: external-controller-unix is managed by tunlr:core (tunlr needs it to work)"},
		{"several owners", "tun: {enable: false}\ngeo-auto-update: true\n", "mixin 10-test.yaml:1: tun.enable is managed by tunlr:tun (change it with: tunlr tun on/off); geo-auto-update is managed by tunlr:geo"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := mixinLayers(template, tt.mixin)
			l.Managed = managed
			_, err := Compose(l)
			var me *MixinError
			if !errors.As(err, &me) || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("err = %v, want prefix %q", err, tt.want)
			}
		})
	}

	// 其他键可以改；默认值可以覆盖和删除。
	l := mixinLayers(template, "tun:\n  stack: system\nexternal-ui-url: https://x.example/ui.zip\ndelete-rules: [DOMAIN,m.example,DIRECT]\n")
	l.Managed = managed
	res, err := Compose(l)
	if err != nil {
		t.Fatal(err)
	}
	want := "tun:\n  enable: true\n  stack: system\n  device: tunlr0\n\nrules:\n  - MATCH,DIRECT\n\n" +
		"external-ui-url: https://x.example/ui.zip\ngeo-auto-update: false\nexternal-controller-unix: api.sock\n"
	if string(res.Config) != want {
		t.Errorf("config:\n%s\nwant:\n%s", res.Config, want)
	}
	if len(res.Overrides) != 1 || res.Overrides[0] != (Override{Path: "external-ui-url", File: "10-test.yaml", Previous: "tunlr:mirror"}) {
		t.Errorf("overrides = %+v", res.Overrides)
	}

	// Enforce 只写入独占的键。
	cfg, _, err := Enforce([]byte("tun:\n  enable: false\n"), managed)
	if err != nil || string(cfg) != "tun:\n  enable: true\n  device: tunlr0\n\ngeo-auto-update: false\nexternal-controller-unix: api.sock\n" {
		t.Errorf("Enforce = %s, %v", cfg, err)
	}
}

// mixinLayers 返回以 template 为模板、带一份 mixin 文件 10-test.yaml 的输入。
func mixinLayers(template, mixin string) Layers {
	return Layers{Template: []byte(template), Mixins: []Mixin{{Name: "10-test.yaml", Data: []byte(mixin)}}}
}

// aliasBomb 生成每层引用上一层 10 次的别名链，展开后约 10^9 个节点。
func aliasBomb() []byte {
	var b strings.Builder
	b.WriteString("l0: &l0 [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i <= 9; i++ {
		refs := strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*l%d, ", i-1), 10), ", ")
		fmt.Fprintf(&b, "l%d: &l%d [%s]\n", i, i, refs)
	}
	return []byte(b.String())
}

func TestExternalUI(t *testing.T) {
	home := filepath.Join("var", "core")
	for _, tt := range []struct{ cfg, dir, url string }{
		{"external-ui: ui\nexternal-ui-name: zashboard\nexternal-ui-url: https://x/dist.zip\n", filepath.Join(home, "ui", "zashboard"), "https://x/dist.zip"},
		{"external-ui: ui\n", filepath.Join(home, "ui"), ""},
		{"external-ui: ui\nexternal-ui-name: ../x\n", "", ""},
		{"mixed-port: 1\n", "", ""},
	} {
		f, err := Inspect([]byte(tt.cfg))
		if err != nil {
			t.Fatal(err)
		}
		if dir, url := f.ExternalUI(home); dir != tt.dir || url != tt.url {
			t.Errorf("ExternalUI(%q) = %q, %q; want %q, %q", tt.cfg, dir, url, tt.dir, tt.url)
		}
	}
}
