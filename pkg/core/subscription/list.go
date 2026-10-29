package subscription

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/settings"
)

// ListVersion 是订阅列表当前的结构版本。手写的文件可以省略 version，视为当前版本。
const ListVersion = 1

// DefaultUpdate 是地址订阅自动更新的默认周期。本地文件只会被用户自己改，默认不自动更新。
const DefaultUpdate = "1d"

// PathStdin 是从标准输入导入的订阅的 Path。
const PathStdin = "-"

// List 是订阅列表（/etc/tunlr/subscriptions.yaml）：用户的意图，由 tunlr sub 修改，也可以手改。
// 抓取记录是工具观察到的情况，在状态文件中（state.Fetch）。
type List struct {
	Version int `yaml:"version"`
	// Active 是启用的订阅名称；同一时间只启用一个，没有订阅时为空。
	Active string `yaml:"active,omitempty"`
	// Subscriptions 按添加顺序排列。
	Subscriptions []Def `yaml:"subscriptions,omitempty"`
}

// Def 是订阅列表中的一个订阅。
type Def struct {
	Name string `yaml:"name"`
	// URL 通常带有机场的访问令牌，不能出现在输出和日志中。与 Path 二选一。
	URL string `yaml:"url,omitempty"`
	// Path 是本地文件订阅的绝对路径，更新时重新读取；为 PathStdin 时是从标准输入导入的，没有可以重读的来源。
	Path string `yaml:"path,omitempty"`
	// UserAgent 为空时使用默认值。机场按 User-Agent 决定返回的格式。
	UserAgent string `yaml:"user-agent,omitempty"`
	// Update 是自动更新的周期（如 "6h"、"1d"，"off" 为不自动更新），为空时为默认值（见 DefaultPeriod）。
	// 不论是否启用都按它更新；只有启用的订阅更新后才发布配置。
	Update string `yaml:"update,omitempty"`
}

// DefaultPeriod 返回订阅自动更新的默认周期：地址为 DefaultUpdate，本地文件和标准输入为 "off"。
func (d *Def) DefaultPeriod() string {
	if d.URL == "" {
		return settings.PeriodOff
	}
	return DefaultUpdate
}

// UpdatePeriod 返回订阅的自动更新周期（没有设置时为默认值）。从标准输入导入的总是 "off"。
func (d *Def) UpdatePeriod() string {
	if d.Update == "" || d.Path == PathStdin {
		return d.DefaultPeriod()
	}
	return d.Update
}

// Find 返回名为 name 的订阅，不存在时返回 nil。
func (l *List) Find(name string) *Def {
	for i := range l.Subscriptions {
		if l.Subscriptions[i].Name == name {
			return &l.Subscriptions[i]
		}
	}
	return nil
}

// LoadList 读取订阅列表。文件不存在时返回空列表。
func LoadList(path string) (*List, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &List{Version: ListVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	l, err := ParseList(data)
	if err != nil {
		return nil, fmt.Errorf("subscription list %s: %w", path, err)
	}
	return l, nil
}

// ParseList 解析并校验订阅列表。未知的键视为错误，避免拼写错误被静默忽略；错误中不含订阅地址。
func ParseList(data []byte) (*List, error) {
	l := &List{Version: ListVersion}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(l); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := l.validate(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *List) validate() error {
	if l.Version == 0 {
		l.Version = ListVersion
	}
	if l.Version > ListVersion {
		return fmt.Errorf("version %d is newer than this tunlr supports (%d); upgrade tunlr", l.Version, ListVersion)
	}
	if l.Version < 0 {
		return fmt.Errorf("%w: invalid version %d", core.ErrInvalidArgument, l.Version)
	}
	seen := map[string]bool{}
	for i := range l.Subscriptions {
		d := &l.Subscriptions[i]
		if err := ValidateName(d.Name); err != nil {
			return err
		}
		if seen[d.Name] {
			return fmt.Errorf("%w: subscription %q is listed twice", core.ErrInvalidArgument, d.Name)
		}
		seen[d.Name] = true
		if err := d.validate(); err != nil {
			return fmt.Errorf("subscription %q: %w", d.Name, err)
		}
	}
	if l.Active != "" && !seen[l.Active] {
		return fmt.Errorf("%w: the active subscription %q is not in the list", core.ErrInvalidArgument, l.Active)
	}
	return nil
}

func (d *Def) validate() error {
	switch {
	case (d.URL == "") == (d.Path == ""):
		return fmt.Errorf("%w: give exactly one of url and path", core.ErrInvalidArgument)
	case d.URL != "":
		if err := ValidateURL(d.URL); err != nil {
			return err
		}
	case d.Path != PathStdin && !filepath.IsAbs(d.Path):
		return fmt.Errorf("%w: path must be absolute", core.ErrInvalidArgument)
	}
	if err := settings.ValidateUserAgent(d.UserAgent); err != nil {
		return fmt.Errorf("user-agent: %w", err)
	}
	if err := validatePeriod(d.Update); err != nil {
		return err
	}
	return checkSource(Source{URL: d.URL, Path: d.Path}, d.UserAgent, d.Update)
}

// listHeader 是新建的订阅列表开头的说明。
const listHeader = `# tunlr 的订阅列表：各订阅的来源（url 或 path）、User-Agent、更新周期（update），以及启用的订阅（active）。
# 由 tunlr sub 修改（add、use、update、remove）；也可以直接改，下次抓取或 tunlr config apply 时生效。
# 抓取到的内容和抓取记录由工具保存在 /var/lib/tunlr 中。
`

// SaveList 校验后原子写入订阅列表（含访问令牌，权限 0600）。文件已存在时在原来的内容上只改动变化的部分，
// 保留用户写的注释和键的顺序。
func SaveList(path string, l *List) error {
	if err := l.validate(); err != nil {
		return err
	}
	var fresh yaml.Node
	if err := fresh.Encode(l); err != nil {
		return err
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, HeadComment: listHeader, Content: []*yaml.Node{&fresh}}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var old yaml.Node
	if err == nil && yaml.Unmarshal(data, &old) == nil && old.Kind == yaml.DocumentNode && len(old.Content) == 1 &&
		old.Content[0].Kind == yaml.MappingNode {
		syncMapping(old.Content[0], &fresh)
		doc = &old
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.WriteFile(path, buf.Bytes(), 0o600)
}

// syncMapping 把映射 dst 改成与 src 相同的键和值：已有的键保留位置和注释，只换值；新的键加在最后；
// src 中没有的键删去。值为列表时按项中的 name 对应（见 syncSequence）。
func syncMapping(dst, src *yaml.Node) {
	dst.Style = 0
	var content []*yaml.Node
	for i := 0; i+1 < len(dst.Content); i += 2 {
		k, v := dst.Content[i], dst.Content[i+1]
		nv := value(src, k.Value)
		if nv == nil {
			continue
		}
		switch {
		case v.Kind == yaml.SequenceNode && nv.Kind == yaml.SequenceNode:
			syncSequence(v, nv)
		case v.Kind == yaml.MappingNode && nv.Kind == yaml.MappingNode:
			syncMapping(v, nv)
		default:
			v.Kind, v.Tag, v.Value, v.Style, v.Content = nv.Kind, nv.Tag, nv.Value, nv.Style, nv.Content
		}
		content = append(content, k, v)
	}
	for i := 0; i+1 < len(src.Content); i += 2 {
		if value(dst, src.Content[i].Value) == nil {
			content = append(content, src.Content[i], src.Content[i+1])
		}
	}
	dst.Content = content
}

// syncSequence 把列表 dst 改成 src 的样子：按项中 name 的值对应，对应上的项保留注释，只同步内容。
func syncSequence(dst, src *yaml.Node) {
	dst.Style = 0
	content := make([]*yaml.Node, 0, len(src.Content))
	for _, item := range src.Content {
		name := value(item, "name")
		k := -1
		if name != nil {
			k = slices.IndexFunc(dst.Content, func(n *yaml.Node) bool {
				v := value(n, "name")
				return n.Kind == yaml.MappingNode && v != nil && v.Value == name.Value
			})
		}
		if k < 0 {
			content = append(content, item)
			continue
		}
		syncMapping(dst.Content[k], item)
		content = append(content, dst.Content[k])
	}
	dst.Content = content
}

// value 返回映射 m 中键 key 的值，没有时返回 nil。
func value(m *yaml.Node, key string) *yaml.Node {
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
