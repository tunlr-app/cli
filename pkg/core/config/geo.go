package config

import (
	"strings"

	"go.yaml.in/yaml/v3"
)

// GeoNeeds 表示一份配置需要哪些 geo 数据。
type GeoNeeds struct {
	// GeoIP：GEOIP 规则、DNS fallback-filter 等。默认使用 MMDB，GeodataMode 时使用 GeoIP.dat。
	GeoIP bool
	// GeoSite：GEOSITE 规则、DNS 中的 geosite: 等。
	GeoSite bool
	// ASN：IP-ASN 规则。
	ASN bool
	// GeodataMode 对应 geodata-mode: true。
	GeodataMode bool
}

// geoIgnoredKeys 是判断时跳过的顶层键：它们只描述 geo 数据从哪里下载，不代表用到了它。
var geoIgnoredKeys = map[string]bool{
	"geox-url":            true,
	"geo-auto-update":     true,
	"geo-update-interval": true,
}

// DetectGeo 判断配置需要哪些 geo 数据。判断按名称宽松匹配（规则、DNS 设置中出现 geoip、
// geosite、ip-asn 即视为需要）：多判只会多准备一个文件，漏判则会让 mihomo -t 现场下载。
// 已经有 Facts 时用其中的 Geo，不必再次解析。
func DetectGeo(cfg []byte) (GeoNeeds, error) {
	f, err := Inspect(cfg)
	if err != nil {
		return GeoNeeds{}, err
	}
	return f.Geo, nil
}

// detectGeo 同 DetectGeo，作用于已展开别名的顶层映射。
func detectGeo(root *yaml.Node) GeoNeeds {
	var needs GeoNeeds
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		switch {
		case geoIgnoredKeys[k.Value]:
		case k.Value == "geodata-mode":
			needs.GeodataMode = v.Kind == yaml.ScalarNode && v.Value == "true"
		default:
			needs.scan(v)
		}
	}
	return needs
}

func (n *GeoNeeds) scan(node *yaml.Node) {
	if node.Kind == yaml.ScalarNode {
		s := strings.ToLower(node.Value)
		n.GeoIP = n.GeoIP || strings.Contains(s, "geoip")
		n.GeoSite = n.GeoSite || strings.Contains(s, "geosite")
		n.ASN = n.ASN || strings.Contains(s, "ip-asn")
		return
	}
	for _, c := range node.Content {
		n.scan(c)
	}
}
