package config

import (
	"slices"

	"go.yaml.in/yaml/v3"
)

// fakeIPFilterKey 是 tunlr: 下修改 dns.fake-ip-filter 的键：在最终生效的那份列表（订阅的或兜底值的）上追加、删除
// 条目，不必用 mihomo: 整份替换。列表的顺序不影响匹配，所以只有 append 和 delete。
const fakeIPFilterKey = "fake-ip-filter"

// fakeIPFilterPath 是 fake-ip-filter 在配置中的路径。
const fakeIPFilterPath = "dns." + fakeIPFilterKey

var fakeIPFilterOps = []string{"append", "delete"}

// checkFakeIPFilter 检查 tunlr.fake-ip-filter：只有 append、delete，值为字符串的列表。
func (m *mixer) checkFakeIPFilter(v *yaml.Node) error {
	if isNull(v) {
		return nil
	}
	path := namespace + "." + fakeIPFilterKey
	if v.Kind != yaml.MappingNode {
		return m.errorf(v.Line, "%s must be a mapping of operations (append, delete)", path)
	}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, ov := v.Content[i], v.Content[i+1]
		if !slices.Contains(fakeIPFilterOps, k.Value) {
			return m.errorf(k.Line, "%s.%s: unknown operation (known: append, delete)", path, k.Value)
		}
		if isNull(ov) {
			continue
		}
		if ov.Kind != yaml.SequenceNode {
			return m.errorf(k.Line, "%s.%s: value must be a list", path, k.Value)
		}
		for _, it := range ov.Content {
			if it.Kind != yaml.ScalarNode || it.Value == "" {
				return m.errorf(it.Line, "%s.%s: each item must be a domain pattern such as +.lan", path, k.Value)
			}
		}
	}
	return nil
}

// isFakeIPFilter 报告列表操作 o 是否作用于 dns.fake-ip-filter。
func (o pending) isFakeIPFilter() bool {
	return o.path == "dns" && o.key == fakeIPFilterKey
}

// touch 记下当前文件改了 path 中的列表（tunlr.fake-ip-filter 的操作），使 DNSSources 把它算作改过 dns 的文件；
// 之后的文件整份替换这个列表时照常记为覆盖。追加、删除不算覆盖之前的文件。
func (m *mixer) touch(path string) {
	if m.setBy == nil {
		return
	}
	m.setBy[path] = m.file
	if m.fallback != nil {
		delete(m.fallback, path)
	}
}

// MixinFakeIPFilter 返回 mixin 内容 data 的 tunlr.fake-ip-filter 中追加和删除的条目；内容有错时返回 *MixinError
// （文件名为 name）。
func MixinFakeIPFilter(name string, data []byte) (appended, deleted []string, err error) {
	doc, err := parseNode(data)
	if err != nil {
		return nil, nil, &MixinError{File: name, Err: err}
	}
	if doc == nil {
		return nil, nil, nil
	}
	m := &mixer{file: name}
	v := lookup(doc.Content[0], []string{namespace, fakeIPFilterKey})
	if v == nil {
		return nil, nil, nil
	}
	if err := m.checkFakeIPFilter(v); err != nil || isNull(v) {
		return nil, nil, err
	}
	values := func(op string) []string {
		var out []string
		if l := lookup(v, []string{op}); l != nil && l.Kind == yaml.SequenceNode {
			for _, it := range l.Content {
				out = append(out, it.Value)
			}
		}
		return out
	}
	return values("append"), values("delete"), nil
}

// FakeIPFilter 读出 tunlr 生成的配置 cfg 中的 dns.fake-ip-filter，没有写或读不出时为空。
func FakeIPFilter(cfg []byte) []string {
	return dnsSection(cfg).FakeIPFilter
}
