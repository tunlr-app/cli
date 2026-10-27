// Package lock 提供文件锁，保证会改动系统的命令不与 timer 触发的任务并发执行。
package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// ErrBusy 表示锁被另一个 tunlr 操作占用，在等待期限内没有释放。
var ErrBusy = errors.New("another tunlr operation is in progress")

// retryDelay 是锁被占用时的重试间隔。
const retryDelay = 200 * time.Millisecond

// Lock 是一把已获取的排他锁。
type Lock struct {
	f *flock.Flock
}

// Acquire 获取 path 上的排他锁，必要时创建所在目录。锁被占用时按 retryDelay 重试，
// 直到成功或 ctx 结束；因超时结束时返回包装了 ErrBusy 的错误。
func Acquire(ctx context.Context, path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	f := flock.New(path)
	ok, err := f.TryLockContext(ctx, retryDelay)
	switch {
	case ok:
		return &Lock{f: f}, nil
	case errors.Is(err, context.DeadlineExceeded):
		return nil, fmt.Errorf("%w (lock file %s)", ErrBusy, path)
	default:
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
}

// TryAcquire 只尝试一次获取 path 上的排他锁，被占用时立即返回包装了 ErrBusy 的错误。
func TryAcquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	f := flock.New(path)
	ok, err := f.TryLock()
	switch {
	case ok:
		return &Lock{f: f}, nil
	case err == nil:
		return nil, fmt.Errorf("%w (lock file %s)", ErrBusy, path)
	default:
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
}

// Release 释放锁。锁文件保留在磁盘上，删除它会让并发的获取者锁住不同的文件。
func (l *Lock) Release() error {
	return l.f.Unlock()
}
