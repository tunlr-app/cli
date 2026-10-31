package config

import (
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ignoreKey 是 tunlr: 下列出不从订阅采用的顶层键的键。
const ignoreKey = "ignore-from-subscription"

// IgnorableKeys 是 tunlr.ignore-from-subscription 可以列出的键：订阅整段采用、mihomo: 的合并又退不回兜底值的映射。
// 列出后订阅的这个键被丢弃，用模板和兜底值（tunlr:defaults、tunlr.defaults）中的。
var IgnorableKeys = []string{"dns", "sniffer", "hosts"}

// IgnoredKey 是订阅中写了、因为 mixin 的 tunlr.ignore-from-subscription 没有采用的顶层键。
type IgnoredKey struct {
	Key string `json:"key"`
	// File 是列出它的 mixin 文件，有几个时为最先应用的。
	File string `json:"file"`
}

// ignoredKeys 检查 tunlr.ignore-from-subscription 的值并返回其中的键：null，或 IgnorableKeys 中的键的列表。
func (m *mixer) ignoredKeys(v *yaml.Node) ([]string, error) {
	if isNull(v) {
		return nil, nil
	}
	if v.Kind != yaml.SequenceNode {
		return nil, m.errorf(v.Line, "%s.%s must be a list of keys (%s)", namespace, ignoreKey, strings.Join(IgnorableKeys, ", "))
	}
	var keys []string
	for _, it := range v.Content {
		if it.Kind != yaml.ScalarNode || !slices.Contains(IgnorableKeys, it.Value) {
			return nil, m.errorf(it.Line, "%s.%s: %s cannot be ignored; only %s can", namespace, ignoreKey, it.Value, strings.Join(IgnorableKeys, ", "))
		}
		keys = append(keys, it.Value)
	}
	return keys, nil
}

// takeIgnored 把 mixin src（已经检查过）的 tunlr.ignore-from-subscription 记入 m.ignored（键 → 最先列出它的文件）。
// 各文件的列表合在一起：后面的文件不能取消前面的。
func (m *mixer) takeIgnored(src *yaml.Node) {
	v := lookup(src, []string{namespace, ignoreKey})
	if v == nil {
		return
	}
	keys, _ := m.ignoredKeys(v)
	for _, k := range keys {
		if m.ignored[k] == "" {
			m.ignored[k] = m.file
		}
	}
}

// MixinIgnores 返回 mixin 内容 data 的 tunlr.ignore-from-subscription 中的键；内容有错时返回 *MixinError（文件名为 name）。
func MixinIgnores(name string, data []byte) ([]string, error) {
	doc, err := parseNode(data)
	if err != nil {
		return nil, &MixinError{File: name, Err: err}
	}
	if doc == nil {
		return nil, nil
	}
	m := &mixer{file: name}
	v := lookup(doc.Content[0], []string{namespace, ignoreKey})
	if v == nil {
		return nil, nil
	}
	return m.ignoredKeys(v)
}
