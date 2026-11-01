package setup

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
)

// fakeIPFilterAppend 是 DNSMixinFile 的 tunlr: 中你加进 fake-ip-filter 的条目。
const fakeIPFilterAppend = "fake-ip-filter.append"

// FakeIPFilter 是 dns.fake-ip-filter 中你加的条目，以及它们加入的那份列表。
type FakeIPFilter struct {
	// Entries 是你加的条目（DNSMixinFile 中的 tunlr.fake-ip-filter.append），按书写顺序。
	Entries []string `json:"entries"`
	// Base 是不算这些条目时合成的配置中的列表：兜底值的或订阅的，经其他 mixin 修改后。
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
	// Changes 是要改的 mixin 文件（DNSMixinFile）。
	Changes []*MixinChange `json:"changes"`
}

// Changed 报告条目是否有变化。
func (e *FakeIPFilterEdit) Changed() bool { return !slices.Equal(e.Before, e.Entries) }

// FakeIPFilterEntries 只读出你加的条目（不合成配置），用于状态显示。
func (i *Installer) FakeIPFilterEntries() ([]string, error) {
	data, err := i.readMixinFile(DNSMixinFile)
	if err != nil || data == nil {
		return nil, err
	}
	entries, _, err := config.MixinFakeIPFilter(DNSMixinFile, data)
	return entries, err
}

// FakeIPFilter 返回你加的条目和它们加入的列表（按启用的订阅合成）。
func (i *Installer) FakeIPFilter(ctx context.Context) (*FakeIPFilter, error) {
	f, _, err := i.fakeIPFilter(ctx)
	return f, err
}

// fakeIPFilter 是 FakeIPFilter，另外返回 DNSMixinFile 的修改（还没有改内容）。
func (i *Installer) fakeIPFilter(ctx context.Context) (*FakeIPFilter, *MixinChange, error) {
	data, err := i.readMixinFile(DNSMixinFile)
	if err != nil {
		return nil, nil, err
	}
	c := &MixinChange{Name: DNSMixinFile, Path: config.MixinPath(i.Layout.MixinDir(), DNSMixinFile), original: data, existed: data != nil}
	f := &FakeIPFilter{File: c.Path, Entries: []string{}, Base: []string{}}
	if data != nil {
		if f.Entries, _, err = config.MixinFakeIPFilter(DNSMixinFile, data); err != nil {
			return nil, nil, err
		}
	}
	// 去掉追加的条目合成一次，得到它们加入的列表。
	without, err := config.SetTunlrKeys(data, dnsHead, map[string]any{fakeIPFilterAppend: nil})
	if err != nil {
		return nil, nil, fmt.Errorf("mixin %s: %w", DNSMixinFile, err)
	}
	plan, err := i.pipeline(nil).ComposeWithMixin(ctx, config.Mixin{Name: DNSMixinFile, Data: without})
	if err != nil {
		return nil, nil, err
	}
	f.Base = append(f.Base, config.FakeIPFilter(plan.Config)...)
	if f.Entries == nil {
		f.Entries = []string{}
	}
	return f, c, nil
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
	f, c, err := i.fakeIPFilter(ctx)
	if err != nil {
		return nil, nil, err
	}
	e := &FakeIPFilterEdit{FakeIPFilter: *f, Before: f.Entries, Changes: []*MixinChange{c}}
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
	if c.data, err = config.SetTunlrKeys(c.original, dnsHead, map[string]any{fakeIPFilterAppend: value}); err != nil {
		return nil, nil, fmt.Errorf("mixin %s: %w", DNSMixinFile, err)
	}
	c.Removed = c.data == nil && c.existed
	if value != nil {
		c.Set = []string{"tunlr." + fakeIPFilterAppend}
	} else {
		c.Unset = []string{"tunlr." + fakeIPFilterAppend}
	}
	plan, err := i.pipeline(nil).PlanWithMixin(ctx, c.mixin())
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
			if o.Path == "dns.fake-ip-filter" && o.Previous == DNSMixinFile {
				e.LostTo = o.File
			}
		}
	}
	return e, plan, nil
}

// SaveFakeIPFilter 写入 e（PlanFakeIPFilter 的结果，已经用户确认），再发布 plan（配置没有变化或还没有发布过时
// 只写入）。发布失败时文件恢复原状，配置由流水线回滚。
func (i *Installer) SaveFakeIPFilter(ctx context.Context, e *FakeIPFilterEdit, plan *pipeline.Plan) (*config.Revision, error) {
	return i.publishChanges(ctx, e.Changes, plan)
}
