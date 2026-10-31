package mihomo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tunlr-app/cli/pkg/core/service"
)

// Reloader 让运行中的内核重新加载配置文件，再确认内核在加载后仍然响应 API。
type Reloader struct {
	Client *Client
	// ConfigPath 是要加载的配置文件，须位于内核工作目录内。
	ConfigPath string
	// RecentLog 返回内核最近的日志，用于解释重载超时；为空时读 journalctl（service.RecentLog）。
	RecentLog func(ctx context.Context) string
}

// Reload 重新加载配置。内核没有运行时返回 ErrNotRunning，配置会在内核启动时生效。
// 内核在时限内没有响应时（加载卡住了），错误里附上它最近的日志，不然事后连接日志很快会把线索冲掉。
func (r *Reloader) Reload(ctx context.Context) error {
	if err := r.Client.ReloadConfig(ctx, r.ConfigPath); err != nil {
		if errors.Is(err, ErrNoResponse) {
			if log := r.recentLog(ctx); log != "" {
				err = fmt.Errorf("%w\nmihomo's recent log (journalctl -u %s):\n%s", err, service.CoreUnit, log)
			}
		}
		return err
	}
	if _, err := r.Client.Version(ctx); err != nil {
		// 用 %v 而非 %w：加载后内核退出（ErrNotRunning）意味着新配置出了问题，
		// 调用方必须当作加载失败回滚，而不是当作“内核没有运行”。
		return fmt.Errorf("mihomo stopped responding after loading the configuration: %v", err)
	}
	return nil
}

func (r *Reloader) recentLog(ctx context.Context) string {
	if r.RecentLog != nil {
		return r.RecentLog(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return service.RecentLog(ctx, 20)
}
