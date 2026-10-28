//go:build unix

package fsutil

import (
	"io/fs"
	"syscall"
)

// FileOwner 返回 fi 的属主；平台不提供时 ok 为 false。
func FileOwner(fi fs.FileInfo) (o Owner, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Owner{}, false
	}
	return Owner{UID: int(st.Uid), GID: int(st.Gid)}, true
}
