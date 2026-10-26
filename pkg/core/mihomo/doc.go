// Package mihomo 负责内核的下载、升级、已知可用版本管理，以及经 Unix socket
// 访问内核 REST API 的 client。
//
// 本包不 import mihomo 的任何 Go 包，只与独立运行的内核进程交互。
package mihomo
