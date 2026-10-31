// Package subscription 管理订阅：保存多个机场订阅，同一时间只启用一个。
// 订阅内容由工具抓取，每个订阅保留最近一次成功抓取的结果，抓取失败时继续使用它。
package subscription

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/lock"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
)

var (
	ErrNotFound = errors.New("subscription not found")
	ErrExists   = errors.New("subscription already exists")
	// ErrActive 表示要删除的订阅正在启用，需先切换到其他订阅。
	ErrActive = errors.New("subscription is active")
)

// lockWait 是等待其他 tunlr 操作释放锁的最长时间。
const lockWait = 30 * time.Second

// Manager 执行订阅相关的操作。会改动状态的方法都先获取文件锁。
type Manager struct {
	Layout paths.Layout
	// Version 是 tunlr 的版本，写入默认的 User-Agent。
	Version string
	// HTTP 为空时使用带默认超时的客户端。
	HTTP *http.Client
	// Now 为空时使用 time.Now。
	Now func() time.Time
}

// Subscription 是一个订阅的定义和它的抓取记录。
type Subscription struct {
	Def
	state.Fetch
}

// Entry 是 List 返回的一个订阅。
type Entry struct {
	Subscription
	Active bool
	// Summary 统计已保存的内容；内容缺失或无法解析时为零值。
	Summary config.SubscriptionSummary
	// Mixins 是订阅自己的 mixin 文件名（mixin.d/<名称>/*.yaml），按应用顺序；读取失败时为空。
	Mixins []string
}

// Fetched 是一次成功抓取的结果。
type Fetched struct {
	Name    string
	Summary config.SubscriptionSummary
	// Activated 报告这次操作是否让该订阅成为启用的订阅。
	Activated bool
}

// UpdateResult 是 Update 中单个订阅的结果，Err 为空表示成功。
type UpdateResult struct {
	Name    string
	Summary config.SubscriptionSummary
	Err     error
}

// UpdateOptions 调整 Update 的行为。
type UpdateOptions struct {
	// UserAgent 非空时改用它作为这些订阅的 User-Agent，指向空字符串表示恢复默认。
	// 只有抓取成功时才保存，用错的 User-Agent 不会覆盖原来可用的设置。
	UserAgent *string
	// Source 非空时换用这个来源（见 ParseSource），只能用于一个订阅；同样只在读取成功时保存。
	Source *string
	// Stdin 是来源为标准输入时读取的内容。
	Stdin io.Reader
}

// AddOptions 是新订阅的设置。
type AddOptions struct {
	// UserAgent 为空时使用默认值（见 userAgent）。只用于地址。
	UserAgent string
	// Update 是自动更新的周期（见 settings.ParsePeriod），为空时为默认值（见 state.Subscription.DefaultPeriod）。
	Update string
	// Stdin 是来源为标准输入（"-"）时读取的内容。
	Stdin io.Reader
	// Filter、Exclude 不为空时写入订阅 mixin 中的节点筛选（tunlr.proxy-filter 的 filter、exclude-filter，
	// 多条用反引号分隔），文件为 mixin.d/<name>/10-filter.yaml。
	Filter, Exclude string
}

// Add 抓取（或读取）并保存一个新订阅，source 是地址、本地文件或 "-"（见 ParseSource）。
// 抓取或检查失败时什么都不保存。这是第一个订阅时自动启用它。
func (m *Manager) Add(ctx context.Context, name, source string, opts AddOptions) (*Fetched, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	src, err := ParseSource(source)
	if err != nil {
		return nil, err
	}
	if err := checkSource(src, opts.UserAgent, opts.Update); err != nil {
		return nil, err
	}
	if err := settings.ValidateUserAgent(opts.UserAgent); err != nil {
		return nil, err
	}
	if err := validatePeriod(opts.Update); err != nil {
		return nil, err
	}
	filterFile := ""
	if opts.Filter != "" || opts.Exclude != "" {
		for _, s := range []string{opts.Filter, opts.Exclude} {
			if err := config.CheckPatterns(s); err != nil {
				return nil, fmt.Errorf("%w: %v", core.ErrInvalidArgument, err)
			}
		}
		filterFile = filepath.Join(m.Layout.SubscriptionMixinDir(name), config.FilterMixinFile)
		if _, err := os.Stat(filterFile); err == nil {
			return nil, fmt.Errorf("%w: %s already exists; edit it with: tunlr config edit %s/%s",
				core.ErrInvalidArgument, filterFile, name, strings.TrimSuffix(config.FilterMixinFile, ".yaml"))
		}
	}
	lk, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer lk.Release()

	l, st, err := m.load()
	if err != nil {
		return nil, err
	}
	if d := l.findFold(name); d != nil {
		if d.Name != name {
			return nil, fmt.Errorf("%w: %s (names differing only in case count as the same)", ErrExists, d.Name)
		}
		return nil, fmt.Errorf("%w: %s", ErrExists, name)
	}
	sub := Subscription{Def: Def{Name: name, URL: src.URL, Path: src.Path, UserAgent: opts.UserAgent, Update: opts.Update},
		Fetch: state.Fetch{AddedAt: m.now().UTC()}}
	sum, err := m.fetchAndStore(ctx, &sub, opts.Stdin)
	if err != nil {
		return nil, err
	}
	l.Subscriptions = append(l.Subscriptions, sub.Def)
	st.Fetches[name] = sub.Fetch
	res := &Fetched{Name: name, Summary: sum}
	if l.Active == "" {
		l.Active = name
		res.Activated = true
	}
	if err := m.save(l, st); err != nil {
		return nil, err
	}
	if filterFile != "" {
		mx := config.Mixin{Name: name + "/" + config.FilterMixinFile, Data: config.FilterMixin(name, opts.Filter, opts.Exclude)}
		if err := config.SaveMixin(m.Layout.MixinDir(), mx, nil, false); err != nil {
			return res, fmt.Errorf("subscription %s is added, but writing its proxy filter failed: %w", name, err)
		}
	}
	return res, nil
}

// List 返回订阅列表中的订阅和它们的抓取记录，按添加顺序排列。
func (m *Manager) List() ([]Entry, error) {
	l, st, err := m.load()
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, d := range l.Subscriptions {
		s := Subscription{Def: d, Fetch: st.Fetches[d.Name]}
		e := Entry{Subscription: s, Active: s.Name == l.Active}
		if c := s.Counts; c != nil {
			e.Summary = config.SubscriptionSummary(*c)
		} else if data, err := os.ReadFile(m.Layout.SubscriptionFile(s.Name)); err == nil {
			e.Summary, _ = config.InspectSubscription(data)
		}
		if mixins, err := config.LoadMixins(m.Layout.SubscriptionMixinDir(s.Name)); err == nil {
			for _, mx := range mixins {
				e.Mixins = append(e.Mixins, mx.Name)
			}
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// Set 修改订阅的自动更新周期（period 为空时恢复默认值）。
func (m *Manager) Set(ctx context.Context, name, period string) error {
	if err := validatePeriod(period); err != nil {
		return err
	}
	lk, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer lk.Release()

	l, err := LoadList(m.Layout.SubscriptionList())
	if err != nil {
		return err
	}
	d := l.Find(name)
	if d == nil {
		return l.NotFound(name)
	}
	if err := checkSource(Source{URL: d.URL, Path: d.Path}, "", period); err != nil {
		return err
	}
	d.Update = period
	return SaveList(m.Layout.SubscriptionList(), l)
}

// checkSource 检查来源与其他设置是否相容：User-Agent 只用于地址；从标准输入导入的不能自动更新。
func checkSource(src Source, userAgent, period string) error {
	if src.URL == "" && userAgent != "" {
		return fmt.Errorf("%w: --user-agent applies only to a subscription URL", core.ErrInvalidArgument)
	}
	if src.Path == PathStdin && period != "" && period != settings.PeriodOff {
		return fmt.Errorf("%w: a subscription imported from standard input cannot be updated automatically", core.ErrInvalidArgument)
	}
	return nil
}

// validatePeriod 检查订阅的自动更新周期，空字符串（默认值）也合法。
func validatePeriod(period string) error {
	if period == "" {
		return nil
	}
	_, err := settings.ParsePeriod(period)
	return err
}

// retryAfter 是抓取失败后再次尝试的最长间隔：失败不必等满一个周期。
const retryAfter = 6 * time.Hour

// NextUpdate 返回订阅下次自动更新的时间：上次抓取（成功或失败，取较晚者；从未抓取过时为添加时间）
// 之后再过一个周期，上次失败时最多再过 retryAfter。不自动更新时返回零值；没有记录（手写进订阅列表）的
// 立即到期。
func NextUpdate(s *Subscription) time.Time {
	d, err := settings.ParsePeriod(s.UpdatePeriod())
	if err != nil || d == 0 {
		return time.Time{}
	}
	last := s.FetchedAt
	if s.LastErrorAt.After(last) {
		last = s.LastErrorAt
		d = min(d, retryAfter)
	}
	if last.IsZero() {
		last = s.AddedAt
	}
	return last.Add(d)
}

// Remove 删除一个订阅及其内容。不能删除正在启用的订阅。
func (m *Manager) Remove(ctx context.Context, name string) error {
	lk, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer lk.Release()

	l, st, err := m.load()
	if err != nil {
		return err
	}
	if l.Find(name) == nil {
		return l.NotFound(name)
	}
	if l.Active == name {
		return fmt.Errorf("%w: %s; switch to another subscription first (tunlr sub use)", ErrActive, name)
	}
	l.Subscriptions = slices.DeleteFunc(l.Subscriptions, func(d Def) bool { return d.Name == name })
	if err := m.save(l, st); err != nil {
		return err
	}
	if err := os.Remove(m.Layout.SubscriptionFile(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// 订阅 mixin 跟着订阅走。
	return os.RemoveAll(m.Layout.SubscriptionMixinDir(name))
}

// MixinFiles 返回订阅 name 的 mixin 文件数（mixin.d/<name>/*.yaml），用于删除前的提示。
func (m *Manager) MixinFiles(name string) (int, error) {
	mixins, err := config.LoadMixins(m.Layout.SubscriptionMixinDir(name))
	return len(mixins), err
}

// Update 重新抓取给定的订阅，names 为空时抓取全部。单个订阅失败时保留它上一次的内容并记录原因，
// 其余订阅照常更新；有任何失败时返回错误，结果中仍包含每个订阅的情况。
func (m *Manager) Update(ctx context.Context, names []string, opts UpdateOptions) ([]UpdateResult, error) {
	if opts.UserAgent != nil {
		if len(names) == 0 {
			return nil, fmt.Errorf("%w: name the subscriptions whose User-Agent should change", core.ErrInvalidArgument)
		}
		if err := settings.ValidateUserAgent(*opts.UserAgent); err != nil {
			return nil, err
		}
	}
	var src *Source
	if opts.Source != nil {
		if len(names) != 1 {
			return nil, fmt.Errorf("%w: --source changes one subscription; name it", core.ErrInvalidArgument)
		}
		s, err := ParseSource(*opts.Source)
		if err != nil {
			return nil, err
		}
		src = &s
	}
	lk, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer lk.Release()

	l, st, err := m.load()
	if err != nil {
		return nil, err
	}
	if len(l.Subscriptions) == 0 {
		return nil, fmt.Errorf("%w: no subscriptions added yet (tunlr sub add)", ErrNotFound)
	}
	var targets []*Def
	if len(names) == 0 {
		// 从标准输入导入的没有可以重读的来源，全部更新时跳过。
		for i := range l.Subscriptions {
			if l.Subscriptions[i].Path != PathStdin {
				targets = append(targets, &l.Subscriptions[i])
			}
		}
	}
	for _, name := range names {
		d := l.Find(name)
		if d == nil {
			return nil, l.NotFound(name)
		}
		cur := Source{URL: d.URL, Path: d.Path}
		if src != nil {
			cur = *src
		}
		if cur.URL == "" && opts.UserAgent != nil && *opts.UserAgent != "" {
			return nil, fmt.Errorf("%w: --user-agent applies only to a subscription URL", core.ErrInvalidArgument)
		}
		if cur.Path == PathStdin && opts.Stdin == nil {
			return nil, fmt.Errorf("%w: %s was %v; give it again with: tunlr sub update %s --source -", core.ErrInvalidArgument, d.Name, ErrNoSource, d.Name)
		}
		targets = append(targets, d)
	}

	results := make([]UpdateResult, 0, len(targets))
	failed := 0
	for _, d := range targets {
		s := Subscription{Def: *d, Fetch: st.Fetches[d.Name]}
		if opts.UserAgent != nil {
			s.UserAgent = *opts.UserAgent
		}
		if src != nil {
			// 换成文件或标准输入时不再需要 User-Agent；标准输入不能自动更新，周期恢复默认（off）。
			s.URL, s.Path = src.URL, src.Path
			if src.URL == "" {
				s.UserAgent = ""
			}
			if src.Path == PathStdin {
				s.Update = ""
			}
		}
		sum, err := m.fetchAndStore(ctx, &s, opts.Stdin)
		if err != nil {
			// 只记下失败，来源、User-Agent 和周期都不变。
			failed++
			s.LastError = err.Error()
			s.LastErrorAt = m.now().UTC()
		} else {
			*d = s.Def
		}
		st.Fetches[d.Name] = s.Fetch
		results = append(results, UpdateResult{Name: d.Name, Summary: sum, Err: err})
	}
	if err := m.save(l, st); err != nil {
		return results, err
	}
	if failed > 0 {
		return results, fmt.Errorf("%d of %d subscriptions failed to update", failed, len(targets))
	}
	return results, nil
}

// Use 启用给定的订阅。它从未成功抓取过时先抓取，失败则不切换。
//
// 切换只改变启用的订阅；生成并发布新配置由配置流水线完成。
func (m *Manager) Use(ctx context.Context, name string) (*Fetched, error) {
	lk, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer lk.Release()

	l, st, err := m.load()
	if err != nil {
		return nil, err
	}
	d := l.Find(name)
	if d == nil {
		return nil, l.NotFound(name)
	}
	res := &Fetched{Name: name, Activated: l.Active != name}
	data, err := os.ReadFile(m.Layout.SubscriptionFile(name))
	if err == nil {
		res.Summary, err = config.InspectSubscription(data)
	}
	if err != nil {
		s := Subscription{Def: *d, Fetch: st.Fetches[name]}
		if res.Summary, err = m.fetchAndStore(ctx, &s, nil); err != nil {
			return nil, fmt.Errorf("not switching to %s: %w", name, err)
		}
		st.Fetches[name] = s.Fetch
	}
	l.Active = name
	if err := m.save(l, st); err != nil {
		return nil, err
	}
	return res, nil
}

// fetchAndStore 抓取（地址）或读取（本地文件、stdin）订阅并检查内容，通过后原子写入内容文件，
// 并更新 s 中的抓取记录。失败时不改动已保存的内容。
func (m *Manager) fetchAndStore(ctx context.Context, s *Subscription, stdin io.Reader) (config.SubscriptionSummary, error) {
	var data []byte
	var info *state.UserInfo
	var err error
	if s.URL != "" {
		data, info, err = m.fetch(ctx, &s.Def)
	} else {
		data, err = read(&s.Def, stdin)
	}
	if err != nil {
		return config.SubscriptionSummary{}, err
	}
	sum, err := config.InspectSubscription(data)
	if errors.Is(err, config.ErrNotMapping) {
		if s.URL != "" {
			// 最常见的原因是机场没有识别出 User-Agent，返回了 base64 编码的节点列表。
			return sum, fmt.Errorf("%w; the provider may have returned a plain node list, try a different --user-agent", err)
		}
		return sum, fmt.Errorf("%w; a full configuration with proxies, proxy-groups and rules is expected", err)
	}
	if err != nil {
		return sum, err
	}
	// 内容没变时（定时更新的常见情况）不重写，省去两次刷盘。
	path := m.Layout.SubscriptionFile(s.Name)
	if old, err := os.ReadFile(path); err != nil || !bytes.Equal(old, data) {
		if err := os.MkdirAll(m.Layout.SubscriptionsDir(), 0o700); err != nil {
			return sum, err
		}
		if err := fsutil.WriteFile(path, data, 0o600); err != nil {
			return sum, err
		}
	}
	s.FetchedAt = m.now().UTC()
	s.UserInfo = info
	c := state.Counts(sum)
	s.Counts = &c
	s.LastError = ""
	s.LastErrorAt = time.Time{}
	return sum, nil
}

// load 读取订阅列表和状态文件（不存在时为新建的状态）。
func (m *Manager) load() (*List, *state.State, error) {
	l, err := LoadList(m.Layout.SubscriptionList())
	if err != nil {
		return nil, nil, err
	}
	st, _, err := state.LoadOrNew(m.Layout.StateFile(), m.now())
	if err != nil {
		return nil, nil, err
	}
	if st.Fetches == nil {
		st.Fetches = map[string]state.Fetch{}
	}
	return l, st, nil
}

// save 先写订阅列表再写状态文件，状态中去掉已不在列表中的订阅的记录（包括手动删掉的）。
func (m *Manager) save(l *List, st *state.State) error {
	if err := SaveList(m.Layout.SubscriptionList(), l); err != nil {
		return err
	}
	maps.DeleteFunc(st.Fetches, func(name string, _ state.Fetch) bool { return l.Find(name) == nil })
	return state.Save(m.Layout.StateFile(), st)
}

func (m *Manager) lock(ctx context.Context) (*lock.Lock, error) {
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	return lock.Acquire(ctx, m.Layout.LockFile())
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// ValidateName 检查订阅名称。名称也是 mixin.d 中的目录名，规则见 config.ValidMixinDir。
func ValidateName(name string) error {
	if !config.ValidMixinDir(name) {
		return fmt.Errorf("%w: subscription name %q must be 1-64 characters of letters, digits, '.', '_' or '-', starting with a letter or digit and not ending in .yaml",
			core.ErrInvalidArgument, name)
	}
	return nil
}

// ValidateURL 检查订阅地址是 http 或 https 地址。
func ValidateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: subscription URL must be an http or https URL", core.ErrInvalidArgument)
	}
	return nil
}
