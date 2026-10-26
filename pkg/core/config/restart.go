package config

import (
	"reflect"

	"go.yaml.in/yaml/v3"
)

// restartKeys 是热重载不会生效、变化时需要重启内核的顶层键。内核的热重载只调用
// executor.ApplyConfig，不重建 API 服务（external-controller*、secret、external-ui* 等），
// geo 定时器也只在启动时注册（读 mihomo 源码 hub/hub.go、main.go，并实测确认）。
var restartKeys = []string{
	"external-controller",
	"external-controller-tls",
	"external-controller-unix",
	"external-controller-pipe",
	"external-controller-cors",
	"external-doh-server",
	"secret",
	"external-ui",
	"external-ui-name",
	"tls",
	"geo-auto-update",
	"geo-update-interval",
}

// RestartKeys 返回从 old 到 new 有变化、需要重启内核才能生效的顶层键。
func RestartKeys(old, new []byte) ([]string, error) {
	var a, b map[string]any
	if err := yaml.Unmarshal(old, &a); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(new, &b); err != nil {
		return nil, err
	}
	var changed []string
	for _, k := range restartKeys {
		if !reflect.DeepEqual(a[k], b[k]) {
			changed = append(changed, k)
		}
	}
	return changed, nil
}
