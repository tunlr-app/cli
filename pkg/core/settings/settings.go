// Package settings 读取工具设置（/etc/tunlr/settings.yaml）。设置由用户编辑，
// 文件不存在时使用默认值。
package settings

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/tunlr-app/cli/pkg/core"
)

// Version 是设置文件当前的结构版本。手写的文件可以省略 version，视为当前版本。
const Version = 1

// Settings 是工具设置。
type Settings struct {
	Version int `yaml:"version"`
	// GitHubMirror 是 GitHub 下载镜像的前缀，例如 "https://gh-proxy.com/"，用于下载内核、
	// geo 数据和面板。为空时直连。无论经哪个镜像下载，都按官方 sha256 校验。
	GitHubMirror string `yaml:"github-mirror,omitempty"`
	// UserAgent 是抓取订阅时默认使用的 User-Agent，单个订阅设置了自己的 User-Agent 时以订阅为准。
	// 为空时使用内置默认值。
	UserAgent string `yaml:"user-agent,omitempty"`
}

// Load 读取设置文件。文件不存在时返回默认设置；未知的键视为错误，避免拼写错误被静默忽略。
func Load(path string) (*Settings, error) {
	s := &Settings{Version: Version}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(s); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("settings %s: %w", path, err)
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("settings %s: %w", path, err)
	}
	return s, nil
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
		u, err := url.Parse(s.GitHubMirror)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%w: github-mirror must be an http or https URL", core.ErrInvalidArgument)
		}
	}
	if err := ValidateUserAgent(s.UserAgent); err != nil {
		return fmt.Errorf("user-agent: %w", err)
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
