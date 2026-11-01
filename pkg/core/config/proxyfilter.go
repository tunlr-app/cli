package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dlclark/regexp2"
	"go.yaml.in/yaml/v3"
)

// namespace 是 mixin 顶层中 tunlr 自己的命名空间，不写入配置。
const namespace = "tunlr"

// proxyFilterKeys 是 tunlr.proxy-filter 的字段，与 mihomo 的 provider 相同。
var proxyFilterKeys = []string{"filter", "exclude-filter", "exclude-type"}

// ProxyFilterResult 是节点筛选（tunlr.proxy-filter）的结果。
type ProxyFilterResult struct {
	// Total 是订阅中直接列出的节点数，Kept 是筛选后留下的。
	Total int `json:"total"`
	Kept  int `json:"kept"`
	// Groups 是因此变空而删掉的策略组。
	Groups []string `json:"removed_groups,omitempty"`
	// Providers 是写入了筛选、由内核筛选的 proxy-providers。
	Providers []string `json:"providers,omitempty"`
}

// compileRegexp 按 mihomo 筛选节点的方式编译正则：regexp2（.NET 语法），选项为空。
// mixin 中所有的正则都用它，同一个字符串在 tunlr 和内核中的结果一致。
func compileRegexp(s string) (*regexp2.Regexp, error) {
	return regexp2.Compile(s, regexp2.None)
}

func matchRegexp(re *regexp2.Regexp, s string) bool {
	ok, _ := re.MatchString(s)
	return ok
}

// tunlrKeys 是 tunlr: 下认识的键。
var tunlrKeys = []string{"defaults", "sub-dns", "fake-ip-filter", "proxy-filter", "proxies", "proxy-groups", "rules", "sub-rules"}

// takeFilter 检查 mixin src 的结构（checkMixin），并把其中的 tunlr.proxy-filter 合并到 ns
// （各文件依次合并的结果，节点筛选在任何 mixin 之前按它执行）。
func (m *mixer) takeFilter(ns, src *yaml.Node) error {
	if err := m.checkMixin(src); err != nil {
		return err
	}
	t := lookup(src, []string{namespace})
	if t == nil || t.Kind != yaml.MappingNode {
		return nil
	}
	i := valueIndex(t, "proxy-filter")
	if i < 0 {
		return nil
	}
	wrapped := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{t.Content[i-1], t.Content[i]}}
	return m.set(ns, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: namespace}, wrapped, "", false)
}

// checkNamespace 检查 tunlr 的内容：只认识的键，值的类型正确，正则有效。
func (m *mixer) checkNamespace(v *yaml.Node) error {
	if isNull(v) {
		return nil
	}
	if v.Kind != yaml.MappingNode {
		return m.errorf(v.Line, "%s must be a mapping", namespace)
	}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, pf := v.Content[i], v.Content[i+1]
		switch {
		case !slices.Contains(tunlrKeys, k.Value):
			return m.errorf(k.Line, "%s.%s: unknown key (known: %s)", namespace, k.Value, strings.Join(tunlrKeys, ", "))
		case k.Value == "defaults":
			if !isNull(pf) && pf.Kind != yaml.MappingNode {
				return m.errorf(pf.Line, "%s.defaults must be a mapping", namespace)
			}
			continue
		case k.Value == subDNSKey:
			if err := m.checkSubDNS(pf); err != nil {
				return err
			}
			continue
		case k.Value == fakeIPFilterKey:
			if err := m.checkFakeIPFilter(pf); err != nil {
				return err
			}
			continue
		case k.Value == "sub-rules":
			if isNull(pf) {
				continue
			}
			if pf.Kind != yaml.MappingNode {
				return m.errorf(pf.Line, "%s.sub-rules must be a mapping of rule set names", namespace)
			}
			for j := 0; j+1 < len(pf.Content); j += 2 {
				if err := m.checkOps(namespace+".sub-rules."+pf.Content[j].Value, pf.Content[j+1]); err != nil {
					return err
				}
			}
			continue
		case k.Value != "proxy-filter":
			if err := m.checkOps(namespace+"."+k.Value, pf); err != nil {
				return err
			}
			continue
		}
		if isNull(pf) {
			continue
		}
		if pf.Kind != yaml.MappingNode {
			return m.errorf(pf.Line, "%s.proxy-filter must be a mapping", namespace)
		}
		for j := 0; j+1 < len(pf.Content); j += 2 {
			fk, fv := pf.Content[j], pf.Content[j+1]
			path := namespace + ".proxy-filter." + fk.Value
			if !slices.Contains(proxyFilterKeys, fk.Value) {
				return m.errorf(fk.Line, "%s: unknown key (known: %s)", path, strings.Join(proxyFilterKeys, ", "))
			}
			if fv.Kind != yaml.ScalarNode {
				return m.errorf(fv.Line, "%s must be a string", path)
			}
			if isNull(fv) || fk.Value == "exclude-type" {
				continue
			}
			for _, p := range splitPatterns(fv.Value) {
				if _, err := compileRegexp(p); err != nil {
					return m.errorf(fv.Line, "%s: invalid regular expression %q: %v", path, p, err)
				}
			}
		}
	}
	return nil
}

// checkOps 检查 tunlr: 下一个列表的操作：只有 prepend、append、patch、delete，值为列表。
func (m *mixer) checkOps(path string, v *yaml.Node) error {
	if isNull(v) {
		return nil
	}
	if v.Kind != yaml.MappingNode {
		return m.errorf(v.Line, "%s must be a mapping of operations (prepend, append, patch, delete)", path)
	}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, ov := v.Content[i], v.Content[i+1]
		if !slices.ContainsFunc(listOps, func(o listOp) bool { return o.key == k.Value }) {
			return m.errorf(k.Line, "%s.%s: unknown operation (known: prepend, append, patch, delete)", path, k.Value)
		}
		if !isNull(ov) && ov.Kind != yaml.SequenceNode {
			return m.errorf(k.Line, "%s.%s: value must be a list", path, k.Value)
		}
	}
	return nil
}

// FilterMixinFile 是 sub add --include/--exclude 在订阅 mixin 目录中写入的文件名。
const FilterMixinFile = "10-filter.yaml"

// CheckPatterns 检查 filter、exclude-filter 写法的正则（多条用反引号分隔）。
func CheckPatterns(s string) error {
	for _, p := range splitPatterns(s) {
		if _, err := compileRegexp(p); err != nil {
			return fmt.Errorf("invalid regular expression %q: %v", p, err)
		}
	}
	return nil
}

// FilterMixin 生成只含节点筛选的订阅 mixin（sub add --include/--exclude），filter、exclude 为空的字段省略。
func FilterMixin(sub, filter, exclude string) []byte {
	pf := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if filter != "" {
		setValue(pf, "filter", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: filter})
	}
	if exclude != "" {
		setValue(pf, "exclude-filter", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: exclude})
	}
	ns := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setValue(ns, "proxy-filter", pf)
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setValue(root, namespace, ns)
	root.HeadComment = "Proxy filter of subscription " + sub + ", applied only while it is active.\n" +
		"filter keeps the proxies whose names match, exclude-filter then removes those that match;\n" +
		"separate several regular expressions with a backtick. Same as mihomo's proxy-providers.\n" +
		"Edit with: tunlr config edit " + sub + "/" + strings.TrimSuffix(FilterMixinFile, mixinExt)
	out, _ := yaml.Marshal(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}})
	return out
}

// splitPatterns 把 filter、exclude-filter 按反引号拆成多条正则，与 mihomo 相同；空字符串表示没有设置。
func splitPatterns(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "`")
}

// proxyFilter 是合并后的 tunlr.proxy-filter。
type proxyFilter struct {
	filter, excludeFilter, excludeType string
}

// proxyFilterOf 从合并后的命名空间中取出节点筛选，没有设置时返回 nil。
func proxyFilterOf(ns *yaml.Node) *proxyFilter {
	pf := lookup(ns, []string{namespace, "proxy-filter"})
	if pf == nil || pf.Kind != yaml.MappingNode {
		return nil
	}
	field := func(k string) string {
		if n := lookup(pf, []string{k}); n != nil && n.Kind == yaml.ScalarNode && !isNull(n) {
			return n.Value
		}
		return ""
	}
	f := &proxyFilter{filter: field("filter"), excludeFilter: field("exclude-filter"), excludeType: field("exclude-type")}
	if *f == (proxyFilter{}) {
		return nil
	}
	return f
}

// filterProxies 按 pf 筛选订阅的节点（DESIGN.md 第 6 节“节点筛选”）：直接列出的节点由 tunlr 去掉，
// 并删掉因此变空的策略组；proxy-providers 写入同样的筛选，由内核执行。返回被删掉的策略组，
// 全部 mixin 应用之后由 checkRemovedGroups 检查规则是否还在引用它们。
func (m *mixer) filterProxies(root *yaml.Node, pf *proxyFilter) (*ProxyFilterResult, error) {
	compile := func(s string) []*regexp2.Regexp {
		var res []*regexp2.Regexp
		for _, p := range splitPatterns(s) {
			re, _ := compileRegexp(p) // checkNamespace 已检查过
			res = append(res, re)
		}
		return res
	}
	filters, excludes := compile(pf.filter), compile(pf.excludeFilter)
	var types []string
	for t := range strings.SplitSeq(pf.excludeType, "|") {
		if t = strings.TrimSpace(t); t != "" {
			types = append(types, t)
		}
	}
	res := &ProxyFilterResult{}
	filterHit, excludeHit := make([]bool, len(filters)), make([]bool, len(excludes))

	var removed map[string]bool
	if j := valueIndex(root, "proxies"); j >= 0 && root.Content[j].Kind == yaml.SequenceNode {
		list := root.Content[j]
		removed = map[string]bool{}
		res.Total = len(list.Content)
		list.Content = slices.DeleteFunc(list.Content, func(n *yaml.Node) bool {
			name, ok := itemName(n)
			if !ok || n.Kind != yaml.MappingNode {
				return false
			}
			keep := len(filters) == 0
			for i, re := range filters {
				if matchRegexp(re, name) {
					filterHit[i], keep = true, true
				}
			}
			// 排除项对所有节点都匹配一遍，只为判断它是不是一个都没匹配到。
			for i, re := range excludes {
				if matchRegexp(re, name) {
					excludeHit[i], keep = true, false
				}
			}
			if keep && len(types) > 0 {
				if t := lookup(n, []string{"type"}); t != nil && slices.ContainsFunc(types, func(s string) bool { return strings.EqualFold(s, t.Value) }) {
					keep = false
				}
			}
			if !keep {
				removed[name] = true
			}
			return !keep
		})
		res.Kept = len(list.Content)
	}

	if j := valueIndex(root, "proxy-providers"); j >= 0 && root.Content[j].Kind == yaml.MappingNode {
		providers := root.Content[j]
		for i := 0; i+1 < len(providers.Content); i += 2 {
			p := providers.Content[i+1]
			if p.Kind != yaml.MappingNode {
				continue
			}
			for _, f := range []struct{ key, value string }{
				{"filter", pf.filter}, {"exclude-filter", pf.excludeFilter}, {"exclude-type", pf.excludeType},
			} {
				if f.value == "" {
					continue
				}
				old := ""
				if n := lookup(p, []string{f.key}); n != nil && n.Kind == yaml.ScalarNode && !isNull(n) {
					old = n.Value
				}
				setValue(p, f.key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: mergeProviderFilter(f.key, old, f.value)})
			}
			res.Providers = append(res.Providers, providers.Content[i].Value)
		}
	}

	// 只有直接列出的节点时，一个都没匹配到的正则多半是写错了；有 provider 时它可能匹配 provider 中的节点。
	if res.Total > 0 && len(res.Providers) == 0 {
		for _, w := range []struct {
			key string
			hit []bool
			s   string
		}{{"filter", filterHit, pf.filter}, {"exclude-filter", excludeHit, pf.excludeFilter}} {
			for i, p := range splitPatterns(w.s) {
				if !w.hit[i] {
					m.warnings = append(m.warnings, Warning{File: m.setBy[namespace+".proxy-filter."+w.key],
						Message: fmt.Sprintf("%s.proxy-filter.%s: no proxy matches %s", namespace, w.key, p)})
				}
			}
		}
		if res.Kept == 0 {
			return nil, &MixinError{File: m.filterFile(), Err: fmt.Errorf("%s.proxy-filter removes all %d proxies of the subscription", namespace, res.Total)}
		}
	}
	if len(removed) == 0 {
		return res, nil
	}
	res.Groups = removeEmptied(root, removed)
	return res, nil
}

// filterFile 返回设置了节点筛选的文件（多个时取 filter 所在的），用于报错。
func (m *mixer) filterFile() string {
	for _, k := range proxyFilterKeys {
		if f := m.setBy[namespace+".proxy-filter."+k]; f != "" {
			return f
		}
	}
	return ""
}

// mergeProviderFilter 把节点筛选合并进 provider 原有的同名字段：排除项任一匹配即去掉，直接拼接；
// filter 要两边都满足，用前瞻组合（regexp2 支持，mihomo 用的就是它）。
func mergeProviderFilter(key, old, add string) string {
	switch {
	case old == "":
		return add
	case key == "exclude-filter":
		return old + "`" + add
	case key == "exclude-type":
		return old + "|" + add
	}
	alt := func(s string) string {
		parts := splitPatterns(s)
		if len(parts) == 1 {
			return parts[0]
		}
		return "(?:" + strings.Join(parts, ")|(?:") + ")"
	}
	return "^(?=.*?(?:" + alt(old) + "))(?=.*?(?:" + alt(add) + "))"
}

// groupSources 是策略组中自己拉取节点的字段：有它们的组即使 proxies 为空也不是空组。
var groupSources = []string{"use", "include-all", "include-all-proxies", "include-all-providers"}

// removeEmptied 把 removed 中的节点从所有策略组中移除，然后反复删掉因此变空的组（原来有成员、现在没有，
// 且不自己拉取节点），直到没有空组。返回被删掉的组，按删除的顺序。
func removeEmptied(root *yaml.Node, removed map[string]bool) []string {
	j := valueIndex(root, "proxy-groups")
	if j < 0 || root.Content[j].Kind != yaml.SequenceNode {
		return nil
	}
	groups := root.Content[j]
	// 原本就是空的组不归这里管（mihomo 自己会报错或兜底）。
	hadMembers := map[*yaml.Node]bool{}
	for _, g := range groups.Content {
		if l := groupProxies(g); l != nil && len(l.Content) > 0 {
			hadMembers[g] = true
		}
	}
	var deleted []string
	for len(removed) > 0 {
		removeFromGroups(root, removed)
		removed = map[string]bool{}
		groups.Content = slices.DeleteFunc(groups.Content, func(g *yaml.Node) bool {
			l := groupProxies(g)
			if !hadMembers[g] || l == nil || len(l.Content) > 0 {
				return false
			}
			for _, k := range groupSources {
				if n := lookup(g, []string{k}); n != nil && !isNull(n) && n.Value != "false" {
					return false
				}
			}
			name, _ := itemName(g)
			removed[name] = true
			deleted = append(deleted, name)
			return true
		})
	}
	return deleted
}

// groupProxies 返回策略组的 proxies 列表，没有时返回 nil。
func groupProxies(g *yaml.Node) *yaml.Node {
	if g.Kind != yaml.MappingNode {
		return nil
	}
	if l := lookup(g, []string{"proxies"}); l != nil && l.Kind == yaml.SequenceNode {
		return l
	}
	return nil
}

// checkRemovedGroups 在全部 mixin 应用之后检查：筛选删掉的组如果没有被 mixin 重新加回来，
// 规则（rules、sub-rules）就不能再引用它，否则这部分流量的去向会悄悄改变。
func (m *mixer) checkRemovedGroups(root *yaml.Node, groups []string) error {
	gone := map[string]bool{}
	for _, g := range groups {
		gone[g] = true
	}
	for _, key := range []string{"proxies", "proxy-groups"} {
		if l := lookup(root, []string{key}); l != nil && l.Kind == yaml.SequenceNode {
			for _, n := range l.Content {
				if name, ok := itemName(n); ok {
					delete(gone, name)
				}
			}
		}
	}
	if len(gone) == 0 {
		return nil
	}
	var bad []string
	check := func(rules *yaml.Node) {
		if rules == nil || rules.Kind != yaml.SequenceNode {
			return
		}
		for _, r := range rules.Content {
			if r.Kind != yaml.ScalarNode {
				continue
			}
			// 目标是第一个字段之后的某一项（逻辑规则的条件里有括号和逗号，取不准位置），逐项比较即可。
			fields := strings.Split(r.Value, ",")
			for _, f := range fields[1:] {
				if gone[strings.TrimSpace(f)] {
					bad = append(bad, r.Value)
					break
				}
			}
		}
	}
	check(lookup(root, []string{"rules"}))
	if sub := lookup(root, []string{"sub-rules"}); sub != nil && sub.Kind == yaml.MappingNode {
		for i := 1; i < len(sub.Content); i += 2 {
			check(sub.Content[i])
		}
	}
	if len(bad) == 0 {
		return nil
	}
	names := make([]string, 0, len(gone))
	for _, g := range groups {
		if gone[g] {
			names = append(names, g)
		}
	}
	more := ""
	if len(bad) > 3 {
		more = fmt.Sprintf(" and %d more", len(bad)-3)
		bad = bad[:3]
	}
	return &MixinError{File: m.filterFile(), Err: fmt.Errorf(
		"%s.proxy-filter leaves no proxies in group %s, but rules still use it: %s%s; loosen the filter, or change those rules in a mixin",
		namespace, strings.Join(names, ", "), strings.Join(bad, "; "), more)}
}
