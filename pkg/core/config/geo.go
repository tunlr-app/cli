package config

import (
	"bytes"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// GeoNeeds 表示一份配置需要哪些 geo 数据，以及它们从哪里下载。
type GeoNeeds struct {
	// GeoIP：GEOIP 规则、DNS fallback-filter 等。默认使用 MMDB，GeodataMode 时使用 GeoIP.dat。
	GeoIP bool
	// GeoSite：GEOSITE 规则、DNS 中的 geosite: 等。
	GeoSite bool
	// ASN：IP-ASN 规则。
	ASN bool
	// GeodataMode 对应 geodata-mode: true。
	GeodataMode bool
	// URLs 是 geox-url 中写明的下载地址，键为 geoip、mmdb、asn、geosite；没有写的用内核的默认地址。
	URLs map[string]string
}

// GeoUpdate 是配置中内核自己更新 geo 数据的设置（geo-auto-update、geo-update-interval）。
type GeoUpdate struct {
	Auto bool
	// Interval 是更新的间隔，配置中没有写时为内核的默认值 24 小时。
	Interval time.Duration
}

// defaultGeoInterval 是内核 geo-update-interval 的默认值。
const defaultGeoInterval = 24 * time.Hour

// geoSettingKeys 是判断需要哪些数据时跳过的顶层键：它们只描述 geo 数据从哪里下载、怎么更新，
// 不代表用到了它。
var geoSettingKeys = map[string]bool{
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

// GeoSettings 读出 tunlr 生成的配置 cfg 中 geo 数据的来源（GeoNeeds.URLs、GeodataMode）和自动更新的
// 设置，不判断需要哪些数据。只解析这几个顶层键所在的几行，不解析整份配置：定时更新每小时都要读它，
// 而大订阅的配置有几 MB。生成的配置中顶层键都在第 0 列，其他行都缩进（见 spaceSections）。
func GeoSettings(cfg []byte) (GeoNeeds, GeoUpdate, error) {
	var part bytes.Buffer
	keep := false
	for line := range bytes.Lines(cfg) {
		if len(line) > 0 && line[0] != ' ' && line[0] != '\n' && line[0] != '#' {
			key, _, _ := bytes.Cut(line, []byte(":"))
			keep = geoSettingKeys[string(key)] || string(key) == "geodata-mode"
		}
		if keep {
			part.Write(line)
		}
	}
	doc, err := parseDocument("config", part.Bytes())
	if err != nil {
		return GeoNeeds{}, GeoUpdate{}, err
	}
	if doc == nil {
		return GeoNeeds{}, GeoUpdate{Interval: defaultGeoInterval}, nil
	}
	needs, update := detectGeo(doc.Content[0])
	return GeoNeeds{GeodataMode: needs.GeodataMode, URLs: needs.URLs}, update, nil
}

// detectGeo 同 DetectGeo，作用于已展开别名的顶层映射；同时读出自动更新的设置。
func detectGeo(root *yaml.Node) (GeoNeeds, GeoUpdate) {
	var needs GeoNeeds
	update := GeoUpdate{Interval: defaultGeoInterval}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		switch k.Value {
		case "geodata-mode":
			needs.GeodataMode = isTrue(v)
		case "geox-url":
			var urls map[string]string
			if v.Decode(&urls) == nil && len(urls) > 0 {
				needs.URLs = urls
			}
		case "geo-auto-update":
			update.Auto = isTrue(v)
		case "geo-update-interval":
			var hours int
			if v.Decode(&hours) == nil && hours > 0 {
				update.Interval = time.Duration(hours) * time.Hour
			}
		}
		if !geoSettingKeys[k.Value] && k.Value != "geodata-mode" {
			needs.scan(v)
		}
	}
	return needs, update
}

// isTrue 报告 v 是否为布尔值 true（同内核的读法：true、True、TRUE）。
func isTrue(v *yaml.Node) bool {
	var b bool
	return v.Kind == yaml.ScalarNode && v.Decode(&b) == nil && b
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
