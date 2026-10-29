package config

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/txtar"
	"go.yaml.in/yaml/v3"

	"github.com/tunlr-app/cli/templates"
)

// TestEncodeMatchesWhole 确认分段编码与一次编码整个文档的输出逐字相同。
func TestEncodeMatchesWhole(t *testing.T) {
	inputs := map[string][]byte{"template": templates.Config}
	files, _ := filepath.Glob(filepath.Join("testdata", "compose", "*.txtar"))
	for _, file := range files {
		ar, err := txtar.ParseFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range ar.Files {
			if strings.HasSuffix(f.Name, ".yaml") {
				inputs[filepath.Base(file)+"/"+f.Name] = f.Data
			}
		}
	}
	inputs["large"] = largeConfig(2*chunkNodes+7, nil)
	// 注释落在分段处附近、列表自身和键上、文档开头与末尾。
	inputs["comments at cuts"] = largeConfig(3*chunkNodes, map[int]string{
		chunkNodes - 1: "# before the first cut",
		chunkNodes:     "# at the first cut",
		2 * chunkNodes: "# at the second cut",
	})
	inputs["comment on list"] = []byte("# head\n\nrules: # line\n" + items(2*chunkNodes+1) + "# foot\n")
	inputs["last key foot"] = []byte("a: 1\nrules:\n" + items(2*chunkNodes+1) + "\n# document foot\n")
	inputs["first key long list"] = []byte("# document head\n\n# key head\nrules:\n" + items(3*chunkNodes) + "b: 2\n")
	inputs["quoted key"] = []byte("\"x y\":\n" + items(2*chunkNodes+1) + "on:\n" + items(2*chunkNodes+1))
	inputs["flow list"] = []byte("rules: [" + strings.Repeat("a, ", 2*chunkNodes) + "b]\n")
	inputs["nested items"] = []byte("proxy-groups:\n" + strings.Repeat("  - name: g\n    proxies:\n      - a\n      - b\n", 2*chunkNodes+3))
	inputs["big items"] = []byte("proxy-groups:\n" + strings.Repeat("  - name: g\n    type: select\n    proxies:\n"+strings.Replace(items(chunkNodes+10), "  -", "      -", -1)+"    url: http://x\n", 3))
	inputs["block scalars"] = []byte("rules:\n" + strings.Repeat("  - |\n    line one\n\n    line two\n", 2*chunkNodes+3))

	for name, data := range inputs {
		t.Run(name, func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
				t.Skip("empty")
			}
			var whole bytes.Buffer
			if err := encodeNode(&whole, &doc); err != nil {
				t.Fatal(err)
			}
			got, err := encodeChunks(&doc)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, whole.Bytes()) {
				diff, _ := Diff("whole", "chunked", whole.Bytes(), got)
				t.Errorf("chunked output differs\n%s", diff)
			}
		})
	}
}

// TestEncodeAstral 确认 emoji 原样输出（不写成 \U 转义），引号的写法保持原样，节点内容不变，
// 文档中原有的私用区字符不被当作占位换掉。
func TestEncodeAstral(t *testing.T) {
	in := "# 🚀 head\n" +
		"proxies:\n" +
		"  - {name: 🇭🇰 香港 01, type: ss}\n" +
		"  - {name: '🇯🇵|日本 02', type: ss}\n" +
		"  - {name: \"🇺🇸 美国\\t03\", type: ss}\n" +
		"  - {name:  私用区 🇭🇰, type: ss} # 🎉\n" +
		"rules:\n" +
		"  - 'DOMAIN,a.example,🚀 节点选择'\n" +
		"  - MATCH,🐟 漏网之鱼\n"
	want := "# 🚀 head\n" +
		"proxies:\n" +
		"  - {name: 🇭🇰 香港 01, type: ss}\n" +
		"  - {name: '🇯🇵|日本 02', type: ss}\n" +
		"  - {name: \"🇺🇸 美国\\t03\", type: ss}\n" +
		"  - {name:  私用区 🇭🇰, type: ss} # 🎉\n" +
		"rules:\n" +
		"  - 'DOMAIN,a.example,🚀 节点选择'\n" +
		"  - MATCH,🐟 漏网之鱼\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(in), &doc); err != nil {
		t.Fatal(err)
	}
	got, err := encode(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("encode =\n%s\nwant\n%s", got, want)
	}
	var back yaml.Node
	if err := yaml.Unmarshal(got, &back); err != nil {
		t.Fatalf("output does not parse: %v", err)
	}
	if v := back.Content[0].Content[1].Content[3].Content[1].Value; v != " 私用区 🇭🇰" {
		t.Errorf("name after round trip = %q", v)
	}
	if v := doc.Content[0].Content[3].Content[1].Value; v != "MATCH,🐟 漏网之鱼" {
		t.Errorf("node not restored: %q", v)
	}
}

func TestSplitPoints(t *testing.T) {
	key := &yaml.Node{Kind: yaml.ScalarNode, Value: "rules"}
	seq := func(n int) *yaml.Node {
		s := &yaml.Node{Kind: yaml.SequenceNode}
		for range n {
			s.Content = append(s.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "x"})
		}
		return s
	}
	if got := splitPoints(key, seq(chunkNodes)); got != nil {
		t.Errorf("short list split at %v", got)
	}
	s := seq(2*chunkNodes + 1)
	if got, want := fmt.Sprint(splitPoints(key, s)), fmt.Sprint([]int{0, chunkNodes, 2 * chunkNodes}); got != want {
		t.Errorf("split points = %s, want %s", got, want)
	}
	s.Content[chunkNodes].HeadComment = "# x"
	if got, want := fmt.Sprint(splitPoints(key, s)), fmt.Sprint([]int{0, chunkNodes + 2}); got != want {
		t.Errorf("split points with comment = %s, want %s", got, want)
	}
}

// BenchmarkComposeLarge 合成 1 万个节点、1 万条规则的订阅（约 1.3 MB），用来观察大订阅的耗时和分配。
func BenchmarkComposeLarge(b *testing.B) {
	sub := largeConfig(10000, nil)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Compose(Layers{Template: templates.Config, Subscription: sub}); err != nil {
			b.Fatal(err)
		}
	}
}

// largeConfig 返回带 n 个节点、n 条规则的配置；comments 给第 i 条规则加上头部注释。
func largeConfig(n int, comments map[int]string) []byte {
	var b strings.Builder
	b.WriteString("# generated\n\nmixed-port: 7890\nproxies:\n")
	for i := range n {
		fmt.Fprintf(&b, "  - {name: \"n-%d\", type: ss, server: 198.51.100.1, port: %d, password: 'p:%d'}\n", i, 1000+i, i)
	}
	b.WriteString("rules:\n")
	for i := range n {
		if c, ok := comments[i]; ok {
			fmt.Fprintf(&b, "  %s\n", c)
		}
		fmt.Fprintf(&b, "  - DOMAIN-SUFFIX,e%d.com,Proxy\n", i)
	}
	return []byte(b.String())
}

func items(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "  - item-%d\n", i)
	}
	return b.String()
}
