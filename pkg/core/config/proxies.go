package config

import (
	"reflect"
	"slices"
)

// ProxyChanges 汇总两份配置之间顶层 proxies 的变化，按节点名对应。机场的订阅每次抓取都可能不同
// （实测有机场每次请求随机分配部分节点的端口，流量信息写在节点名里），逐行 diff 中这类变化很多，
// 汇总让用户分清哪些只是订阅本身的变动。只列出字段名，不含字段的值（可能是密码）。
type ProxyChanges struct {
	// Added、Removed 是新增、删除的节点名（改名的不在其中）。
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	// Renamed 是除名称外内容相同的节点，例如机场写着剩余流量的信息节点。
	Renamed []Rename `json:"renamed,omitempty"`
	// Changed 是名称相同、内容变化的节点数；Fields 是变化的字段及涉及的节点数。
	Changed int            `json:"changed,omitempty"`
	Fields  map[string]int `json:"fields,omitempty"`
}

// Rename 是一个改名的节点。
type Rename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Empty 报告节点是否没有变化。
func (c *ProxyChanges) Empty() bool {
	return len(c.Added) == 0 && len(c.Removed) == 0 && len(c.Renamed) == 0 && c.Changed == 0
}

// DiffProxies 比较 old 与 new 的顶层 proxies。任一份为 nil（无法解析）或其 proxies 不是由映射
// 组成的列表时返回 nil。
//
// 删除与新增的节点只在除名称外内容完全相同时配对为改名，不按位置等线索猜测：名称和其他字段同时
// 变化的（如端口也随机的信息节点）照常列为一增一删。
func DiffProxies(old, new *Facts) *ProxyChanges {
	if old == nil || new == nil || !old.proxiesOK || !new.proxiesOK {
		return nil
	}
	a, b := old.proxies, new.proxies
	c := &ProxyChanges{}
	var removed, added []string
	for _, name := range a.names {
		p, ok := b.items[name]
		if !ok {
			removed = append(removed, name)
			continue
		}
		fields := changedFields(a.items[name], p)
		if len(fields) == 0 {
			continue
		}
		c.Changed++
		if c.Fields == nil {
			c.Fields = map[string]int{}
		}
		for _, f := range fields {
			c.Fields[f]++
		}
	}
	for _, name := range b.names {
		if _, ok := a.items[name]; !ok {
			added = append(added, name)
		}
	}
	for _, from := range removed {
		i := slices.IndexFunc(added, func(to string) bool {
			return slices.Equal(changedFields(a.items[from], b.items[to]), []string{"name"})
		})
		if i < 0 {
			c.Removed = append(c.Removed, from)
			continue
		}
		c.Renamed = append(c.Renamed, Rename{From: from, To: added[i]})
		added = slices.Delete(added, i, i+1)
	}
	c.Added = added
	return c
}

type namedProxies struct {
	names []string
	items map[string]map[string]any
}

// changedFields 返回 a 与 b 中值不同的字段名，按名称排序。
func changedFields(a, b map[string]any) []string {
	var fields []string
	for k, va := range a {
		if vb, ok := b[k]; !ok || !reflect.DeepEqual(va, vb) {
			fields = append(fields, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			fields = append(fields, k)
		}
	}
	slices.Sort(fields)
	return fields
}
