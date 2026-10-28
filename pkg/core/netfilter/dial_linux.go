package netfilter

import (
	"net"
	"syscall"
)

// MarkedDialer 返回给连接打上 Mark 的 Dialer：工具的 ip rule 让带标记的包查主路由表，不进 TUN，
// 透明代理开启时也能直接访问局域网（如向路由器查询设备名称）。需要 root。
func MarkedDialer() *net.Dialer {
	return &net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, Mark)
		})
		if err != nil {
			return err
		}
		return serr
	}}
}
