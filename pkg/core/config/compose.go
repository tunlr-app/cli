package config

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

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

var errNotMapping = errors.New("not a YAML mapping")

// Layers 是配置合成的各层输入，均为 YAML 文本。除 Template 外都可以为空。
type Layers struct {
	// Template 是工具内置的基础配置（系统部分）。
	Template []byte
	// Subscription 是机场提供的完整配置，只采用其中的路由部分。
	Subscription []byte
	// Managed 是 tunlr 的交互功能生成的受管 mixin，语义见 Managed：提供默认值的在用户的 mixin
	// 之前应用，独占键的在之后应用。
	Managed []Managed
	// Mixins 是用户的 mixin 文件，按顺序逐个应用，语义见 Mixin。
	Mixins []Mixin
}

// Result 是合成的结果。
type Result struct {
	// Config 是最终的 mihomo 配置。
	Config []byte
	// Dropped 是订阅中被丢弃的顶层键，按出现顺序排列。
	Dropped []string
	// Warnings 是不阻止发布的问题，例如 patch 没有匹配的项。
	Warnings []Warning
	// Overrides 是后面的 mixin 文件覆盖前面文件所设置的键。
	Overrides []Override
	// Facts 是从 Config 中提取的信息，与 Inspect(Config) 的结果相同。
	Facts *Facts
}

// Compose 按 模板 → 订阅 → 提供默认值的受管 mixin → 用户的 mixin → 独占键的受管 mixin 的顺序
// 合成最终配置。合并基于 yaml.Node，保留模板的键顺序与注释；模板中没有的键追加在末尾。
// mixin 有错，或用户的 mixin 动了受管 mixin 独占的键时返回 *MixinError。
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
	m := &mixer{root: root, subKeys: map[string]bool{}, setBy: map[string]string{}}
	if sub != nil {
		s := sub.Content[0]
		for i := 0; i < len(s.Content); i += 2 {
			k, v := s.Content[i].Value, s.Content[i+1]
			if !routingKeys[k] {
				res.Dropped = append(res.Dropped, k)
				continue
			}
			// 用新的键节点，不带入订阅里附在键上的注释。
			setValue(root, k, v)
			m.subKeys[k] = true
		}
	}

	// 受管 mixin 是工具自己生成的，内容有错是程序的错误，不当作用户 mixin 的错误报告。
	var defaults, exclusive []Mixin
	for _, mg := range l.Managed {
		if _, err := parseDocument(mg.Name, mg.Data); err != nil {
			return nil, err
		}
		if mg.Default {
			defaults = append(defaults, Mixin{Name: mg.Name, Data: mg.Data})
		} else {
			exclusive = append(exclusive, Mixin{Name: mg.Name, Data: mg.Data})
		}
	}
	keys := ownedKeys(l.Managed)

	for _, mx := range append(defaults, l.Mixins...) {
		doc, err := parseNode(mx.Data)
		if err != nil {
			return nil, &MixinError{File: mx.Name, Err: err}
		}
		if doc == nil {
			continue
		}
		src := doc.Content[0]
		stripComments(src)
		before := make([]*yaml.Node, len(keys))
		for i, k := range keys {
			if n := lookup(root, k.path); n != nil {
				before[i] = clone(n)
			}
		}
		m.file = mx.Name
		first := len(m.warnings)
		if err := m.apply(root, src, ""); err != nil {
			return nil, err
		}
		if err := conflicts(mx.Name, src, root, keys, before); err != nil {
			return nil, err
		}
		slices.SortStableFunc(m.warnings[first:], func(a, b Warning) int { return a.Line - b.Line })
	}
	res.Warnings = append(orderWarnings(l.Mixins), m.warnings...)
	res.Overrides = m.overrides

	for _, mx := range exclusive {
		if doc, _ := parseNode(mx.Data); doc != nil {
			mergeMapping(root, doc.Content[0])
		}
	}
	res.Facts = factsOf(root)

	out, err := encode(doc)
	if err != nil {
		return nil, err
	}
	res.Config = spaceSections(out)
	return res, nil
}

// Enforce 把 managed 中独占键的受管 mixin 写入已经合成好的配置 cfg（例如回滚到的历史修订），
// 其余内容不变；提供默认值的受管 mixin 被忽略。
func Enforce(cfg []byte, managed []Managed) ([]byte, *Facts, error) {
	res, err := Compose(Layers{Template: cfg, Managed: slices.DeleteFunc(slices.Clone(managed), func(m Managed) bool { return m.Default })})
	if err != nil {
		return nil, nil, err
	}
	return res.Config, res.Facts, nil
}

// spaceSections 在顶层段落之间补回空行（yaml.v3 编码时会丢弃键之间的空行）。
// 规则固定，保证输出可预测：顶层键带头部注释，或它自己、前一个键的值是多行块时，前面空一行。
// 编码时缩进为 2 且列表也缩进，所以第 0 列的行只可能是顶层键或顶层注释。
// 直接在字节上逐行处理，不拆成行的切片，大配置时少一份拷贝。
func spaceSections(out []byte) []byte {
	b := make([]byte, 0, len(out)+256)
	// prev 是上一行，line 是当前行（不含换行符），next 从 rest 中取。
	var prev []byte
	rest := out
	for i := 0; ; i++ {
		line, after, more := bytes.Cut(rest, []byte("\n"))
		if i > 0 && len(line) > 0 && line[0] != ' ' {
			next, _, _ := bytes.Cut(after, []byte("\n"))
			indented := func(l []byte) bool { return len(l) > 0 && l[0] == ' ' }
			// next 在 rest 已是最后一行时为空，与原来越界视为不缩进一致。
			if !more {
				next = nil
			}
			if len(prev) > 0 && prev[0] != '#' && (line[0] == '#' || indented(prev) || indented(next)) {
				b = append(b, '\n')
			}
		}
		b = append(b, line...)
		if !more {
			return b
		}
		b = append(b, '\n')
		prev, rest = line, after
	}
}

// parseDocument 解析一层输入并展开其中的别名。内容为空时返回 nil；
// 非空时保证返回的文档节点只有一个子节点，且为映射。
func parseDocument(name string, data []byte) (*yaml.Node, error) {
	doc, err := parseNode(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return doc, nil
}

// parseNode 同 parseDocument，错误信息不带输入的名称。
func parseNode(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || isNull(doc.Content[0]) {
		return nil, nil
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, errNotMapping
	}
	// 常见的订阅没有别名和合并键，只需原地去掉锚点；有的才整棵复制展开，免得大订阅多占一份内存。
	if dropAnchors(doc.Content[0]) {
		return &doc, nil
	}
	budget := maxNodes
	root, err := expandAliases(doc.Content[0], &budget)
	if err != nil {
		return nil, err
	}
	doc.Content[0] = root
	return &doc, nil
}

// dropAnchors 去掉 n 中所有节点的锚点，报告其中是否没有别名和合并键（即不需要 expandAliases）。
// 有别名时锚点被去掉也无妨：expandAliases 按 Alias 指针找到所引用的节点。
func dropAnchors(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Tag == "!!merge" {
		return false
	}
	n.Anchor = ""
	plain := true
	for _, c := range n.Content {
		plain = dropAnchors(c) && plain
	}
	return plain
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
