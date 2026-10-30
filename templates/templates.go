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

// GatewayNft 是网关模式开启时的 nftables 规则（text/template），由 nft -f 加载。
//
//go:embed nft/gateway.nft
var GatewayNft []byte

// AccessNft 是代理端口、面板对其他设备开放时的 nftables 规则（text/template），由 nft -f 加载。
//
//go:embed nft/access.nft
var AccessNft []byte

// DevicesNft 是记录局域网设备的 nftables 表（text/template），由 nft -f 加载。
//
//go:embed nft/devices.nft
var DevicesNft []byte

// Config 是默认的 mihomo 基础配置，即配置合成的第一层（系统部分）。
//
//go:embed config.yaml
var Config []byte

// Mixin 是新建 mixin 文件时的内容：用法说明和注释掉的示例。
//
//go:embed mixin.yaml
var Mixin []byte

// Defaults 是受管 mixin tunlr:defaults 的内容：订阅没有写时使用的 geo 数据设置和 DNS，写在 tunlr.defaults 中
// （订阅写了的顶层键整个替换它，geox-url 按文件）。geo 数据由内核自己按 geo-update-interval（小时）下载和更新；
// 设置了 github-mirror 时，geox-url 中 GitHub 的地址由 tunlr:mirror 改为经镜像。dns 不写监听地址（由 tunlr:dns
// 提供）；开启 TUN 时 tunlr:tun 强制 dns.enable: true。它只在内存中合成，不写入 mixin.d；用户按订阅用
// tunlr update set geo 覆盖，或在自己的 mixin 中覆盖。
//
//go:embed defaults.yaml
var Defaults []byte

// Settings 是还没有设置文件时 tunlr settings edit 的起始内容：各项的说明和注释掉的默认值。
//
//go:embed settings.yaml
var Settings []byte
