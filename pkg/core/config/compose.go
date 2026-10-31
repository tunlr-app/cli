package config

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// subscriptionKeys 是从订阅中采用的顶层键：路由，以及和规则配套的解析与数据（DNS、hosts、嗅探、
// geo 数据的来源和自动更新）。其余的键（端口、监听地址、API、TUN、运行参数）一律丢弃，避免机场配置
// 打开端口、改监听地址或设置 API；内核新增的键在评估之前也丢弃，用户在 config plan 中看得到。
// 采用的键整个替换模板和 tunlr.defaults 中的同名键，geox-url 例外：订阅没有写的文件用兜底的地址。
// dns.listen 不采用：它决定 DNS 是否暴露在外，订阅里的一律丢弃，由受管 mixin 提供（见 Compose）。
var subscriptionKeys = map[string]bool{
	"proxies":             true,
	"proxy-groups":        true,
	"rules":               true,
	"proxy-providers":     true,
	"rule-providers":      true,
	"sub-rules":           true,
	"dns":                 true,
	"hosts":               true,
	"sniffer":             true,
	"geox-url":            true,
	"geodata-mode":        true,
	"geodata-loader":      true,
	"geo-auto-update":     true,
	"geo-update-interval": true,
}

// maxNodes 限制展开锚点和别名后的 YAML 节点数，防止恶意订阅用别名放大。
const maxNodes = 1_000_000

// ErrNotMapping 表示 YAML 内容不是映射，例如订阅返回了节点列表。
var ErrNotMapping = errors.New("not a YAML mapping")

// Layers 是配置合成的各层输入，均为 YAML 文本。除 Template 外都可以为空。
type Layers struct {
	// Template 是工具内置的基础配置（系统部分）。
	Template []byte
	// Subscription 是机场提供的完整配置，只采用 subscriptionKeys 列出的部分。
	Subscription []byte
	// Managed 是 tunlr 的交互功能生成的受管 mixin，语义见 Managed：提供默认值的在用户的 mixin 之前应用，
	// 独占键的在之后应用。
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
	// Ignored 是订阅中写了、mixin 要求不采用的顶层键（tunlr.ignore-from-subscription），按出现顺序排列。
	Ignored []IgnoredKey
	// Warnings 是不阻止发布的问题，例如 patch 没有匹配的项。
	Warnings []Warning
	// Overrides 是后面的 mixin 文件覆盖前面文件所设置的键。
	Overrides []Override
	// Rewrites 是受管 mixin 在合成的最后改写的值（见 Managed.RewriteGeoxURL），按出现顺序排列。
	Rewrites []Rewrite
	// ProxyFilter 是节点筛选（tunlr.proxy-filter）的结果，没有设置或没有订阅时为 nil。
	ProxyFilter *ProxyFilterResult
	// Facts 是从 Config 中提取的信息，与 Inspect(Config) 的结果相同。
	Facts *Facts
	// GeoSources 是 GeoKeys 中各键的值和来源。
	GeoSources []KeySource
	// DNSSources 是 DNSKeys 中各键的来源。
	DNSSources []SectionSource
}

// Compose 按 模板 → mixin 中的 tunlr.defaults → 订阅 → 提供默认值的受管 mixin → 用户的 mixin → 独占键的受管 mixin
// → 受管 mixin 的改写 的顺序合成最终配置。合并基于 yaml.Node，保留模板的键顺序与注释；模板中没有的键追加在末尾。
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

	// 受管 mixin 是工具自己生成的，内容有错是程序的错误，不当作用户 mixin 的错误报告。
	var defaults, exclusive []Mixin
	managed := map[string]bool{}
	for _, mg := range l.Managed {
		if _, err := parseDocument(mg.Name, mg.Data); err != nil {
			return nil, err
		}
		managed[mg.Name] = true
		if mg.Default {
			defaults = append(defaults, Mixin{Name: mg.Name, Data: mg.Data})
		} else {
			exclusive = append(exclusive, Mixin{Name: mg.Name, Data: mg.Data})
		}
	}
	keys := ownedKeys(l.Managed)

	// 先检查所有 mixin，合并其中的 tunlr.proxy-filter（订阅的节点按合并后的筛选处理，然后才应用 mixin，
	// mixin 自己加的节点不受筛选影响），并把 tunlr.defaults 合并进模板：它是兜底值，订阅采用的同名顶层键
	// 随后整个替换它。
	m := &mixer{root: root, subKeys: map[string]bool{}, setBy: map[string]string{}, fallback: map[string]bool{}, filled: map[string]bool{},
		ignored: map[string]string{}}
	user := append(defaults, l.Mixins...)
	order := make([]string, len(user))
	for i, mx := range user {
		order[i] = mx.Name
	}
	srcs := make([]*yaml.Node, len(user))
	ns := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i, mx := range user {
		doc, err := parseNode(mx.Data)
		if err != nil {
			return nil, &MixinError{File: mx.Name, Err: err}
		}
		if doc == nil {
			continue
		}
		srcs[i] = doc.Content[0]
		stripComments(srcs[i])
		m.file = mx.Name
		if err := m.takeFilter(ns, srcs[i]); err != nil {
			return nil, err
		}
		m.takeIgnored(srcs[i])
		if err := m.applyDefaults(root, srcs[i]); err != nil {
			return nil, err
		}
	}

	sub, err := parseDocument("subscription", l.Subscription)
	if err != nil {
		return nil, fmt.Errorf("%w (only full mihomo configs are supported as subscriptions)", err)
	}
	if sub != nil {
		s := sub.Content[0]
		for i := 0; i < len(s.Content); i += 2 {
			k, v := s.Content[i].Value, s.Content[i+1]
			if !subscriptionKeys[k] {
				res.Dropped = append(res.Dropped, k)
				continue
			}
			// mixin 要求不采用的键（tunlr.ignore-from-subscription）留给兜底值。
			if f := m.ignored[k]; f != "" {
				res.Ignored = append(res.Ignored, IgnoredKey{Key: k, File: f})
				continue
			}
			// 订阅的 DNS 整段替换模板的，但不采用监听地址：机场常写 0.0.0.0:53，在公网服务器上
			// 就是开放的 DNS 解析器（见 CheckExposure）。监听地址由受管 mixin 提供。
			if k == "dns" && v.Kind == yaml.MappingNode {
				var dropped bool
				if v, dropped = withoutKey(v, "listen"); dropped {
					res.Dropped = append(res.Dropped, "dns.listen")
				}
			}
			// geox-url 的各文件互不相干：订阅没有写的文件仍用兜底的地址，而不是内核内置的（不经镜像）。
			if k == "geox-url" {
				v = m.fillFiles(lookup(root, []string{k}), v, k)
			}
			// 用新的键节点，不带入订阅里附在键上的注释。
			setValue(root, k, v)
			m.subKeys[k] = true
		}
		// 被订阅替换掉的兜底值不再有来源，补全进去的除外。
		for p := range m.setBy {
			if top, _, _ := strings.Cut(p, "."); m.subKeys[top] && !m.filled[p] {
				delete(m.setBy, p)
				delete(m.fallback, p)
			}
		}
	}
	bases := m.sectionBases(order)

	var removedGroups []string
	if pf := proxyFilterOf(ns); pf != nil && sub != nil {
		if res.ProxyFilter, err = m.filterProxies(root, pf); err != nil {
			return nil, err
		}
		removedGroups = res.ProxyFilter.Groups
	}
	filterWarnings := m.warnings
	m.warnings = nil

	for i, mx := range user {
		src := srcs[i]
		if src == nil {
			continue
		}
		before := snapshot(root, keys)
		m.file = mx.Name
		first := len(m.warnings)
		if err := m.applyFile(root, src); err != nil {
			return nil, err
		}
		if err := conflicts(mx.Name, mihomoOf(src), root, keys, before); err != nil {
			return nil, err
		}
		slices.SortStableFunc(m.warnings[first:], func(a, b Warning) int { return a.Line - b.Line })
	}
	if len(removedGroups) > 0 {
		if err := m.checkRemovedGroups(root, removedGroups); err != nil {
			return nil, err
		}
	}
	res.Warnings = slices.Concat(orderWarnings(l.Mixins), filterWarnings, m.warnings)
	res.Overrides = m.overrides

	for _, mx := range exclusive {
		if doc, _ := parseNode(mx.Data); doc != nil {
			mergeMapping(root, mihomoOf(doc.Content[0]))
		}
	}
	res.GeoSources = m.geoSources()
	res.DNSSources = m.sectionSources(bases, order, managed)
	for _, mg := range l.Managed {
		if mg.RewriteGeoxURL != nil {
			for _, k := range rewriteGeoxURL(root, mg.RewriteGeoxURL) {
				res.Rewrites = append(res.Rewrites, Rewrite{Path: "geox-url." + k, By: mg.Name})
			}
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

// fillFiles 返回订阅的映射 sub 补上兜底的映射 prev 中有、sub 中没有的键之后的拷贝（不改动 sub），
// 补上的键记在 m.filled 中（path 是映射的路径）。两者有一个不是映射时返回 sub。
func (m *mixer) fillFiles(prev, sub *yaml.Node, path string) *yaml.Node {
	if prev == nil || prev.Kind != yaml.MappingNode || sub.Kind != yaml.MappingNode {
		return sub
	}
	c := *sub
	c.Content = slices.Clone(sub.Content)
	for i := 0; i+1 < len(prev.Content); i += 2 {
		k := prev.Content[i].Value
		if valueIndex(sub, k) >= 0 {
			continue
		}
		c.Content = append(c.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, clone(prev.Content[i+1]))
		m.filled[path+"."+k] = true
	}
	return &c
}

// withoutKey 返回去掉了 key 的映射 m 的拷贝（不改动 m），dropped 报告 m 原来是否有 key。
func withoutKey(m *yaml.Node, key string) (out *yaml.Node, dropped bool) {
	c := *m
	c.Content = make([]*yaml.Node, 0, len(m.Content))
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			dropped = true
			continue
		}
		c.Content = append(c.Content, m.Content[i], m.Content[i+1])
	}
	return &c, dropped
}

// rewriteGeoxURL 把 geox-url 中 https://github.com/ 开头的地址换成 rewrite 的返回值，不论它来自模板、
// 订阅还是 mixin，返回改写了的键。其他地址原样保留。
func rewriteGeoxURL(root *yaml.Node, rewrite func(string) string) []string {
	m := lookup(root, []string{"geox-url"})
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	var keys []string
	for i := 1; i < len(m.Content); i += 2 {
		if v := m.Content[i]; v.Kind == yaml.ScalarNode && strings.HasPrefix(v.Value, "https://github.com/") {
			m.Content[i] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: rewrite(v.Value)}
			keys = append(keys, m.Content[i-1].Value)
		}
	}
	return keys
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
		return nil, ErrNotMapping
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

// mihomoOf 返回 mixin 中 mihomo: 的映射，没有时返回空映射。
func mihomoOf(src *yaml.Node) *yaml.Node {
	if v := lookup(src, []string{mihomoKey}); v != nil && v.Kind == yaml.MappingNode {
		return v
	}
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
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
