package config

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// routingKeys 是从订阅中采用的顶层键，即路由部分。订阅的其余键一律丢弃，
// 避免机场配置打开端口、改监听地址或设置 API。
var routingKeys = map[string]bool{
	"proxies":         true,
	"proxy-groups":    true,
	"rules":           true,
	"proxy-providers": true,
	"rule-providers":  true,
	"sub-rules":       true,
}

// maxNodes 限制展开锚点和别名后的 YAML 节点数，防止恶意订阅用别名放大。
const maxNodes = 1_000_000

// Layers 是配置合成的各层输入，均为 YAML 文本。除 Template 外都可以为空。
type Layers struct {
	// Template 是工具内置的基础配置（系统部分）。
	Template []byte
	// Subscription 是机场提供的完整配置，只采用其中的路由部分。
	Subscription []byte
	// Mixin 是用户的覆盖，支持 prepend-<键> 与 append-<键>。
	Mixin []byte
	// Enforced 是工具运行所依赖的键，最后写入，mixin 无法覆盖。
	Enforced []byte
}

// Result 是合成的结果。
type Result struct {
	// Config 是最终的 mihomo 配置。
	Config []byte
	// Dropped 是订阅中被丢弃的顶层键，按出现顺序排列。
	Dropped []string
}

// Compose 按 模板 → 订阅 → mixin → 强制项 的顺序合成最终配置。
// 合并基于 yaml.Node，保留模板的键顺序与注释；模板中没有的键追加在末尾。
func Compose(l Layers) (*Result, error) {
	doc, err := parseDocument("template", l.Template)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, errors.New("template: empty")
	}
	root := doc.Content[0]
	res := &Result{}

	sub, err := parseDocument("subscription", l.Subscription)
	if err != nil {
		return nil, fmt.Errorf("%w (only full mihomo configs are supported as subscriptions)", err)
	}
	if sub != nil {
		m := sub.Content[0]
		for i := 0; i < len(m.Content); i += 2 {
			k, v := m.Content[i].Value, m.Content[i+1]
			if !routingKeys[k] {
				res.Dropped = append(res.Dropped, k)
				continue
			}
			// 用新的键节点，不带入订阅里附在键上的注释。
			setPair(root, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, v)
		}
	}

	mixin, err := parseDocument("mixin", l.Mixin)
	if err != nil {
		return nil, err
	}
	if mixin != nil {
		if err := applyMixin(root, mixin.Content[0]); err != nil {
			return nil, fmt.Errorf("mixin: %w", err)
		}
	}

	enforced, err := parseDocument("enforced", l.Enforced)
	if err != nil {
		return nil, err
	}
	if enforced != nil {
		mergeMapping(root, enforced.Content[0])
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	res.Config = spaceSections(buf.Bytes())
	return res, nil
}

// spaceSections 在顶层段落之间补回空行（yaml.v3 编码时会丢弃键之间的空行）。
// 规则固定，保证输出可预测：顶层键带头部注释，或它自己、前一个键的值是多行块时，前面空一行。
// 编码时缩进为 2 且列表也缩进，所以第 0 列的行只可能是顶层键或顶层注释。
func spaceSections(out []byte) []byte {
	lines := strings.Split(string(out), "\n")
	indented := func(i int) bool {
		return i >= 0 && i < len(lines) && strings.HasPrefix(lines[i], " ")
	}
	var b strings.Builder
	for i, line := range lines {
		if i > 0 && line != "" && !indented(i) {
			prev := lines[i-1]
			comment := strings.HasPrefix(line, "#")
			if prev != "" && !strings.HasPrefix(prev, "#") && (comment || indented(i-1) || indented(i+1)) {
				b.WriteString("\n")
			}
		}
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}

// parseDocument 解析一层输入并展开其中的别名。内容为空时返回 nil；
// 非空时保证返回的文档节点只有一个子节点，且为映射。
func parseDocument(name string, data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || isNull(doc.Content[0]) {
		return nil, nil
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: not a YAML mapping", name)
	}
	budget := maxNodes
	root, err := expandAliases(doc.Content[0], &budget)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	doc.Content[0] = root
	return &doc, nil
}

// expandAliases 返回 n 的深拷贝，其中的别名替换为所引用节点的拷贝，锚点被去除。
// 被引用的锚点可能位于随后被丢弃的键里，不展开会输出悬空的别名。
func expandAliases(n *yaml.Node, budget *int) (*yaml.Node, error) {
	if *budget--; *budget < 0 {
		return nil, fmt.Errorf("more than %d nodes after expanding aliases", maxNodes)
	}
	if n.Kind == yaml.AliasNode {
		return expandAliases(n.Alias, budget)
	}
	c := *n
	c.Anchor = ""
	if n.Content != nil {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			var err error
			if c.Content[i], err = expandAliases(child, budget); err != nil {
				return nil, err
			}
		}
	}
	if c.Kind == yaml.MappingNode {
		if err := inlineMerge(&c); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

// inlineMerge 把映射中的合并键（<<）展开为普通键：显式写出的键优先，多个来源时靠前的优先。
// 合并来的键放在合并键原来的位置。输出因此更易读，也不依赖 mihomo 对合并键的支持。
// 子节点已先行展开，嵌套的合并键在这之前已处理完。
func inlineMerge(m *yaml.Node) error {
	at := -1
	present := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Tag == "!!merge" {
			at = i
		} else {
			present[k.Value] = true
		}
	}
	if at < 0 {
		return nil
	}
	var sources []*yaml.Node
	switch v := m.Content[at+1]; v.Kind {
	case yaml.MappingNode:
		sources = []*yaml.Node{v}
	case yaml.SequenceNode:
		sources = v.Content
	}
	var merged []*yaml.Node
	for _, s := range sources {
		if s.Kind != yaml.MappingNode {
			return fmt.Errorf("line %d: merge key value must be a mapping or a list of mappings", m.Content[at].Line)
		}
		for i := 0; i+1 < len(s.Content); i += 2 {
			if k := s.Content[i]; !present[k.Value] {
				present[k.Value] = true
				merged = append(merged, k, s.Content[i+1])
			}
		}
	}
	m.Content = append(append(append([]*yaml.Node{}, m.Content[:at]...), merged...), m.Content[at+2:]...)
	return nil
}

// applyMixin 先按 mergeMapping 的语义合并普通键，再处理 prepend-<键> 与 append-<键>。
func applyMixin(root, mixin *yaml.Node) error {
	type listOp struct {
		key     string
		prepend bool
		items   *yaml.Node
	}
	var ops []listOp
	plain := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i < len(mixin.Content); i += 2 {
		k, v := mixin.Content[i], mixin.Content[i+1]
		if key, ok := strings.CutPrefix(k.Value, "prepend-"); ok {
			ops = append(ops, listOp{key, true, v})
		} else if key, ok := strings.CutPrefix(k.Value, "append-"); ok {
			ops = append(ops, listOp{key, false, v})
		} else {
			plain.Content = append(plain.Content, k, v)
		}
	}
	mergeMapping(root, plain)

	for _, op := range ops {
		name := "append-" + op.key
		if op.prepend {
			name = "prepend-" + op.key
		}
		if isNull(op.items) {
			continue
		}
		if op.items.Kind != yaml.SequenceNode {
			return fmt.Errorf("%s: value must be a list", name)
		}
		i := valueIndex(root, op.key)
		if i < 0 {
			setPair(root, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: op.key},
				&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
			i = valueIndex(root, op.key)
		}
		target := root.Content[i]
		if target.Kind != yaml.SequenceNode {
			return fmt.Errorf("%s: %q is not a list", name, op.key)
		}
		if op.prepend {
			target.Content = append(append([]*yaml.Node{}, op.items.Content...), target.Content...)
		} else {
			target.Content = append(target.Content, op.items.Content...)
		}
	}
	return nil
}

// mergeMapping 把 src 合并进 dst：两边都是映射时逐层合并，否则用 src 的值整体替换。
func mergeMapping(dst, src *yaml.Node) {
	for i := 0; i < len(src.Content); i += 2 {
		k, v := src.Content[i], src.Content[i+1]
		if j := valueIndex(dst, k.Value); j >= 0 && dst.Content[j].Kind == yaml.MappingNode && v.Kind == yaml.MappingNode {
			mergeMapping(dst.Content[j], v)
			continue
		}
		setPair(dst, k, v)
	}
}

// setPair 设置映射中 key 的值。键已存在时只替换值，保留原键节点及其注释；否则追加到末尾。
func setPair(m, key, value *yaml.Node) {
	if i := valueIndex(m, key.Value); i >= 0 {
		m.Content[i] = value
		return
	}
	m.Content = append(m.Content, key, value)
}

// valueIndex 返回映射中 key 对应的值在 Content 中的下标，不存在时返回 -1。
func valueIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i + 1
		}
	}
	return -1
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}
