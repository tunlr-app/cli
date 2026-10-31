package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
)

// FakeIPFilterMixinFile 是 tun dns fake-ip-filter 写入的共用 mixin。排在多数共用文件之后，它追加的条目不会被
// 那些文件用 mihomo: 整份替换的列表盖掉。
const FakeIPFilterMixinFile = "90-fake-ip-filter.yaml"

// FakeIPFilter 是 dns.fake-ip-filter 中你加的条目，以及它们加入的那份列表。
type FakeIPFilter struct {
	// Entries 是你加的条目（FakeIPFilterMixinFile 中的 tunlr.fake-ip-filter.append），按书写顺序。
	Entries []string `json:"entries"`
	// Base 是不算这些条目时合成的配置中的列表：订阅的或兜底值的，经其他 mixin 修改后。
	Base []string `json:"base"`
	// File 是写入条目的 mixin 文件。
	File string `json:"file"`
}

// FakeIPFilterEdit 是 PlanFakeIPFilter 准备的修改，由 SaveFakeIPFilter 写入。
type FakeIPFilterEdit struct {
	FakeIPFilter
	// Before 是修改前你加的条目。
	Before []string `json:"before"`
	// Duplicates 是给出的条目中本来就在 Base 里、因此没有加入的。
	Duplicates []string `json:"duplicates,omitempty"`
	// Lost 是加入了、却不在合成的配置中的条目：之后应用的 mixin LostTo 整份替换了这个列表。
	Lost   []string `json:"lost,omitempty"`
	LostTo string   `json:"lost_to,omitempty"`

	// data 是新的文件内容，nil 表示删除文件；original、existed 是读到的内容和文件是否存在。
	data     []byte
	original []byte
	existed  bool
}

// Changed 报告条目是否有变化。
func (e *FakeIPFilterEdit) Changed() bool { return !slices.Equal(e.Before, e.Entries) }

// fakeIPFilterHead 是 FakeIPFilterMixinFile 开头的注释。
const fakeIPFilterHead = "Names added to dns.fake-ip-filter: in fake-ip mode they get real addresses, on top of the list\n" +
	"the subscription or tunlr's fallbacks give (tunlr config shows it). Written by tunlr tun dns\n" +
	"fake-ip-filter; you may edit it too, e.g. delete removes entries from that list."

// FakeIPFilterEntries 只读出你加的条目（不合成配置），用于状态显示。
func (i *Installer) FakeIPFilterEntries() ([]string, error) {
	data, err := os.ReadFile(config.MixinPath(i.Layout.MixinDir(), FakeIPFilterMixinFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, _, err := config.MixinFakeIPFilter(FakeIPFilterMixinFile, data)
	return entries, err
}

// FakeIPFilter 返回你加的条目和它们加入的列表（按启用的订阅合成）。
func (i *Installer) FakeIPFilter(ctx context.Context) (*FakeIPFilter, error) {
	f, _, _, err := i.fakeIPFilter(ctx)
	return f, err
}

// fakeIPFilter 是 FakeIPFilter，另外返回文件的内容和它是否存在。
func (i *Installer) fakeIPFilter(ctx context.Context) (f *FakeIPFilter, data []byte, existed bool, err error) {
	path := config.MixinPath(i.Layout.MixinDir(), FakeIPFilterMixinFile)
	data, err = os.ReadFile(path)
	existed = err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, false, err
	}
	f = &FakeIPFilter{File: path, Entries: []string{}, Base: []string{}}
	if existed {
		if f.Entries, _, err = config.MixinFakeIPFilter(FakeIPFilterMixinFile, data); err != nil {
			return nil, nil, false, err
		}
	}
	// 去掉追加的条目合成一次，得到它们加入的列表。
	without, err := config.SetTunlrKeys(data, fakeIPFilterHead, map[string]any{"fake-ip-filter.append": nil})
	if err != nil {
		return nil, nil, false, fmt.Errorf("mixin %s: %w", FakeIPFilterMixinFile, err)
	}
	plan, err := i.pipeline(nil).ComposeWithMixin(ctx, config.Mixin{Name: FakeIPFilterMixinFile, Data: without})
	if err != nil {
		return nil, nil, false, err
	}
	f.Base = append(f.Base, config.FakeIPFilter(plan.Config)...)
	if f.Entries == nil {
		f.Entries = []string{}
	}
	return f, data, existed, nil
}

// ParseFakeIPFilter 把一行一条的文字拆成条目：去掉首尾空白和空行，重复的只留第一条。条目中间有空白时报错，
// 错误可以直接给用户看（文本框中的提示）。
func ParseFakeIPFilter(text string) ([]string, error) {
	var out []string
	for line := range strings.Lines(text) {
		s := strings.TrimSpace(line)
		if s == "" || slices.Contains(out, s) {
			continue
		}
		if strings.ContainsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' }) {
			return nil, fmt.Errorf("%q: one name or pattern per line, such as +.lan or nas.example.com", s)
		}
		out = append(out, s)
	}
	return out, nil
}

// PlanFakeIPFilter 准备把你加的条目换成 entries（已由 ParseFakeIPFilter 整理），返回修改和按它合成的计划
// （pipeline.PlanWithMixin）。已在列表中的条目不加入，记在 Duplicates 中。
func (i *Installer) PlanFakeIPFilter(ctx context.Context, entries []string) (*FakeIPFilterEdit, *pipeline.Plan, error) {
	f, data, existed, err := i.fakeIPFilter(ctx)
	if err != nil {
		return nil, nil, err
	}
	e := &FakeIPFilterEdit{FakeIPFilter: *f, Before: f.Entries, original: data, existed: existed}
	e.Entries = []string{}
	for _, s := range entries {
		if slices.Contains(f.Base, s) {
			e.Duplicates = append(e.Duplicates, s)
		} else {
			e.Entries = append(e.Entries, s)
		}
	}
	var value any
	if len(e.Entries) > 0 {
		value = e.Entries
	}
	if e.data, err = config.SetTunlrKeys(data, fakeIPFilterHead, map[string]any{"fake-ip-filter.append": value}); err != nil {
		return nil, nil, fmt.Errorf("mixin %s: %w", FakeIPFilterMixinFile, err)
	}
	plan, err := i.pipeline(nil).PlanWithMixin(ctx, config.Mixin{Name: FakeIPFilterMixinFile, Data: e.data})
	if err != nil {
		return nil, nil, err
	}
	published := config.FakeIPFilter(plan.Config)
	for _, s := range e.Entries {
		if !slices.Contains(published, s) {
			e.Lost = append(e.Lost, s)
		}
	}
	if len(e.Lost) > 0 {
		for _, o := range plan.Overrides {
			if o.Path == "dns.fake-ip-filter" && o.Previous == FakeIPFilterMixinFile {
				e.LostTo = o.File
			}
		}
	}
	return e, plan, nil
}

// SaveFakeIPFilter 写入 e（PlanFakeIPFilter 的结果，已经用户确认），再发布 plan（配置没有变化或还没有发布过时
// 只写入）。发布失败时文件恢复原状，配置由流水线回滚。
func (i *Installer) SaveFakeIPFilter(ctx context.Context, e *FakeIPFilterEdit, plan *pipeline.Plan) (*config.Revision, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	dir := i.Layout.MixinDir()
	write := func(data, previous []byte, existed bool) error {
		if data != nil {
			return config.SaveMixin(dir, config.Mixin{Name: FakeIPFilterMixinFile, Data: data}, previous, existed)
		}
		if !existed {
			return nil
		}
		cur, err := os.ReadFile(e.File)
		if err != nil || string(cur) != string(previous) {
			return errors.Join(config.ErrMixinChanged, err)
		}
		return os.Remove(e.File)
	}
	if err := write(e.data, e.original, e.existed); err != nil {
		return nil, err
	}
	if plan == nil || plan.Current == nil || !plan.Changed() {
		return nil, nil
	}
	applied, err := i.pipeline(nil).ApplyLocked(ctx, plan)
	if err != nil {
		// 把文件改回去：写入的是 e.data，原来的是 e.original（不存在时删除）。
		var undo error
		if e.existed {
			undo = write(e.original, e.data, e.data != nil)
		} else {
			undo = write(nil, e.data, e.data != nil)
		}
		return nil, errors.Join(err, undo)
	}
	if applied == nil {
		return nil, nil
	}
	return applied.Revision, nil
}
