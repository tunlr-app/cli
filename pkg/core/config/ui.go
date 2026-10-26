package config

import (
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// ExternalUI 返回配置中面板的目录和下载地址。目录按内核的规则得出：external-ui 为相对路径时
// 相对于工作目录 home，再接上 external-ui-name。没有配置 external-ui，或 external-ui-name
// 不是单纯的子目录名时，dir 为空。
func ExternalUI(cfg []byte, home string) (dir, url string, err error) {
	var c struct {
		UI   string `yaml:"external-ui"`
		Name string `yaml:"external-ui-name"`
		URL  string `yaml:"external-ui-url"`
	}
	if err := yaml.Unmarshal(cfg, &c); err != nil {
		return "", "", err
	}
	if c.UI == "" {
		return "", c.URL, nil
	}
	dir = c.UI
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(home, dir)
	}
	if c.Name != "" {
		if !filepath.IsLocal(c.Name) {
			return "", c.URL, nil
		}
		dir = filepath.Join(dir, c.Name)
	}
	return dir, c.URL, nil
}
