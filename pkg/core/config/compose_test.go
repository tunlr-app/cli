package config

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/txtar"

	"github.com/tunlr-app/cli/templates"
)

var update = flag.Bool("update", false, "rewrite the want sections of golden files")

// TestComposeGolden 读取 testdata/compose/*.txtar。每个文件包含输入
// template.yaml（缺省时使用内置模板）、defaults.yaml、subscription.yaml、mixin.yaml、enforced.yaml，
// 以及期望输出 want.yaml 和 dropped（被丢弃的订阅键，每行一个）。
// 用 go test ./pkg/core/config -update 重新生成期望输出。
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
			for _, f := range ar.Files {
				sections[f.Name] = f.Data
			}
			l := Layers{
				Template:     sections["template.yaml"],
				Defaults:     sections["defaults.yaml"],
				Subscription: sections["subscription.yaml"],
				Mixin:        sections["mixin.yaml"],
				Enforced:     sections["enforced.yaml"],
			}
			if _, ok := sections["template.yaml"]; !ok {
				l.Template = templates.Config
			}
			res, err := Compose(l)
			if err != nil {
				t.Fatal(err)
			}
			var dropped []byte
			if len(res.Dropped) > 0 {
				dropped = []byte(strings.Join(res.Dropped, "\n") + "\n")
			}

			if *update {
				setSection(ar, "want.yaml", res.Config)
				setSection(ar, "dropped", dropped)
				if err := os.WriteFile(file, txtar.Format(ar), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			if want := sections["want.yaml"]; !bytes.Equal(res.Config, want) {
				t.Errorf("config mismatch (run with -update to accept)\n--- got ---\n%s\n--- want ---\n%s", res.Config, want)
			}
			if want := sections["dropped"]; !bytes.Equal(dropped, want) {
				t.Errorf("dropped = %q, want %q", dropped, want)
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
			name: "invalid yaml",
			l:    Layers{Template: []byte("a: 1\n"), Mixin: []byte("a: [\n")},
			want: "mixin: yaml:",
		},
		{
			name: "prepend to non-list",
			l:    Layers{Template: []byte("dns:\n  enable: true\n"), Mixin: []byte("prepend-dns:\n  - x\n")},
			want: `mixin: prepend-dns: "dns" is not a list`,
		},
		{
			name: "append non-list value",
			l:    Layers{Template: []byte("rules: []\n"), Mixin: []byte("append-rules: MATCH,DIRECT\n")},
			want: "mixin: append-rules: value must be a list",
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
