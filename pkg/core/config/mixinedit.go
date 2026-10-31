package config

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// SetMixinKeys 在 mixin 内容 data 的 mihomo: 中设置点分路径的键（如 geox-url.geosite），值为 nil 时删除它，
// 删空的上级随之删除；其余内容和注释保留。data 中没有内容时新建，以 head 为开头的注释（head 为空时不加）。
// 结果中什么都没有时返回 nil，调用方可以删除文件。
func SetMixinKeys(data []byte, head string, values map[string]any) ([]byte, error) {
	return setMixinValues(data, head, mihomoKey, values)
}

// SetTunlrKeys 同 SetMixinKeys，设置的是 tunlr: 中的键（如 ignore-from-subscription）。
func SetTunlrKeys(data []byte, head string, values map[string]any) ([]byte, error) {
	return setMixinValues(data, head, namespace, values)
}

// setMixinValues 在 mixin 内容 data 的命名空间 ns（mihomo 或 tunlr）中设置点分路径的键，见 SetMixinKeys。
func setMixinValues(data []byte, head, ns string, values map[string]any) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 || len(doc.Content) == 0 || isNull(doc.Content[0]) {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: head}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, ErrNotMapping
	}
	for _, k := range slices.Sorted(maps.Keys(values)) {
		var v *yaml.Node
		if values[k] != nil {
			v = &yaml.Node{}
			if err := v.Encode(values[k]); err != nil {
				return nil, err
			}
		}
		if err := setKey(root, append([]string{ns}, strings.Split(k, ".")...), v); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
	}
	if len(root.Content) == 0 {
		return nil, nil
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// setKey 把映射 m 中 path 处的值设为 v，v 为 nil 时删除它；删空的上级映射随之删除。
func setKey(m *yaml.Node, path []string, v *yaml.Node) error {
	m.Style = 0
	j := valueIndex(m, path[0])
	if len(path) == 1 {
		switch {
		case v == nil && j >= 0:
			m.Content = slices.Delete(m.Content, j-1, j+1)
		case v != nil:
			setValue(m, path[0], v)
		}
		return nil
	}
	if j < 0 || isNull(m.Content[j]) {
		if v == nil {
			return nil
		}
		setValue(m, path[0], &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
		j = valueIndex(m, path[0])
	}
	child := m.Content[j]
	if child.Kind != yaml.MappingNode {
		return fmt.Errorf("%s is not a mapping", path[0])
	}
	if err := setKey(child, path[1:], v); err != nil {
		return err
	}
	if len(child.Content) == 0 {
		m.Content = slices.Delete(m.Content, j-1, j+1)
	}
	return nil
}
