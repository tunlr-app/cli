package config

import "strings"

// DNSKeys 是报告整体来源的顶层键（见 SectionSource）：DNS 和嗅探。tunlr:defaults 为它们提供兜底值，订阅写了的
// 整个替换兜底值，所以生效的是哪一份要看订阅。
var DNSKeys = []string{"dns", "sniffer"}

// SectionSource 是合成的配置中一个顶层映射的来源：它的基础来自哪一层，之后又被哪些 mixin 文件改过。
type SectionSource struct {
	Key string `json:"key"`
	// From 是基础所在的层：subscription（订阅写了它，整个采用）、提供兜底值的 mixin（Fallback 为 true，通常是
	// tunlr:defaults，有几个时为最先应用的）或 template；应用 mixin 之前哪里都没有它时为空。
	From     string `json:"from,omitempty"`
	Fallback bool   `json:"fallback,omitempty"`
	// ChangedBy 是改了其中的键、改动留在合成的配置中的 mixin 文件（包括它们的 tunlr.defaults，不含 tunlr 的
	// 受管 mixin 和 From），按应用的顺序。
	ChangedBy []string `json:"changed_by,omitempty"`
	// Set 报告合成的配置中有没有这个键（mixin 可以用 null 删除它）。
	Set bool `json:"set"`
}

// sectionBases 返回 DNSKeys 中各键在应用 mixin 之前的基础（SectionSource 的 Key、From 和 Fallback）。
// 在采用订阅之后、应用 mixin 之前调用；order 是 mixin 的应用顺序。
func (m *mixer) sectionBases(order []string) []SectionSource {
	out := make([]SectionSource, len(DNSKeys))
	for i, k := range DNSKeys {
		s := SectionSource{Key: k}
		switch {
		case m.subKeys[k]:
			s.From = FromSubscription
		case lookup(m.root, []string{k}) != nil:
			s.From = FromTemplate
			for _, f := range order {
				if m.wrote(f, k, true) {
					s.From, s.Fallback = f, true
					break
				}
			}
		}
		out[i] = s
	}
	return out
}

// sectionSources 在 bases（sectionBases 的结果）上补全 ChangedBy 和 Set，在合成的最后调用。managed 是受管
// mixin 的名称。
func (m *mixer) sectionSources(bases []SectionSource, order []string, managed map[string]bool) []SectionSource {
	for i := range bases {
		s := &bases[i]
		s.Set = lookup(m.root, []string{s.Key}) != nil
		for _, f := range order {
			if f != s.From && !managed[f] && m.wrote(f, s.Key, false) {
				s.ChangedBy = append(s.ChangedBy, f)
			}
		}
	}
	return bases
}

// wrote 报告 file 是不是顶层键 key 或其中某个键最后的设置者；fallback 为 true 时只看兜底值（tunlr.defaults）。
func (m *mixer) wrote(file, key string, fallback bool) bool {
	for p, f := range m.setBy {
		if f == file && (p == key || strings.HasPrefix(p, key+".")) && (!fallback || m.fallback[p]) {
			return true
		}
	}
	return false
}
