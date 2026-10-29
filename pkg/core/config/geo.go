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

// GeoKeys 是 geo 数据的自动更新和来源的设置（点分路径），geox-url 按文件分开，按展示的顺序。
var GeoKeys = []string{"geo-auto-update", "geo-update-interval", "geox-url.geoip", "geox-url.geosite", "geox-url.mmdb", "geox-url.asn"}

// 键的来源（KeySource.From）。此外还可以是 mixin 的文件名或受管 mixin 的名称。
const (
	FromTemplate     = "template"
	FromSubscription = "subscription"
)

// KeySource 是合成的配置中一个键的值和它来自哪一层。
type KeySource struct {
	// Path 是点分路径，如 geox-url.geosite。
	Path string `json:"path"`
	// Value 是合成的值（经镜像改写之前）；配置中没有这个键时为空，内核用它的默认值。
	Value string `json:"value,omitempty"`
	From  string `json:"from"`
	// Fallback 为 true 时值是兜底值：来自模板，或 From 这个 mixin 的 tunlr.defaults，订阅没有写它。
	Fallback bool `json:"fallback,omitempty"`
}

// geoSources 返回 GeoKeys 中各键的值和来源：最后写它的 mixin（mihomo: 或没有被订阅替换的 tunlr.defaults），
// 否则订阅（采用了它所在的顶层键，补全进去的键除外），否则模板。在应用完用户的 mixin 之后、改写之前调用。
func (m *mixer) geoSources() []KeySource {
	out := make([]KeySource, 0, len(GeoKeys))
	for _, k := range GeoKeys {
		path := strings.Split(k, ".")
		s := KeySource{Path: k, From: FromTemplate, Fallback: true}
		v := lookup(m.root, path)
		if v != nil && v.Kind == yaml.ScalarNode {
			s.Value = v.Value
		}
		switch {
		case m.setBy[k] != "":
			s.From, s.Fallback = m.setBy[k], m.fallback[k]
		case len(path) > 1 && m.setBy[path[0]] != "":
			s.From, s.Fallback = m.setBy[path[0]], m.fallback[path[0]]
		// 订阅的 geox-url 没写、前面也没有的文件用内核的默认地址，不算订阅的。
		case m.subKeys[path[0]] && !m.filled[k] && v != nil:
			s.From, s.Fallback = FromSubscription, false
		}
		out = append(out, s)
	}
	return out
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
