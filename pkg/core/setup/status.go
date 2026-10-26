package setup

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// Status 汇总 tunlr 的当前状态。版本等一律以内核实际情况为准，工具的记录只作为已知可用的历史。
type Status struct {
	// Installed 报告内核服务是否已安装（tunlr init）。
	Installed bool `json:"installed"`
	// Service 是 tunlr-core.service 的状态，服务未安装时为 nil。
	Service *service.Status `json:"service,omitempty"`
	// Running 是经 API 查询到的运行中的内核版本，内核没有响应时为空。
	Running string `json:"running,omitempty"`
	// Kernel 是内核的安装情况和已知可用版本。
	Kernel *mihomo.KernelStatus `json:"kernel"`
	// Subscription 是启用的订阅。
	Subscription string `json:"subscription,omitempty"`
	// Revision 是与生效配置一致的历史修订，没有发布过或手工改过时为 nil。
	Revision *config.Revision `json:"revision,omitempty"`
	// NextUpdate 是下次定时更新的时间。
	NextUpdate time.Time `json:"next_update,omitzero"`
}

// Status 返回当前状态。
func (i *Installer) Status(ctx context.Context) (*Status, error) {
	sd := i.systemd()
	s := &Status{Installed: sd.Installed()}
	if s.Installed {
		st, err := sd.Status(ctx, service.CoreUnit)
		if err != nil {
			return nil, err
		}
		s.Service = &st
		if v, err := sd.Property(ctx, service.UpdateTimer, "NextElapseUSecRealtime"); err == nil {
			s.NextUpdate = parseTimestamp(v)
		}
	}
	if v, err := mihomo.NewClient(i.Layout.CoreSocket()).Version(ctx); err == nil {
		s.Running = v
	}
	var err error
	if s.Kernel, err = i.kernels(nil, mihomo.Arch{}).Status(); err != nil {
		return nil, err
	}
	st, err := state.Load(i.Layout.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if st != nil {
		s.Subscription = st.Active
	}
	revs, current, err := i.pipeline(nil).Revisions()
	if err != nil {
		return nil, err
	}
	for _, r := range revs {
		if r.Number == current {
			s.Revision = &r
			break
		}
	}
	return s, nil
}

// parseTimestamp 解析 systemctl show 的时间，例如 "Tue 2026-10-27 00:34:57 CST"。时区缩写是本机
// 时区的（systemd 252 的 show 不理会 --timestamp=utc），按 time.Local 解析即得到正确的偏移。
// 无法解析时（如 timer 尚未排期）返回零值。
func parseTimestamp(v string) time.Time {
	t, err := time.ParseInLocation("Mon 2006-01-02 15:04:05 MST", v, time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}
