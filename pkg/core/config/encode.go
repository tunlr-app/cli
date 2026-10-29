package config

import (
	"bytes"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// chunkNodes 是长列表分段编码时每段大约包含的 YAML 节点数。每个节点对应一两个编码事件，每个事件
// 约占 700 字节，2000 个节点一段时队列约 1～3 MB。单个列表项不再拆分：节点很多的项（如引用上万个
// 节点的策略组）独占一段。
const chunkNodes = 2000

// encode 把文档编码为 YAML 文本（缩进 2），emoji 等基本平面以外的字符原样输出（见 keepAstral）。
func encode(doc *yaml.Node) ([]byte, error) {
	restore, back := keepAstral(doc)
	defer restore()
	out, err := encodeChunks(doc)
	if err != nil {
		return nil, err
	}
	return back(out), nil
}

// encodeChunks 把文档编码为 YAML 文本，输出与一次编码整个文档逐字相同。
//
// yaml.v3 的编码器把处理过的事件留在队列里直到文档结束，一次编码大文档时整份文档的事件同时
// 存活（4 MB 的配置约 200 MB）。这里按顶层键分段，长列表再按约 chunkNodes 个节点分段，每段用
// 新的编码器，同时存活的只有一段的事件。
func encodeChunks(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode || len(root.Content) == 0 {
		if err := encodeNode(&buf, doc); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	var scratch bytes.Buffer
	// piece 返回只含一个键的文档。文档和顶层映射自身的注释只放在整个输出的第一段（head）
	// 或最后一段（foot）。
	piece := func(key, value *yaml.Node, head, foot bool) *yaml.Node {
		m := *root
		m.Content = []*yaml.Node{key, value}
		d := *doc
		d.Content = []*yaml.Node{&m}
		if !head {
			m.HeadComment, m.LineComment, d.HeadComment, d.LineComment = "", "", "", ""
		}
		if !foot {
			m.FootComment, d.FootComment = "", ""
		}
		return &d
	}
	for i := 0; i < len(root.Content); i += 2 {
		first, last := i == 0, i+2 == len(root.Content)
		key, value := root.Content[i], root.Content[i+1]
		cut := splitPoints(key, value)
		if cut == nil {
			if err := encodeNode(&buf, piece(key, value, first, last)); err != nil {
				return nil, err
			}
			continue
		}
		// 后续各段用不带注释的键，编码后去掉键所在的第一行，只留列表项。
		bare := &yaml.Node{Kind: key.Kind, Style: key.Style, Tag: key.Tag, Value: key.Value}
		prefix := key.Value + ":\n"
		start := buf.Len()
		for j, from := range cut {
			to, final := len(value.Content), j+1 == len(cut)
			if !final {
				to = cut[j+1]
			}
			seq := *value
			seq.Content = value.Content[from:to]
			if j == 0 {
				if err := encodeNode(&buf, piece(key, &seq, first, last && final)); err != nil {
					return nil, err
				}
				continue
			}
			scratch.Reset()
			if err := encodeNode(&scratch, piece(bare, &seq, false, last && final)); err != nil {
				return nil, err
			}
			out, ok := bytes.CutPrefix(scratch.Bytes(), []byte(prefix))
			if !ok {
				// 编码器给键加了引号等意料之外的写法：退回整体编码这个键。
				buf.Truncate(start)
				if err := encodeNode(&buf, piece(key, value, first, last)); err != nil {
					return nil, err
				}
				break
			}
			buf.Write(out)
		}
	}
	return buf.Bytes(), nil
}

// splitPoints 返回顶层键 key 的列表值可以分段编码的各段起点（第一段从 0 开始）；
// 不需要或不能分段时返回 nil。只在两侧的项都不带注释处分段，键和列表自身带注释、
// 键需要加引号时不分段，这样各段拼接后与整体编码相同。
func splitPoints(key, value *yaml.Node) []int {
	if value.Kind != yaml.SequenceNode || value.Style&yaml.FlowStyle != 0 || len(value.Content) < 2 {
		return nil
	}
	if key.Kind != yaml.ScalarNode || key.Style != 0 || !plainKey(key.Value) ||
		hasComments(key) || hasComments(value) {
		return nil
	}
	cut := []int{0}
	nodes := 0
	for i, item := range value.Content {
		if i > 0 && nodes >= chunkNodes && !hasComments(value.Content[i-1]) && !hasComments(item) {
			cut = append(cut, i)
			nodes = 0
		}
		nodes += countNodes(item)
	}
	if len(cut) == 1 {
		return nil
	}
	return cut
}

// countNodes 返回以 n 为根的节点数。
func countNodes(n *yaml.Node) int {
	c := 1
	for _, child := range n.Content {
		c += countNodes(child)
	}
	return c
}

// plainKey 报告 s 是否只含小写字母、数字和连字符且以字母开头。顶层键都是这样的形式；其余情况
// 不分段，免得猜测编码器的引号规则。个别这样的键仍可能被加引号（如 on），encode 会发现并退回整体编码。
func plainKey(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func hasComments(n *yaml.Node) bool {
	return n.HeadComment != "" || n.LineComment != "" || n.FootComment != ""
}

// privateUseFirst、privateUseLast 是 keepAstral 用作占位的私用区字符的范围，编码器把它们当作可打印字符原样输出。
const privateUseFirst, privateUseLast = 0xE000, 0xF8FF

// keepAstral 让基本平面以外的字符（emoji、国旗等，UTF-8 中占 4 字节）原样输出。yaml.v3 的编码器
// 把它们当作不可打印字符，整个字符串改用双引号并写成 \U0001F1ED 这样的转义，配置和 diff 都难以阅读。
//
// 编码前把每个这样的字符换成文档中没有出现过的私用区字符（同一个字符用同一个占位），编码器原样输出
// 占位，引号的写法也与只含普通字符时相同；back 把编码结果中的占位换回原来的字符，restore 恢复节点。
// 不同的字符多到私用区放不下时，放不下的照旧由编码器转义。
func keepAstral(doc *yaml.Node) (restore func(), back func([]byte) []byte) {
	var fields []*string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		fields = append(fields, &n.Value, &n.HeadComment, &n.LineComment, &n.FootComment)
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(doc)

	used := map[rune]bool{}
	astral := false
	for _, f := range fields {
		for _, r := range *f {
			switch {
			case r > 0xFFFF:
				astral = true
			case r >= privateUseFirst && r <= privateUseLast:
				used[r] = true
			}
		}
	}
	if !astral {
		return func() {}, func(b []byte) []byte { return b }
	}

	holder := map[rune]rune{} // 原字符 → 占位
	next := rune(privateUseFirst)
	type saved struct {
		field *string
		value string
	}
	var originals []saved
	var pairs []string
	for _, f := range fields {
		if !strings.ContainsFunc(*f, func(r rune) bool { return r > 0xFFFF }) {
			continue
		}
		originals = append(originals, saved{f, *f})
		*f = strings.Map(func(r rune) rune {
			if r <= 0xFFFF {
				return r
			}
			if h, ok := holder[r]; ok {
				return h
			}
			for next <= privateUseLast && used[next] {
				next++
			}
			if next > privateUseLast {
				return r
			}
			holder[r] = next
			pairs = append(pairs, string(next), string(r))
			next++
			return holder[r]
		}, *f)
	}
	restore = func() {
		for _, o := range originals {
			*o.field = o.value
		}
	}
	replacer := strings.NewReplacer(pairs...)
	back = func(b []byte) []byte {
		var out bytes.Buffer
		out.Grow(len(b) + len(b)/8)
		replacer.WriteString(&out, string(b))
		return out.Bytes()
	}
	return restore, back
}

// encodeNode 用新的编码器把 doc 写入 w。
func encodeNode(w *bytes.Buffer, doc *yaml.Node) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return nil
}
