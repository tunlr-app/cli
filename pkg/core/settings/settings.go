// Package settings 读取和修改工具设置（/etc/tunlr/settings.yaml）。设置由用户编辑，
// 文件不存在时使用默认值。
package settings

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

// Version 是设置文件当前的结构版本。手写的文件可以省略 version，视为当前版本。
const Version = 1

// Settings 是工具设置。
type Settings struct {
	Version int `yaml:"version"`
	// GitHubMirror 是 GitHub 下载镜像的前缀，例如 "https://gh-proxy.com/"，用于下载内核（按官方 sha256 校验）；
	// 配置中 external-ui-url、geox-url 的 GitHub 地址也改为经它下载（内核自己下载）。为空时直连。
	GitHubMirror string `yaml:"github-mirror,omitempty"`
	// UserAgent 是抓取订阅时默认使用的 User-Agent，单个订阅设置了自己的 User-Agent 时以订阅为准。
	// 为空时使用内置默认值。
	UserAgent string `yaml:"user-agent,omitempty"`
}

// PeriodOff 表示不定时更新。
const PeriodOff = "off"

// SuggestedMirror 是 init 提问时预填的 GitHub 镜像（还没有设置时），与 install.sh 的默认值相同。
const SuggestedMirror = "https://gh-proxy.com/"

// maxPeriod 是更新周期的上限，防止写错单位（如把小时写成天）后几乎永不更新而不自知。
const maxPeriod = 365 * 24 * time.Hour

// ParsePeriod 解析更新周期：正整数加单位 h（小时）或 d（天），至少 1 小时，最多 365 天；"off" 返回 0。
func ParsePeriod(s string) (time.Duration, error) {
	if s == PeriodOff {
		return 0, nil
	}
	if len(s) >= 2 {
		n, err := strconv.Atoi(s[:len(s)-1])
		unit := map[byte]time.Duration{'h': time.Hour, 'd': 24 * time.Hour}[s[len(s)-1]]
		if err == nil && n >= 1 && unit != 0 && s[0] != '+' && time.Duration(n) <= maxPeriod/unit {
			return time.Duration(n) * unit, nil
		}
	}
	return 0, fmt.Errorf("%w: invalid update period %q: use a number of hours or days such as 6h, 1d or 7d (at most 365d), or off", core.ErrInvalidArgument, s)
}

// DescribePeriod 把周期设置写成给人看的说法，如 "every 6 hours"、"daily"、"weekly"、"never"。
func DescribePeriod(p string) string {
	d, err := ParsePeriod(p)
	switch {
	case err != nil:
		return p
	case d == 0:
		return "never"
	case d == time.Hour:
		return "hourly"
	case d == 24*time.Hour:
		return "daily"
	case d == 7*24*time.Hour:
		return "weekly"
	case p[len(p)-1] == 'h':
		return "every " + p[:len(p)-1] + " hours"
	}
	return "every " + p[:len(p)-1] + " days"
}

// Load 读取设置文件。文件不存在时返回默认设置；未知的键视为错误，避免拼写错误被静默忽略。
func Load(path string) (*Settings, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Settings{Version: Version}, nil
	}
	if err != nil {
		return nil, err
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("settings %s: %w", path, err)
	}
	return s, nil
}

// Parse 解析并校验设置文件的内容。只有注释或为空时是默认设置。
func Parse(data []byte) (*Settings, error) {
	s := &Settings{Version: Version}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(s); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// ErrChanged 表示设置文件在编辑期间被改动了。
var ErrChanged = errors.New("the settings file changed while you were editing it")

// Write 把编辑后的内容 data 校验后原子写入设置文件。previous、existed 是开始编辑时文件的内容和是否存在；
// 文件此后被改动过时不覆盖，返回 ErrChanged。
func Write(path string, data, previous []byte, existed bool) error {
	if _, err := Parse(data); err != nil {
		return fmt.Errorf("%w: %w", core.ErrInvalidArgument, err)
	}
	current, err := os.ReadFile(path)
	switch {
	case err == nil && (!existed || !bytes.Equal(current, previous)):
		return ErrChanged
	case errors.Is(err, fs.ErrNotExist) && existed:
		return ErrChanged
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.WriteFile(path, data, 0o644)
}

// Restore 把设置文件恢复为 original；existed 为 false 时删除它。
func Restore(path string, original []byte, existed bool) error {
	if !existed {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return fsutil.WriteFile(path, original, 0o644)
}

// Entry 是 tunlr settings 列出的一项设置。
type Entry struct {
	Key string `json:"key"`
	// Value 是生效的值；Default 报告文件中没有设置，Value 为默认值（github-mirror 的默认值为空：直连）。
	Value   string `json:"value"`
	Default bool   `json:"default"`
	// Help 说明这项设置的作用和何时生效。
	Help string `json:"help"`
}

// Entries 列出全部设置，按设置文件模板中的顺序。defaultUA 是内置的 User-Agent。
func (s *Settings) Entries(defaultUA string) []Entry {
	return []Entry{
		{Key: "github-mirror", Value: s.GitHubMirror, Default: s.GitHubMirror == "",
			Help: "GitHub mirror for downloading mihomo (checked against the official sha256); the GitHub addresses mihomo downloads the panel and geo data from go through it too, published with the configuration"},
		{Key: "user-agent", Value: cmp.Or(s.UserAgent, defaultUA), Default: s.UserAgent == "",
			Help: "User-Agent for fetching subscriptions without their own; used from the next fetch"},
	}
}

func (s *Settings) validate() error {
	if s.Version == 0 {
		s.Version = Version
	}
	if s.Version > Version {
		return fmt.Errorf("version %d is newer than this tunlr supports (%d); upgrade tunlr", s.Version, Version)
	}
	if s.Version < 0 {
		return fmt.Errorf("%w: invalid version %d", core.ErrInvalidArgument, s.Version)
	}
	if s.GitHubMirror != "" {
		if err := ValidateMirror(s.GitHubMirror); err != nil {
			return err
		}
	}
	if err := ValidateUserAgent(s.UserAgent); err != nil {
		return fmt.Errorf("user-agent: %w", err)
	}
	return nil
}

// Set 把设置文件中的 key 设为 value（为空时删除该键），保留其他键和注释，校验通过后原子写入。
// 文件不存在时创建。
func Set(path, key, value string) error {
	var doc yaml.Node
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("settings %s: %w", path, err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return fmt.Errorf("settings %s: not a mapping", path)
	}
	// 删光键后文件是 "{}"，再读回来是流式写法；总是按块写法写出，一行一个键。
	m.Style = 0
	found := false
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		found = true
		if value == "" {
			m.Content = slices.Delete(m.Content, i, i+2)
		} else {
			m.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
		}
		break
	}
	if !found && value != "" {
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(buf.Bytes()))
	dec.KnownFields(true)
	s := &Settings{Version: Version}
	if err := dec.Decode(s); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %v", core.ErrInvalidArgument, err)
	}
	if err := s.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.WriteFile(path, buf.Bytes(), 0o644)
}

// ValidateMirror 检查 GitHub 镜像前缀：必须是 http 或 https 地址。
func ValidateMirror(mirror string) error {
	u, err := url.Parse(mirror)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: github-mirror must be an http or https URL", core.ErrInvalidArgument)
	}
	return nil
}

// ValidateUserAgent 检查 User-Agent 能否作为 HTTP 头发送：不能含控制字符（包括换行）。
// 空字符串表示使用默认值，是合法的。
func ValidateUserAgent(ua string) error {
	if strings.TrimSpace(ua) != ua {
		return fmt.Errorf("%w: User-Agent must not start or end with whitespace", core.ErrInvalidArgument)
	}
	for _, r := range ua {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: User-Agent must not contain control characters", core.ErrInvalidArgument)
		}
	}
	return nil
}
