//go:build !unix

package fsutil

// syncDir 在非 Unix 平台上不做任何事。tunlr 只在 Linux 上运行，这里仅为本地开发能编译和测试。
func syncDir(string) error { return nil }
