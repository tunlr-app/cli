package config

import (
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// Facts 是发布流程需要从一份配置中读取的信息：需要重启才生效的键、节点、geo 数据、面板。
// 它们从同一次解析中提取，免得各处各自完整解析大配置（每次解析约占配置大小十倍的内存）。
// Compose 的结果自带 Facts；其他配置（如磁盘上生效的配置）用 Inspect 得到。
type Facts struct {
	// Geo 是配置需要的 geo 数据，见 DetectGeo。
	Geo GeoNeeds
	// GeoUpdate 是内核自己更新 geo 数据的设置。
	GeoUpdate GeoUpdate

	restart map[string]any
	proxies namedProxies
	// proxiesOK 为 false 表示顶层 proxies 不是由映射组成的列表，无法比较节点。
	proxiesOK bool
	ui        struct{ dir, name, url string }
	dns       dnsFacts
}

// Inspect 解析 cfg 并提取 Facts。内容为空时返回零值的 Facts。
func Inspect(cfg []byte) (*Facts, error) {
	doc, err := parseDocument("config", cfg)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return &Facts{proxiesOK: true, proxies: namedProxies{items: map[string]map[string]any{}}}, nil
	}
	return factsOf(doc.Content[0]), nil
}

// factsOf 从已展开别名的顶层映射中提取 Facts。
func factsOf(root *yaml.Node) *Facts {
	f := &Facts{restart: map[string]any{}, proxiesOK: true, proxies: namedProxies{items: map[string]map[string]any{}}}
	restart := map[string]bool{}
	for _, k := range restartKeys {
		restart[k] = true
	}
	str := func(n *yaml.Node) string {
		var s string
		if n.Decode(&s) != nil {
			return ""
		}
		return s
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i].Value, root.Content[i+1]
		if restart[k] {
			var x any
			if v.Decode(&x) == nil {
				f.restart[k] = x
			}
		}
		switch k {
		case "proxies":
			var list []map[string]any
			if v.Decode(&list) != nil {
				f.proxiesOK = false
				break
			}
			for _, p := range list {
				name, _ := p["name"].(string)
				if _, dup := f.proxies.items[name]; dup {
					continue
				}
				f.proxies.names = append(f.proxies.names, name)
				f.proxies.items[name] = p
			}
		case "external-ui":
			f.ui.dir = str(v)
		case "external-ui-name":
			f.ui.name = str(v)
		case "external-ui-url":
			f.ui.url = str(v)
		case "dns":
			f.dns = dnsOf(v)
		}
	}
	f.Geo, f.GeoUpdate = detectGeo(root)
	return f
}

// ExternalUI 返回配置中面板的目录和下载地址。目录按内核的规则得出：external-ui 为相对路径时
// 相对于工作目录 home，再接上 external-ui-name。没有配置 external-ui，或 external-ui-name
// 不是单纯的子目录名时，dir 为空。
func (f *Facts) ExternalUI(home string) (dir, url string) {
	if f.ui.dir == "" {
		return "", f.ui.url
	}
	dir = f.ui.dir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(home, dir)
	}
	if f.ui.name != "" {
		if !filepath.IsLocal(f.ui.name) {
			return "", f.ui.url
		}
		dir = filepath.Join(dir, f.ui.name)
	}
	return dir, f.ui.url
}
