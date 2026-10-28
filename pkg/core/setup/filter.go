package setup

import (
	"context"
	"fmt"
	"slices"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// FilterStatus 是 tunlr filter 的结果：各个过滤项（DESIGN.md 8.13）。
type FilterStatus struct {
	// Direct 是设为直连的设备（状态文件中的记录）。
	Direct []state.Device `json:"direct"`
	// Sources 是全部直连的来源地址段。
	Sources []string `json:"sources"`
	// QUIC 报告是否拒绝 QUIC（UDP 443）。
	QUIC bool `json:"quic"`
	// Ports 报告是否只代理 PortList 中的端口，其余直连。
	Ports bool `json:"ports"`
	// PortList 是只代理的端口，DefaultPorts 报告它是默认的列表。
	PortList     []string `json:"port_list"`
	DefaultPorts bool     `json:"default_ports"`
}

// Active 报告有没有开启的过滤项。
func (s *FilterStatus) Active() bool {
	return len(s.Direct) > 0 || len(s.Sources) > 0 || s.QUIC || s.Ports
}

// Filter 返回各个过滤项。
func (i *Installer) Filter() (*FilterStatus, error) {
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return nil, err
	}
	return filterStatus(st), nil
}

func filterStatus(st *state.State) *FilterStatus {
	s := &FilterStatus{Direct: []state.Device{}, Sources: append([]string{}, st.Filter.Sources...), QUIC: st.Filter.QUIC, Ports: st.Filter.Ports,
		PortList: pipeline.PortsOf(st.Filter), DefaultPorts: len(st.Filter.PortList) == 0}
	for _, d := range st.Devices {
		if d.Route == state.RouteDirect {
			s.Direct = append(s.Direct, d)
		}
	}
	return s
}

// FilterEdit 是对全局过滤项的修改，字段为 nil 或空时不变。
type FilterEdit struct {
	// AddSource、RemoveSource 是要加入、去掉的直连来源地址段（地址或 CIDR）。
	AddSource    string
	RemoveSource string
	QUIC         *bool
	Ports        *bool
	// PortList 替换只代理的端口（已经 pipeline.ParsePorts 检查），与默认的列表相同时记为默认。
	PortList []string
}

// EditFilter 返回把 edit 用于 f 之后的过滤项，不修改 f。地址段无效、要去掉的地址段不在名单中时返回满足
// errors.Is(err, core.ErrInvalidArgument) 的错误；加入已有的地址段不变。
func EditFilter(f state.Filter, edit FilterEdit) (state.Filter, error) {
	f.Sources = slices.Clone(f.Sources)
	if edit.AddSource != "" {
		p, err := pipeline.ParseSource(edit.AddSource)
		if err != nil {
			return f, fmt.Errorf("%w: %w", core.ErrInvalidArgument, err)
		}
		if !slices.Contains(f.Sources, p) {
			f.Sources = append(f.Sources, p)
		}
	}
	if edit.RemoveSource != "" {
		p, err := pipeline.ParseSource(edit.RemoveSource)
		if err != nil {
			return f, fmt.Errorf("%w: %w", core.ErrInvalidArgument, err)
		}
		k := slices.Index(f.Sources, p)
		if k < 0 {
			return f, fmt.Errorf("%w: %s is not in the list of direct sources", core.ErrInvalidArgument, p)
		}
		if f.Sources = slices.Delete(f.Sources, k, k+1); len(f.Sources) == 0 {
			f.Sources = nil
		}
	}
	if edit.QUIC != nil {
		f.QUIC = *edit.QUIC
	}
	if edit.Ports != nil {
		f.Ports = *edit.Ports
	}
	if edit.PortList != nil {
		f.PortList = slices.Clone(edit.PortList)
		if slices.Equal(f.PortList, pipeline.DefaultPorts) {
			f.PortList = nil
		}
	}
	return f, nil
}

// SameFilter 报告两组过滤项是否相同。
func SameFilter(a, b state.Filter) bool {
	return a.QUIC == b.QUIC && a.Ports == b.Ports && slices.Equal(a.PortList, b.PortList) && slices.Equal(a.Sources, b.Sources)
}

// PlanFilter 把 edit 用于全局过滤项，返回修改前后的过滤项和按修改后的合成的计划（pipeline.PlanFilter）。
func (i *Installer) PlanFilter(ctx context.Context, edit FilterEdit) (before, after state.Filter, plan *pipeline.Plan, err error) {
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return before, after, nil, err
	}
	before = st.Filter
	if after, err = EditFilter(before, edit); err != nil {
		return before, after, nil, err
	}
	plan, err = i.pipeline(nil).PlanFilter(ctx, after)
	return before, after, plan, err
}

// SaveFilter 保存全局过滤项，并发布 plan（pipeline.PlanFilter(ctx, f) 的结果，已经用户确认）。发布失败时
// 过滤项恢复原状，配置由流水线回滚。
func (i *Installer) SaveFilter(ctx context.Context, f state.Filter, plan *pipeline.Plan) (*config.Revision, error) {
	return i.saveAndApply(ctx, plan, func(st *state.State) func() {
		previous := st.Filter
		st.Filter = f
		return func() { st.Filter = previous }
	})
}
