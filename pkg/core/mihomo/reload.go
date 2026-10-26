package mihomo

import (
	"context"
	"fmt"
)

// Reloader 让运行中的内核重新加载配置文件，再确认内核在加载后仍然响应 API。
type Reloader struct {
	Client *Client
	// ConfigPath 是要加载的配置文件，须位于内核工作目录内。
	ConfigPath string
}

// Reload 重新加载配置。内核没有运行时返回 ErrNotRunning，配置会在内核启动时生效。
func (r *Reloader) Reload(ctx context.Context) error {
	if err := r.Client.ReloadConfig(ctx, r.ConfigPath); err != nil {
		return err
	}
	if _, err := r.Client.Version(ctx); err != nil {
		// 用 %v 而非 %w：加载后内核退出（ErrNotRunning）意味着新配置出了问题，
		// 调用方必须当作加载失败回滚，而不是当作“内核没有运行”。
		return fmt.Errorf("mihomo stopped responding after loading the configuration: %v", err)
	}
	return nil
}
