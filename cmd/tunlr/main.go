// tunlr 是在 Linux 服务器上运行 mihomo 的运维工具。
package main

import (
	"os"

	"github.com/tunlr-app/cli/internal/cli"
)

// 由 goreleaser 通过 -ldflags -X 注入。
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(cli.Main(cli.BuildInfo{Version: version, Commit: commit, Date: date}, os.Args[1:]))
}
