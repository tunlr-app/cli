package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
)

// MixinChange 是命令对一个 mixin 文件的修改。命令和编辑器改的是同一份文件：命令改的是现在决定那个键的文件，
// 没有时写进它自己的文件（如 DNSMixinFile），文件中的其他内容和注释保留。
type MixinChange struct {
	// Name 是 mixin 的名称（如 90-dns.yaml、home/20-geo.yaml），Path 是文件的路径。
	Name string `json:"name"`
	Path string `json:"path"`
	// Set 是设置的键，Unset 是删除的键（mihomo: 中的点分路径），按名称排序。
	Set   []string `json:"set,omitempty"`
	Unset []string `json:"unset,omitempty"`
	// Removed 报告改完没有内容，文件被删除。
	Removed bool `json:"removed,omitempty"`

	// data 是新的内容，nil 表示删除文件；original、existed 是读到的内容和文件是否存在。
	data     []byte
	original []byte
	existed  bool
}

// mixin 返回预览时代替磁盘上的文件的 mixin（Data 为 nil 表示删除，见 pipeline.PlanWithMixins）。
func (c *MixinChange) mixin() config.Mixin { return config.Mixin{Name: c.Name, Data: c.data} }

// mixinsOf 返回 changes 对应的 mixin，用于预览。
func mixinsOf(changes []*MixinChange) []config.Mixin {
	ms := make([]config.Mixin, len(changes))
	for i, c := range changes {
		ms[i] = c.mixin()
	}
	return ms
}

// planKeys 准备在 mixin 的 mihomo: 中设置 values（点分路径；值为 nil 时删除）。scope 是可以改的 mixin（按应用的
// 顺序，含内容），from 是现在决定各键的层（config.KeySource 的 From，兜底值时不列）。要设置的键写进决定它的
// 文件（它在 scope 中时），否则写进 home（不存在时新建，以 head 开头）；要删除的键从 scope 中所有写了它的文件里
// 删除。返回有变化的文件，按第一次用到的顺序。
func planKeys(dir string, scope []config.Mixin, from map[string]string, values map[string]any, home, head string) ([]*MixinChange, error) {
	var changes []*MixinChange
	vals := map[string]map[string]any{}
	change := func(name string) *MixinChange {
		if i := slices.IndexFunc(changes, func(c *MixinChange) bool { return c.Name == name }); i >= 0 {
			return changes[i]
		}
		c := &MixinChange{Name: name, Path: config.MixinPath(dir, name)}
		if i := slices.IndexFunc(scope, func(m config.Mixin) bool { return m.Name == name }); i >= 0 {
			c.original, c.existed = scope[i].Data, true
		} else if data, err := os.ReadFile(c.Path); err == nil {
			// home 不在 scope 中（如没有启用的订阅的 mixin）时按磁盘上的。
			c.original, c.existed = data, true
		}
		changes, vals[name] = append(changes, c), map[string]any{}
		return c
	}
	for _, k := range slices.Sorted(maps.Keys(values)) {
		if v := values[k]; v != nil {
			target := home
			if f := from[k]; f != "" && slices.ContainsFunc(scope, func(m config.Mixin) bool { return m.Name == f }) {
				target = f
			}
			c := change(target)
			c.Set, vals[target][k] = append(c.Set, k), v
			continue
		}
		for _, m := range scope {
			if config.MixinSets(m.Data, k) {
				c := change(m.Name)
				c.Unset, vals[m.Name][k] = append(c.Unset, k), nil
			}
		}
	}
	out := changes[:0]
	for _, c := range changes {
		h := ""
		if c.Name == home {
			h = head
		}
		data, err := config.SetMixinKeys(c.original, h, vals[c.Name])
		if err != nil {
			return nil, fmt.Errorf("mixin %s: %w", c.Name, err)
		}
		if (data == nil && !c.existed) || (data != nil && c.existed && bytes.Equal(data, c.original)) {
			continue
		}
		c.data, c.Removed = data, data == nil
		out = append(out, c)
	}
	return out, nil
}

// writeChanges 写入 changes；中途失败时把已经写了的改回去。调用方持有文件锁。
func writeChanges(dir string, changes []*MixinChange) error {
	for i, c := range changes {
		if err := replaceMixin(dir, c.Name, c.data, c.original, c.existed); err != nil {
			return errors.Join(err, undoChanges(dir, changes[:i]))
		}
	}
	return nil
}

// undoChanges 把已经写入的 changes 改回原来的内容（原来不存在的删除）。
func undoChanges(dir string, changes []*MixinChange) error {
	var errs []error
	for _, c := range changes {
		var original []byte
		if c.existed {
			original = c.original
		}
		errs = append(errs, replaceMixin(dir, c.Name, original, c.data, c.data != nil))
	}
	return errors.Join(errs...)
}

// replaceMixin 把 mixin name 的内容换成 data（nil 为删除文件）。previous、existed 是之前读到的内容和文件是否存在，
// 文件此后被改动过时返回 config.ErrMixinChanged。
func replaceMixin(dir, name string, data, previous []byte, existed bool) error {
	if data != nil {
		return config.SaveMixin(dir, config.Mixin{Name: name, Data: data}, previous, existed)
	}
	if !existed {
		return nil
	}
	path := config.MixinPath(dir, name)
	cur, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(cur, previous) {
		return errors.Join(config.ErrMixinChanged, err)
	}
	return os.Remove(path)
}

// publishChanges 写入 changes（已经用户确认），再发布 plan（按它们合成的；配置没有变化或还没有发布过时只写入）。
// 发布失败时文件恢复原状，配置由流水线回滚。
func (i *Installer) publishChanges(ctx context.Context, changes []*MixinChange, plan *pipeline.Plan) (*config.Revision, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	dir := i.Layout.MixinDir()
	if err := writeChanges(dir, changes); err != nil {
		return nil, err
	}
	if plan == nil || plan.Current == nil || !plan.Changed() {
		return nil, nil
	}
	applied, err := i.pipeline(nil).ApplyLocked(ctx, plan)
	if err != nil {
		return nil, errors.Join(err, undoChanges(dir, changes))
	}
	if applied == nil {
		return nil, nil
	}
	return applied.Revision, nil
}
