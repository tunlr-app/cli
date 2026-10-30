// Package backup 定义备份文件的格式：一个 tar.gz，含说明与状态的 backup.json、工具设置、订阅列表、mixin
// 和订阅最近一次抓取的内容。不含内核、geo 数据、配置历史，以及实例 ID、抓取记录等只对本机有意义的状态。
package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// Version 是备份格式当前的版本。结构变化时递增。版本 2 改为打包订阅列表（subscriptions.yaml），
// 不再读取版本 1。
const Version = 2

// 归档中的文件。
const (
	manifestName    = "backup.json"
	settingsName    = "settings.yaml"
	listName        = "subscriptions.yaml"
	mixinDir        = "mixin.d/"
	subscriptionDir = "subscriptions/"
	contentExt      = ".yaml"
)

// 读取时的大小上限，避免损坏或恶意的文件耗尽内存。
const (
	maxFile  = 64 << 20
	maxTotal = 256 << 20
)

// Backup 是一份备份的内容。
type Backup struct {
	Version int `json:"version"`
	// Tunlr 是生成备份的 tunlr 版本，Host 是主机名，只用于展示。
	Tunlr     string    `json:"tunlr"`
	Host      string    `json:"host"`
	CreatedAt time.Time `json:"created_at"`

	// 以下取自状态文件，含义同 state.State 的同名字段。
	APISecret string         `json:"api_secret,omitempty"`
	Devices   []state.Device `json:"devices,omitempty"`
	Filter    state.Filter   `json:"filter,omitzero"`
	// Switches 记录备份时开着的开关。还原时不直接打开它们，由用户决定是否在新机器上走各自的流程。
	Switches Switches `json:"switches"`

	// Settings 是设置文件的内容，没有设置文件时为 nil。
	Settings []byte `json:"-"`
	// List 是订阅列表文件的内容（订阅地址带有机场的访问令牌），没有时为 nil；Subscriptions 是解析的结果，
	// 读取备份时总不为 nil。
	List          []byte             `json:"-"`
	Subscriptions *subscription.List `json:"-"`
	// Mixins 是 mixin.d 中的文件，订阅 mixin 的名称带订阅名（如 home/10-filter.yaml），顺序同 config.LoadAllMixins。
	Mixins []config.Mixin `json:"-"`
	// Contents 是订阅最近一次抓取的内容，键为订阅名称；从未抓取成功的订阅没有。
	Contents map[string][]byte `json:"-"`
}

// Switches 是备份时开着的开关。
type Switches struct {
	Tun bool `json:"tun,omitempty"`
	// Gateway 是网关模式的局域网网卡，关闭时为空。
	Gateway  string `json:"gateway,omitempty"`
	ProxyLAN bool   `json:"proxy_lan,omitempty"`
	PanelLAN bool   `json:"panel_lan,omitempty"`
	// LANInterfaces、LANContainers 是代理端口和面板放行的入口（见 state.LANAccess），选过时才有。
	LANInterfaces []string `json:"lan_interfaces,omitempty"`
	LANContainers bool     `json:"lan_containers,omitempty"`
}

// Any 报告是否有开着的开关。
func (s Switches) Any() bool { return s.Tun || s.Gateway != "" || s.ProxyLAN || s.PanelLAN }

// Write 把备份写成 tar.gz。
func Write(w io.Writer, b *Backup) error {
	manifest, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	mtime := b.CreatedAt
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: mtime, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := add(manifestName, append(manifest, '\n')); err != nil {
		return err
	}
	if b.Settings != nil {
		if err := add(settingsName, b.Settings); err != nil {
			return err
		}
	}
	if b.List != nil {
		if err := add(listName, b.List); err != nil {
			return err
		}
	}
	for _, m := range b.Mixins {
		if err := add(mixinDir+m.Name, m.Data); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(b.Contents))
	for name := range b.Contents {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := add(subscriptionDir+name+contentExt, b.Contents[name]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// WriteFile 把备份写入新文件 path（0600，含机场凭据）。文件已存在时不覆盖。
func WriteFile(path string, b *Backup) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(path)
		}
	}()
	if err := Write(f, b); err != nil {
		return err
	}
	return f.Sync()
}

// ErrInvalid 表示不是有效的 tunlr 备份，满足 errors.Is(err, core.ErrInvalidArgument)。
var ErrInvalid = fmt.Errorf("%w: not a valid tunlr backup", core.ErrInvalidArgument)

// Read 读取并检查备份：格式版本、各文件的名称、设置文件、订阅列表、订阅与内容的对应关系。mixin 的内容在还原前
// 合成配置时检查。
func Read(r io.Reader) (*Backup, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	var order []string
	total := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%w: unexpected entry %q", ErrInvalid, h.Name)
		}
		if _, dup := files[h.Name]; dup {
			return nil, fmt.Errorf("%w: %s appears twice", ErrInvalid, h.Name)
		}
		if h.Size > maxFile || total+int(h.Size) > maxTotal {
			return nil, fmt.Errorf("%w: %s is too large", ErrInvalid, h.Name)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxFile+1))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		total += len(data)
		files[h.Name] = data
		order = append(order, h.Name)
	}

	manifest, ok := files[manifestName]
	if !ok {
		return nil, fmt.Errorf("%w: %s is missing", ErrInvalid, manifestName)
	}
	b, err := decode(manifest)
	if err != nil {
		return nil, err
	}
	b.Subscriptions = &subscription.List{Version: subscription.ListVersion}
	if data, ok := files[listName]; ok {
		if b.Subscriptions, err = subscription.ParseList(data); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrInvalid, listName, err)
		}
		b.List = data
	}
	b.Contents = map[string][]byte{}
	for _, name := range order {
		data := files[name]
		switch {
		case name == manifestName, name == listName:
		case name == settingsName:
			if _, err := settings.Parse(data); err != nil {
				return nil, fmt.Errorf("%w: settings: %v", ErrInvalid, err)
			}
			b.Settings = data
		case strings.HasPrefix(name, mixinDir):
			base := strings.TrimPrefix(name, mixinDir)
			if fn, err := config.MixinFileName(base); err != nil || fn != base {
				return nil, fmt.Errorf("%w: invalid mixin file name %q", ErrInvalid, base)
			}
			b.Mixins = append(b.Mixins, config.Mixin{Name: base, Data: data})
		case strings.HasPrefix(name, subscriptionDir) && path.Ext(name) == contentExt:
			sub := strings.TrimSuffix(strings.TrimPrefix(name, subscriptionDir), contentExt)
			if b.Subscriptions.Find(sub) == nil {
				return nil, fmt.Errorf("%w: content for unknown subscription %q", ErrInvalid, sub)
			}
			b.Contents[sub] = data
		default:
			return nil, fmt.Errorf("%w: unexpected file %q", ErrInvalid, name)
		}
	}
	// 与 config.LoadAllMixins 的顺序相同：共用的在前，订阅的在后。
	slices.SortFunc(b.Mixins, func(x, y config.Mixin) int {
		if xs, ys := strings.Contains(x.Name, "/"), strings.Contains(y.Name, "/"); xs != ys {
			if xs {
				return 1
			}
			return -1
		}
		return strings.Compare(x.Name, y.Name)
	})
	if a := b.Subscriptions.Active; a != "" && b.Contents[a] == nil {
		return nil, fmt.Errorf("%w: the active subscription %q has no content", ErrInvalid, a)
	}
	return b, nil
}

// decode 解析 backup.json 并检查版本。
func decode(data []byte) (*Backup, error) {
	var b Backup
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalid, manifestName, err)
	}
	switch {
	case b.Version > Version:
		return nil, fmt.Errorf("%w: the backup has version %d, newer than this tunlr supports (%d); upgrade tunlr", core.ErrInvalidArgument, b.Version, Version)
	case b.Version < 1:
		return nil, fmt.Errorf("%w: invalid version %d", ErrInvalid, b.Version)
	case b.Version < Version:
		return nil, fmt.Errorf("%w: the backup has version %d, made by an older tunlr; it can no longer be restored", core.ErrInvalidArgument, b.Version)
	}
	return &b, nil
}

// ReadFile 读取备份文件 path；path 为 "-" 时从 stdin 读取。
func ReadFile(path string, stdin io.Reader) (*Backup, error) {
	if path == "-" {
		return Read(stdin)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f)
}
