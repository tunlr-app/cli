// Package templates 打包 tunlr 内置的模板文件。
package templates

import _ "embed"

// Config 是默认的 mihomo 基础配置，即配置合成的第一层（系统部分）。
//
//go:embed config.yaml
var Config []byte
