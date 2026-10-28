//go:build !linux

package netfilter

import "net"

// MarkedDialer 在 Linux 以外的系统上（只用于开发时运行测试）返回普通的 Dialer。
func MarkedDialer() *net.Dialer {
	return &net.Dialer{}
}
