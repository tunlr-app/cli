// Package templates 打包 tunlr 内置的模板文件。
package templates

import (
	"embed"
)

// Systemd 是 systemd unit 的模板（text/template），文件名即 unit 名。
//
//go:embed systemd
var Systemd embed.FS

// Nft 是透明代理开启时工具补充的 nftables 规则（text/template），由 nft -f 加载。
//
//go:embed nft/tunlr.nft
var Nft []byte

// Config 是默认的 mihomo 基础配置，即配置合成的第一层（系统部分）。
//
//go:embed config.yaml
var Config []byte

// Mixin 是新建 mixin 文件时的内容：用法说明和注释掉的示例。
//
//go:embed mixin.yaml
var Mixin []byte
