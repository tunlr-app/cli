// tunlr 是在 Linux 服务器上运行 mihomo 的运维工具。
package main

import (
	"os"
	"runtime/debug"

	"github.com/tunlr-app/cli/internal/cli"
)

// 由 goreleaser 通过 -ldflags -X 注入。
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// memoryLimit 是 Go 堆的软上限。默认设置下堆涨到上次回收后存活数据的两倍才回收，处理大订阅时
// 进程占用约为实际所需的两倍；接近上限时运行时会提早回收，超过也不会出错，只是回收更频繁。
// 小内存设备上 tunlr 与内核同时运行，宁可多花一点 CPU。环境变量 GOMEMLIMIT 可以覆盖。
const memoryLimit = 64 << 20

func main() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memoryLimit)
	}
	os.Exit(cli.Main(cli.BuildInfo{Version: version, Commit: commit, Date: date}, os.Args[1:]))
}
