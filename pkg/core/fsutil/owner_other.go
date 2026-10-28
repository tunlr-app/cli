//go:build !unix

package fsutil

import "io/fs"

// FileOwner 返回 fi 的属主；平台不提供时 ok 为 false。
func FileOwner(fi fs.FileInfo) (o Owner, ok bool) {
	return Owner{}, false
}
