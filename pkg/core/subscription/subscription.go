// Package subscription 管理订阅：保存多个机场订阅，同一时间只启用一个。
// 订阅内容由工具抓取，每个订阅保留最近一次成功抓取的结果，抓取失败时继续使用它。
package subscription

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/lock"
	"github.com/tunlr-app/cli/pkg/core/paths"
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

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

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

// Entry 是 List 返回的一个订阅。
type Entry struct {
	state.Subscription
	Active bool
	// Summary 统计已保存的内容；内容缺失或无法解析时为零值。
	Summary config.SubscriptionSummary
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

// Add 抓取并保存一个新订阅。抓取或检查失败时什么都不保存。
// 这是第一个订阅时自动启用它。userAgent 为空时使用默认值。
func (m *Manager) Add(ctx context.Context, name, rawURL, userAgent string) (*Fetched, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := validateURL(rawURL); err != nil {
		return nil, err
	}
	l, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()

	st, _, err := state.LoadOrNew(m.Layout.StateFile(), m.now())
	if err != nil {
		return nil, err
	}
	if st.Find(name) != nil {
		return nil, fmt.Errorf("%w: %s", ErrExists, name)
	}
	sub := state.Subscription{Name: name, URL: rawURL, UserAgent: userAgent, AddedAt: m.now().UTC()}
	sum, err := m.fetchAndStore(ctx, &sub)
	if err != nil {
		return nil, err
	}
	st.Subscriptions = append(st.Subscriptions, sub)
	res := &Fetched{Name: name, Summary: sum}
	if st.Active == "" {
		st.Active = name
		res.Activated = true
	}
	if err := state.Save(m.Layout.StateFile(), st); err != nil {
		return nil, err
	}
	return res, nil
}

// List 返回已保存的订阅，按添加顺序排列。尚未安装时返回空列表。
func (m *Manager) List() ([]Entry, error) {
	st, err := state.Load(m.Layout.StateFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(st.Subscriptions))
	for _, s := range st.Subscriptions {
		e := Entry{Subscription: s, Active: s.Name == st.Active}
		if data, err := os.ReadFile(m.Layout.SubscriptionFile(s.Name)); err == nil {
			e.Summary, _ = config.InspectSubscription(data)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// Remove 删除一个订阅及其内容。不能删除正在启用的订阅。
func (m *Manager) Remove(ctx context.Context, name string) error {
	l, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer l.Release()

	st, err := m.loadExisting()
	if err != nil {
		return err
	}
	if st.Find(name) == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if st.Active == name {
		return fmt.Errorf("%w: %s; switch to another subscription first (tunlr sub use)", ErrActive, name)
	}
	subs := st.Subscriptions[:0]
	for _, s := range st.Subscriptions {
		if s.Name != name {
			subs = append(subs, s)
		}
	}
	st.Subscriptions = subs
	if err := state.Save(m.Layout.StateFile(), st); err != nil {
		return err
	}
	if err := os.Remove(m.Layout.SubscriptionFile(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Update 重新抓取给定的订阅，names 为空时抓取全部。单个订阅失败时保留它上一次的内容并记录原因，
// 其余订阅照常更新；有任何失败时返回错误，结果中仍包含每个订阅的情况。
func (m *Manager) Update(ctx context.Context, names []string) ([]UpdateResult, error) {
	l, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()

	st, err := m.loadExisting()
	if err != nil {
		return nil, err
	}
	var targets []*state.Subscription
	if len(names) == 0 {
		for i := range st.Subscriptions {
			targets = append(targets, &st.Subscriptions[i])
		}
	}
	for _, name := range names {
		s := st.Find(name)
		if s == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		targets = append(targets, s)
	}

	results := make([]UpdateResult, 0, len(targets))
	failed := 0
	for _, s := range targets {
		sum, err := m.fetchAndStore(ctx, s)
		if err != nil {
			failed++
			s.LastError = err.Error()
			s.LastErrorAt = m.now().UTC()
		}
		results = append(results, UpdateResult{Name: s.Name, Summary: sum, Err: err})
	}
	if err := state.Save(m.Layout.StateFile(), st); err != nil {
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
	l, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()

	st, err := m.loadExisting()
	if err != nil {
		return nil, err
	}
	s := st.Find(name)
	if s == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	res := &Fetched{Name: name, Activated: st.Active != name}
	data, err := os.ReadFile(m.Layout.SubscriptionFile(name))
	if err == nil {
		res.Summary, err = config.InspectSubscription(data)
	}
	if err != nil {
		if res.Summary, err = m.fetchAndStore(ctx, s); err != nil {
			return nil, fmt.Errorf("not switching to %s: %w", name, err)
		}
	}
	st.Active = name
	if err := state.Save(m.Layout.StateFile(), st); err != nil {
		return nil, err
	}
	return res, nil
}

// fetchAndStore 抓取订阅并检查内容，通过后原子写入内容文件，并更新 s 中的抓取信息。
// 失败时不改动已保存的内容。
func (m *Manager) fetchAndStore(ctx context.Context, s *state.Subscription) (config.SubscriptionSummary, error) {
	data, info, err := m.fetch(ctx, s)
	if err != nil {
		return config.SubscriptionSummary{}, err
	}
	sum, err := config.InspectSubscription(data)
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
	s.LastError = ""
	s.LastErrorAt = time.Time{}
	return sum, nil
}

// loadExisting 读取状态文件；尚未添加过订阅时报告 ErrNotFound。
func (m *Manager) loadExisting() (*state.State, error) {
	st, err := state.Load(m.Layout.StateFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: no subscriptions added yet (tunlr sub add)", ErrNotFound)
	}
	return st, err
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

func validateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: subscription name %q must be 1-64 characters of letters, digits, '.', '_' or '-', starting with a letter or digit",
			core.ErrInvalidArgument, name)
	}
	return nil
}

func validateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: subscription URL must be an http or https URL", core.ErrInvalidArgument)
	}
	return nil
}
