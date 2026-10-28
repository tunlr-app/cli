package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// 生效的配置相对于现在发布会得到的配置的状态（tunlr status 的漂移）。
const (
	// DriftNone 表示生效的配置就是现在发布会得到的配置。
	DriftNone = "none"
	// DriftPending 表示有尚未发布的改动：订阅更新了、mixin 或设置改了、升级 tunlr 后模板变了、回滚到了旧修订。
	DriftPending = "pending"
	// DriftEdited 表示生效的配置与所有历史修订都不同：在 tunlr 之外被改过，tunlr apply 会覆盖它。
	DriftEdited = "edited"
	// DriftError 表示无法合成（mixin 有错、端口安全检查不通过、订阅没有内容等），tunlr plan 给出详情。
	DriftError = "error"
)

// Drift 是生效的配置的漂移情况。
type Drift struct {
	State string `json:"state"`
	// Sections 是有变化的顶层键，按在配置中的顺序（DriftPending、DriftEdited）。
	Sections []string `json:"sections,omitempty"`
	// Error 是无法合成的原因（DriftError）。
	Error string `json:"error,omitempty"`
}

// Drift 比较生效的配置与现在发布会得到的配置：只合成并比较，不运行 mihomo -t。还没有发布过配置时
// 返回 nil。合成出错不算错误，记为 DriftError；context 取消和读取失败时返回错误。
func (p *Pipeline) Drift(ctx context.Context) (*Drift, error) {
	_, rev, err := p.Revisions()
	if err != nil {
		return nil, err
	}
	plan, err := p.plan(ctx, planOptions{noValidate: true})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		current, rerr := p.readCurrent()
		if rerr != nil || current == nil {
			return nil, rerr
		}
		return &Drift{State: DriftError, Error: err.Error()}, nil
	}
	if plan.Current == nil {
		return nil, nil
	}
	d := &Drift{State: DriftNone}
	switch {
	case !plan.Changed():
	case rev == 0:
		d.State = DriftEdited
	default:
		d.State = DriftPending
	}
	for _, s := range plan.DiffSections {
		if s.Key != "" {
			d.Sections = append(d.Sections, s.Key)
		}
	}
	return d, nil
}

// DriftKey 是漂移的输入（状态、设置、订阅、mixin、生效的配置和历史）的指纹：按文件的大小和修改时间
// 计算，不读内容。指纹不变时 Drift 的结果不变，调用方可以据此缓存（主菜单每 10 秒刷新一次状态）。
func (p *Pipeline) DriftKey() (string, error) {
	var b strings.Builder
	stat := func(path string) error {
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(&b, "%s -\n", path)
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %d %d\n", path, fi.Size(), fi.ModTime().UnixNano())
		return nil
	}
	for _, f := range []string{p.Layout.StateFile(), p.Layout.SettingsFile(), p.Layout.CoreConfig()} {
		if err := stat(f); err != nil {
			return "", err
		}
	}
	for _, dir := range []string{p.Layout.MixinDir(), p.Layout.SubscriptionsDir(), p.Layout.HistoryDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		for _, e := range entries {
			if err := stat(filepath.Join(dir, e.Name())); err != nil {
				return "", err
			}
		}
	}
	return b.String(), nil
}
