package config

import (
	"bytes"
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
// template.yaml（缺省时使用内置模板）、subscription.yaml、generated.yaml、enforced.yaml
// 和 mixin.d/<文件名>（按出现顺序应用），以及期望输出 want.yaml、dropped（被丢弃的订阅键）、
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
			for _, f := range ar.Files {
				sections[f.Name] = f.Data
				if name, ok := strings.CutPrefix(f.Name, "mixin.d/"); ok {
					mixins = append(mixins, Mixin{Name: name, Data: f.Data})
				}
			}
			l := Layers{
				Template:     sections["template.yaml"],
				Subscription: sections["subscription.yaml"],
				Generated:    sections["generated.yaml"],
				Mixins:       mixins,
				Enforced:     sections["enforced.yaml"],
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
