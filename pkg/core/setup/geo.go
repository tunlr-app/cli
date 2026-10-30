package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// GeoDefault 表示把一项 geo 设置恢复为默认：从订阅的 geo mixin 中删除它，回到订阅写的值，订阅没有写时为兜底值。
const GeoDefault = "default"

// GeoDefaultUpdate 是兜底的 geo 数据更新周期（tunlr:defaults 中的 geo-update-interval: 168）。
const GeoDefaultUpdate = "7d"

// GeoMixinFile 是 SetGeo 在订阅的 mixin 目录中写入的文件名。
const GeoMixinFile = "20-geo.yaml"

// GeoURLKeys 是 geox-url 中的键，与内核相同。
var GeoURLKeys = []string{"geoip", "geosite", "mmdb", "asn"}

// GeoChange 是对一个订阅的 geo 数据设置的修改（tunlr sub set <订阅> --geo-update、--geo-url）。
type GeoChange struct {
	// Update 是内核更新 geo 数据的周期，写法同订阅（off 为不更新），GeoDefault 恢复默认；为空时不改。
	Update string
	// URLs 是要改的下载地址，键为 GeoURLKeys 之一，值为 GeoDefault 时恢复默认。
	URLs map[string]string
}

// GeoWrite 是 SetGeo 写了什么、写到了哪里。
type GeoWrite struct {
	// Keys 是改了的配置中的键（config.GeoKeys 中的）。
	Keys []string `json:"keys"`
	// File 是写入的 mixin 文件（mixin.d/<订阅>/GeoMixinFile），改完没有内容时已删除。
	File         string `json:"file"`
	Subscription string `json:"subscription"`
	// Active 报告订阅是否为启用的订阅：是时调用方随后发布配置，否则修改在它启用时生效。
	Active bool `json:"active"`
}

// SetGeo 修改订阅 sub 的 geo 数据设置：写进它的 mixin（mixin.d/<sub>/GeoMixinFile）的 mihomo:，覆盖订阅写的值，
// 只在它启用时生效；没有写的键仍按订阅、兜底值（tunlr:defaults）的顺序取值。同一目录中排在这个文件之后的
// mixin 写了的键拒绝修改，以免改了不生效。周期对应 geo-auto-update 和 geo-update-interval 两个键。
// 只写文件，配置由调用方随后发布。
func (i *Installer) SetGeo(ctx context.Context, sub string, ch GeoChange) (*GeoWrite, error) {
	if ch.Update != "" && ch.Update != GeoDefault {
		if _, err := settings.ParsePeriod(ch.Update); err != nil {
			return nil, err
		}
	}
	for k, u := range ch.URLs {
		if !slices.Contains(GeoURLKeys, k) {
			return nil, fmt.Errorf("%w: unknown geo file %q: use %s", core.ErrInvalidArgument, k, strings.Join(GeoURLKeys, ", "))
		}
		if u != GeoDefault {
			if err := ValidateGeoURL(u); err != nil {
				return nil, err
			}
		}
	}
	if ch.Update == "" && len(ch.URLs) == 0 {
		return nil, fmt.Errorf("%w: nothing to change", core.ErrInvalidArgument)
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
		return nil, fmt.Errorf("%w: %s", subscription.ErrNotFound, sub)
	}
	sources, err := i.GeoSources(ctx, sub)
	if err != nil {
		return nil, err
	}
	own := sub + "/" + GeoMixinFile
	from := map[string]config.KeySource{}
	for _, s := range sources {
		from[s.Path] = s
	}
	// 订阅的 mixin 在共用的之后、按文件名应用，排在 own 之后的会压过它。
	check := func(key string) error {
		s := from[key]
		if strings.HasPrefix(s.From, sub+"/") && s.From > own {
			return fmt.Errorf("%w: %s is set by the mixin %s, which is applied after %s; change it there (tunlr config edit %s)",
				core.ErrInvalidArgument, key, s.From, own, strings.TrimSuffix(s.From, ".yaml"))
		}
		return nil
	}

	values := map[string]any{}
	w := &GeoWrite{File: config.MixinPath(i.Layout.MixinDir(), own), Subscription: sub, Active: sub == list.Active}
	if ch.Update != "" {
		var auto, interval any
		if ch.Update != GeoDefault {
			d, _ := settings.ParsePeriod(ch.Update)
			auto = d > 0
			if d > 0 {
				interval = int(d / time.Hour)
			}
		}
		values["geo-auto-update"], values["geo-update-interval"] = auto, interval
		w.Keys = append(w.Keys, "geo-auto-update", "geo-update-interval")
	}
	for _, k := range GeoURLKeys {
		if u, ok := ch.URLs[k]; ok {
			key := "geox-url." + k
			values[key] = nil
			if u != GeoDefault {
				values[key] = u
			}
			w.Keys = append(w.Keys, key)
		}
	}
	for _, k := range w.Keys {
		if err := check(k); err != nil {
			return nil, err
		}
	}
	head := "geo data settings for subscription " + sub + ", applied only while it is active: they override\n" +
		"what the subscription sets; what is not here comes from the subscription, or else from tunlr's fallbacks\n" +
		"(tunlr:defaults, listed by tunlr config). Written by tunlr sub set " + sub + " --geo-update, --geo-url; you may edit it too."
	if err := writeGeoMixin(i.Layout.MixinDir(), own, head, values); err != nil {
		return nil, err
	}
	return w, nil
}

// writeGeoMixin 在订阅的 geo mixin name 的 mihomo: 中设置 values（见 config.SetMixinKeys），新文件以 head 为开头的
// 注释；改完没有内容时删除文件。
func writeGeoMixin(dir, name, head string, values map[string]any) error {
	path := config.MixinPath(dir, name)
	data, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := config.SetMixinKeys(data, head, values)
	if err != nil {
		return fmt.Errorf("mixin %s: %w", name, err)
	}
	if out == nil {
		if existed {
			return os.Remove(path)
		}
		return nil
	}
	return config.SaveMixin(dir, config.Mixin{Name: name, Data: out}, data, existed)
}

// GeoPeriod 返回 sources（GeoSources 的结果）中内核更新 geo 数据的周期，写法同订阅的周期（off 为不更新）。
func GeoPeriod(sources []config.KeySource) string {
	auto, interval := false, 24*time.Hour // 配置中没有写时内核的默认值
	for _, s := range sources {
		switch s.Path {
		case "geo-auto-update":
			auto = strings.EqualFold(s.Value, "true")
		case "geo-update-interval":
			if h, err := strconv.Atoi(s.Value); err == nil && h > 0 {
				interval = time.Duration(h) * time.Hour
			}
		}
	}
	if !auto {
		return settings.PeriodOff
	}
	return periodOf(interval)
}

// ValidateGeoURL 检查 geo 数据的下载地址：必须是 http 或 https 地址。
func ValidateGeoURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: %q is not an http or https URL", core.ErrInvalidArgument, s)
	}
	return nil
}
