// Package service 负责 systemd unit 与 timer 的生成和控制。
// 工具生成的 unit 统一使用 tunlr- 前缀，控制操作直接执行 systemctl。
package service
