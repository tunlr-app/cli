package config

import (
	"go.yaml.in/yaml/v3"
)

// subDNSKey 是 tunlr: 下采用订阅的 DNS 的开关。
const subDNSKey = "sub-dns"

// SubscriptionDNSKeys 是默认不从订阅采用的顶层键：DNS 和配合它调的嗅探，用 tunlr 的兜底值（tunlr:defaults）。
// 机场的 DNS 是为桌面客户端写的，常缺 respect-rules、按中国和境外分开的 nameserver-policy，在服务器上不好用。
// mixin 中 tunlr.sub-dns: true 时采用订阅的（订阅没写的仍用兜底值）。
var SubscriptionDNSKeys = []string{"dns", "sniffer"}

// checkSubDNS 检查 tunlr.sub-dns 的值：true、false 或 null。
func (m *mixer) checkSubDNS(v *yaml.Node) error {
	var b bool
	if isNull(v) || (v.Kind == yaml.ScalarNode && v.Tag == "!!bool" && v.Decode(&b) == nil) {
		return nil
	}
	return m.errorf(v.Line, "%s.%s must be true or false", namespace, subDNSKey)
}

// takeSubDNS 按 mixin src（已经检查过）的 tunlr.sub-dns 设置 m.subDNS：true 时记下这个文件，false 或 null 时清空。
// mixin 按应用的顺序调用，后面的文件压过前面的，例如订阅自己的 false 压过共用的 true。
func (m *mixer) takeSubDNS(src *yaml.Node) {
	v := lookup(src, []string{namespace, subDNSKey})
	if v == nil {
		return
	}
	m.subDNS = ""
	if isTrue(v) {
		m.subDNS = m.file
	}
}

// MixinSubDNS 读出 mixin 内容 data 的 tunlr.sub-dns：set 报告写没写，on 是它的值；内容有错时返回 *MixinError
// （文件名为 name）。
func MixinSubDNS(name string, data []byte) (set, on bool, err error) {
	doc, err := parseNode(data)
	if err != nil {
		return false, false, &MixinError{File: name, Err: err}
	}
	if doc == nil {
		return false, false, nil
	}
	v := lookup(doc.Content[0], []string{namespace, subDNSKey})
	if v == nil {
		return false, false, nil
	}
	if err := (&mixer{file: name}).checkSubDNS(v); err != nil {
		return false, false, err
	}
	return true, isTrue(v), nil
}
