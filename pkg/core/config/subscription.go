package config

import (
	"errors"
	"fmt"

	"go.yaml.in/yaml/v3"
)

// SubscriptionSummary 概括订阅内容中的路由部分。
type SubscriptionSummary struct {
	Proxies        int `json:"proxies"`
	ProxyProviders int `json:"proxy_providers"`
	ProxyGroups    int `json:"proxy_groups"`
	Rules          int `json:"rules"`
}

// InspectSubscription 检查订阅内容是否是可用的完整配置，并统计其路由部分。
// 内容必须是 YAML 映射，且至少带有节点（proxies 或 proxy-providers）。
func InspectSubscription(data []byte) (SubscriptionSummary, error) {
	var sum SubscriptionSummary
	doc, err := parseDocument("subscription", data)
	if errors.Is(err, errNotMapping) {
		// 最常见的原因是机场没有识别出 User-Agent，返回了 base64 编码的节点列表。
		return sum, fmt.Errorf("%w; the provider may have returned a plain node list, try a different --user-agent", err)
	}
	if err != nil {
		return sum, err
	}
	if doc == nil {
		return sum, errors.New("subscription: empty")
	}
	m := doc.Content[0]
	sum = SubscriptionSummary{
		Proxies:        countItems(m, "proxies"),
		ProxyProviders: countItems(m, "proxy-providers"),
		ProxyGroups:    countItems(m, "proxy-groups"),
		Rules:          countItems(m, "rules"),
	}
	if sum.Proxies == 0 && sum.ProxyProviders == 0 {
		return sum, errors.New("subscription: no proxies or proxy-providers")
	}
	return sum, nil
}

// countItems 返回映射中 key 对应的列表元素数，或映射的键数。
func countItems(m *yaml.Node, key string) int {
	i := valueIndex(m, key)
	if i < 0 {
		return 0
	}
	switch v := m.Content[i]; v.Kind {
	case yaml.SequenceNode:
		return len(v.Content)
	case yaml.MappingNode:
		return len(v.Content) / 2
	default:
		return 0
	}
}
