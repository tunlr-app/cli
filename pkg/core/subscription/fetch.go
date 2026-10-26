package subscription

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
)

const (
	// fetchTimeout 是单次抓取的总时限。
	fetchTimeout = 60 * time.Second
	// maxBody 限制订阅内容的大小。订阅是不可信输入，真实的完整配置通常在 1 MB 以内。
	maxBody = 32 << 20
)

// DefaultUserAgent 返回默认的 User-Agent。机场面板通常按 User-Agent 中的
// "clash.meta"、"mihomo" 等子串判断返回哪种格式，这里两者都带上。
func DefaultUserAgent(version string) string {
	return "clash.meta mihomo tunlr/" + version
}

// fetch 下载订阅内容并解析 subscription-userinfo 响应头。
// 返回的错误不含订阅 URL，因为 URL 中通常带有访问令牌。
func (m *Manager) fetch(ctx context.Context, s *state.Subscription) ([]byte, *state.UserInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, nil, errors.New("invalid subscription URL")
	}
	ua, err := m.userAgent(s)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", ua)

	client := m.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch %s: %w", Redact(s.URL), stripURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, nil, fmt.Errorf("fetch %s: HTTP %s", Redact(s.URL), resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, nil, fmt.Errorf("fetch %s: %w", Redact(s.URL), stripURL(err))
	}
	if len(data) > maxBody {
		return nil, nil, fmt.Errorf("fetch %s: response larger than %d MB", Redact(s.URL), maxBody>>20)
	}
	return data, parseUserInfo(resp.Header.Get("Subscription-Userinfo")), nil
}

// userAgent 决定抓取 s 时使用的 User-Agent：订阅自己的设置优先，其次是工具设置中的
// user-agent，最后是内置默认值。
func (m *Manager) userAgent(s *state.Subscription) (string, error) {
	if s.UserAgent != "" {
		return s.UserAgent, nil
	}
	st, err := settings.Load(m.Layout.SettingsFile())
	if err != nil {
		return "", err
	}
	if st.UserAgent != "" {
		return st.UserAgent, nil
	}
	return DefaultUserAgent(m.Version), nil
}

// stripURL 去掉 net/http 错误中附带的完整 URL，只保留底层原因。
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// Redact 把订阅 URL 缩写为 scheme://host/...，用于输出和日志。
func Redact(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "(invalid URL)"
	}
	return u.Scheme + "://" + u.Host + "/..."
}

// parseUserInfo 解析形如 "upload=1; download=2; total=3; expire=1700000000" 的响应头。
// 头不存在或没有可识别的字段时返回 nil。机场的写法并不统一，未知字段和无法解析的值一律忽略。
func parseUserInfo(header string) *state.UserInfo {
	var info state.UserInfo
	found := false
	for part := range strings.SplitSeq(header, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || f < 0 {
			continue
		}
		n := int64(f)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "upload":
			info.Upload = n
		case "download":
			info.Download = n
		case "total":
			info.Total = n
		case "expire":
			if n > 0 {
				info.Expire = time.Unix(n, 0).UTC()
			}
		default:
			continue
		}
		found = true
	}
	if !found {
		return nil
	}
	return &info
}
