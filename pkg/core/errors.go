// Package core 包含 tunlr 的全部业务逻辑，供 CLI 以及将来的 agent 等组件复用。
//
// 子包按职责划分：config（配置流水线）、mihomo（内核与 API）、netfilter（nftables）、
// service（systemd）、doctor（诊断）、lock（文件锁）。
package core

import "errors"

// 业务层的错误分类。调用方用 errors.Is 判断，CLI 据此映射退出码。
// 返回时用 fmt.Errorf("...: %w", ErrCheckFailed) 包装以携带具体原因。
var (
	// ErrCheckFailed 表示检查未通过，例如 doctor 发现环境不满足、mihomo -t 校验失败。
	ErrCheckFailed = errors.New("check failed")

	// ErrRolledBack 表示操作失败，但已自动回滚到之前的状态。
	ErrRolledBack = errors.New("operation failed and was rolled back")
)
