package config

import (
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Managed 是 tunlr 的一个交互功能生成的受管 mixin：功能（tun on/off、panel lan、update set geo 等）的
// 开关存在状态文件或工具设置中，它在配置中写成什么由这份 mixin 表达，与用户的 mixin 用同一套合并引擎。
// 受管 mixin 只在内存中合成，不写入 mixin.d。
type Managed struct {
	// Name 是名称，如 tunlr:tun，出现在 plan、覆盖记录和冲突提示中。
	Name string `json:"name"`
	// Command 是修改它的命令，如 "tunlr tun on/off"；为空表示是工具运行所需，不能修改。
	Command string `json:"command,omitempty"`
	// Default 为 true 时它只提供默认值：在用户的 mixin 之前应用，用户可以覆盖（覆盖记录显示它的名称）。
	// 否则它独占所写的键：在用户的 mixin 之后应用，用户的 mixin 设置、修改或删除这些键时报错。
	Default bool `json:"default"`
	// Data 是 mixin 的内容。
	Data []byte `json:"-"`
	// Owns 是 Data 之外另行独占的键（点分路径），例如自动更新关闭时不写的 geo-update-interval。
	Owns []string `json:"owns,omitempty"`
}

// Keys 返回独占的键（点分路径）：Data 中的叶子和 Owns，按出现顺序。Default 时为空。
func (m Managed) Keys() []string {
	if m.Default {
		return nil
	}
	var keys []string
	if doc, err := parseNode(m.Data); err == nil && doc != nil {
		for _, p := range leafPaths(doc.Content[0], nil) {
			keys = append(keys, strings.Join(p, "."))
		}
	}
	for _, k := range m.Owns {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// owned 是一个独占的键及其所属的受管 mixin。
type owned struct {
	path []string
	by   *Managed
}

// ownedKeys 列出 managed 中独占的全部键。
func ownedKeys(managed []Managed) []owned {
	var out []owned
	for i := range managed {
		for _, k := range managed[i].Keys() {
			out = append(out, owned{path: strings.Split(k, "."), by: &managed[i]})
		}
	}
	return out
}

// conflicts 检查一个用户 mixin（src，已应用到 root 上；before 是应用前各独占键的值）是否动了独占的键：
// 直接写了它（即使值相同），或经上级的替换、删除改变了它。有冲突时返回 *MixinError，指出第一处的行号，
// 消息中列出全部冲突的键和修改它们的命令。
func conflicts(file string, src, root *yaml.Node, keys []owned, before []*yaml.Node) error {
	var line int
	var parts []string
	byOwner := map[*Managed][]string{}
	var owners []*Managed
	for i, k := range keys {
		if sameNode(before[i], lookup(root, k.path)) && !writes(src, k.path) {
			continue
		}
		if l := pathLine(src, k.path); l > 0 && (line == 0 || l < line) {
			line = l
		}
		if _, ok := byOwner[k.by]; !ok {
			owners = append(owners, k.by)
		}
		byOwner[k.by] = append(byOwner[k.by], strings.Join(k.path, "."))
	}
	if len(owners) == 0 {
		return nil
	}
	remove := "remove it from the mixin"
	if len(owners) > 1 || len(byOwner[owners[0]]) > 1 {
		remove = "remove them from the mixin"
	}
	for _, o := range owners {
		how := "tunlr needs it to work"
		if o.Command != "" {
			how = "change it with: " + o.Command
		}
		verb := "is"
		if len(byOwner[o]) > 1 {
			verb = "are"
		}
		parts = append(parts, fmt.Sprintf("%s %s managed by %s (%s)", strings.Join(byOwner[o], ", "), verb, o.Name, how))
	}
	return &MixinError{File: file, Line: line, Err: fmt.Errorf("%s; %s", strings.Join(parts, "; "), remove)}
}

// writes 报告 mixin 是否写了 path：写了它本身，或把它的某一级上级写成了非映射的值（替换或用 null 删除）。
// 后者在键原本不存在时不改变配置，同样算作动了它。
func writes(src *yaml.Node, path []string) bool {
	m := src
	for _, k := range path {
		if m.Kind != yaml.MappingNode {
			return true
		}
		i := valueIndex(m, k)
		if i < 0 {
			return false
		}
		m = m.Content[i]
	}
	return true
}

// pathLine 返回 mixin 中离 path 最近的、写出来的键所在的行：沿路径能走多深走多深，每一层也认
// 作用于该键的前缀操作（如 delete-tun）。找不到时为 0。
func pathLine(m *yaml.Node, path []string) int {
	line := 0
	for _, k := range path {
		if m == nil || m.Kind != yaml.MappingNode {
			return line
		}
		next := -1
		for i := 0; i+1 < len(m.Content); i += 2 {
			name := m.Content[i].Value
			if name == k {
				next = i
				break
			}
			if _, _, key, ok := parseOp(name); ok && key == k {
				line = m.Content[i].Line
			}
		}
		if next < 0 {
			return line
		}
		line = m.Content[next].Line
		m = m.Content[next+1]
	}
	return line
}
