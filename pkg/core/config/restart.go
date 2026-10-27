package config

import (
	"reflect"
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

// RestartKeys 返回从 old 到 new 有变化、需要重启内核才能生效的顶层键。任一份为 nil（无法解析）
// 时返回 nil，交给 mihomo -t 报告。
func RestartKeys(old, new *Facts) []string {
	if old == nil || new == nil {
		return nil
	}
	var changed []string
	for _, k := range restartKeys {
		if !reflect.DeepEqual(old.restart[k], new.restart[k]) {
			changed = append(changed, k)
		}
	}
	return changed
}
