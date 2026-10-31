package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// DNSMixinFile 是 SetSubscriptionDNS 在订阅的 mixin 目录中写入的文件名。
const DNSMixinFile = "20-dns.yaml"

// 订阅用谁的 DNS（tunlr sub set <订阅> --dns）。
const (
	// DNSFromSubscription 采用订阅写的 dns、sniffer（默认），订阅没写的用 tunlr 的兜底值。
	DNSFromSubscription = "subscription"
	// DNSFromTunlr 不采用订阅的 dns、sniffer，总用 tunlr 的兜底值。
	DNSFromTunlr = "tunlr"
)

// dnsIgnored 是 DNSFromTunlr 时不从订阅采用的键：tunlr 的嗅探是配合它的 DNS 调的，两者一起换。
var dnsIgnored = []string{"dns", "sniffer"}

// DNSWrite 是 SetSubscriptionDNS 写了什么、写到了哪里。
type DNSWrite struct {
	// From 是 DNSFromSubscription 或 DNSFromTunlr。
	From string `json:"from"`
	// File 是写入的 mixin 文件（mixin.d/<订阅>/DNSMixinFile），改完没有内容时已删除。
	File         string `json:"file"`
	Subscription string `json:"subscription"`
	// Active 报告订阅是否为启用的订阅：是时调用方随后发布配置，否则修改在它启用时生效。
	Active bool `json:"active"`
}

// SubscriptionDNS 返回订阅 sub 启用时用谁的 DNS：它的 mixin（共用的和它自己的）中有一个不采用订阅的 dns 时为
// DNSFromTunlr，并返回那个文件；否则为 DNSFromSubscription。
func (i *Installer) SubscriptionDNS(sub string) (from, file string, err error) {
	mixins, err := config.LoadMixinsFor(i.Layout.MixinDir(), sub)
	if err != nil {
		return "", "", err
	}
	for _, m := range mixins {
		keys, err := config.MixinIgnores(m.Name, m.Data)
		if err != nil {
			return "", "", err
		}
		if slices.Contains(keys, "dns") {
			return DNSFromTunlr, m.Name, nil
		}
	}
	return DNSFromSubscription, "", nil
}

// SetSubscriptionDNS 设置订阅 sub 用谁的 DNS：DNSFromTunlr 在它的 mixin（mixin.d/<sub>/DNSMixinFile）的
// tunlr.ignore-from-subscription 中列出 dns 和 sniffer，DNSFromSubscription 去掉它们（文件没有别的内容时删除）。
// 只在它启用时生效。别的 mixin 也列出了它们时，DNSFromSubscription 拒绝修改，以免改了不生效。只写文件，配置由
// 调用方随后发布。
func (i *Installer) SetSubscriptionDNS(ctx context.Context, sub, from string) (*DNSWrite, error) {
	if from != DNSFromSubscription && from != DNSFromTunlr {
		return nil, fmt.Errorf("%w: --dns takes %s or %s, got %q", core.ErrInvalidArgument, DNSFromTunlr, DNSFromSubscription, from)
	}
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	list, err := subscription.LoadList(i.Layout.SubscriptionList())
	if err != nil {
		return nil, err
	}
	if list.Find(sub) == nil {
		return nil, list.NotFound(sub)
	}
	own := sub + "/" + DNSMixinFile
	if from == DNSFromSubscription {
		mixins, err := config.LoadMixinsFor(i.Layout.MixinDir(), sub)
		if err != nil {
			return nil, err
		}
		for _, m := range mixins {
			if m.Name == own {
				continue
			}
			keys, err := config.MixinIgnores(m.Name, m.Data)
			if err != nil {
				return nil, err
			}
			if k := slices.IndexFunc(keys, func(k string) bool { return slices.Contains(dnsIgnored, k) }); k >= 0 {
				return nil, fmt.Errorf("%w: the mixin %s does not take %s from the subscription either; remove it there (tunlr config edit %s)",
					core.ErrInvalidArgument, m.Name, keys[k], strings.TrimSuffix(m.Name, ".yaml"))
			}
		}
	}

	path := config.MixinPath(i.Layout.MixinDir(), own)
	data, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var keep []string
	if existed {
		// 文件中列出的其他键（如手写的 hosts）保留。
		keys, err := config.MixinIgnores(own, data)
		if err != nil {
			return nil, err
		}
		keep = slices.DeleteFunc(keys, func(k string) bool { return slices.Contains(dnsIgnored, k) })
	}
	if from == DNSFromTunlr {
		keep = append(keep, dnsIgnored...)
	}
	var value any
	if len(keep) > 0 {
		value = keep
	}
	head := "DNS settings for subscription " + sub + ", applied only while it is active: the keys listed under\n" +
		"ignore-from-subscription are not taken from the subscription; tunlr's fallbacks are used instead\n" +
		"(tunlr:defaults, listed by tunlr config). Written by tunlr sub set " + sub + " --dns; you may edit it too."
	out, err := config.SetTunlrKeys(data, head, map[string]any{"ignore-from-subscription": value})
	if err != nil {
		return nil, fmt.Errorf("mixin %s: %w", own, err)
	}
	switch {
	case out == nil && existed:
		err = os.Remove(path)
	case out != nil:
		err = config.SaveMixin(i.Layout.MixinDir(), config.Mixin{Name: own, Data: out}, data, existed)
	}
	if err != nil {
		return nil, err
	}
	return &DNSWrite{From: from, File: path, Subscription: sub, Active: sub == list.Active}, nil
}

// PlanDNSMode 返回按 DNS 模式开关为 mode（state.DNSFakeIP、state.DNSRedirHost，空字符串为按配置）合成的计划
// （pipeline.PlanDNSMode），以及现在的开关。
func (i *Installer) PlanDNSMode(ctx context.Context, mode string) (before string, plan *pipeline.Plan, err error) {
	if mode != "" && mode != state.DNSFakeIP && mode != state.DNSRedirHost {
		return "", nil, fmt.Errorf("%w: unknown DNS mode %q (%s, %s or default)", core.ErrInvalidArgument, mode, state.DNSFakeIP, state.DNSRedirHost)
	}
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), i.now())
	if err != nil {
		return "", nil, err
	}
	plan, err = i.pipeline(nil).PlanDNSMode(ctx, mode)
	return st.DNSMode, plan, err
}

// SaveDNSMode 保存 DNS 模式开关，并发布 plan（PlanDNSMode 的结果，已经用户确认；为 nil 时只保存）。发布失败时
// 开关恢复原状，配置由流水线回滚。
func (i *Installer) SaveDNSMode(ctx context.Context, mode string, plan *pipeline.Plan) (*config.Revision, error) {
	return i.saveAndApply(ctx, plan, func(st *state.State) func() {
		previous := st.DNSMode
		st.DNSMode = mode
		return func() { st.DNSMode = previous }
	})
}
