// Package state 管理 tunlr 的状态文件。状态由工具维护，用户不需要编辑。
package state

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

// Version 是状态文件当前的结构版本。结构变化时递增，并在 migrations 中补上从上一版本升级的函数。
const Version = 1

// migrations[v] 把版本 v 的状态就地升级到 v+1。
var migrations = map[int]func(raw map[string]any) error{}

// State 是状态文件的内容。
type State struct {
	Version int `json:"version"`
	// InstanceID 唯一标识这一次安装，生成后不再改变，将来管理多台服务器时用于识别。
	InstanceID string    `json:"instance_id"`
	CreatedAt  time.Time `json:"created_at"`

	// Subscriptions 是已保存的订阅，按添加顺序排列。
	Subscriptions []Subscription `json:"subscriptions,omitempty"`
	// Active 是启用的订阅名称；同一时间只启用一个，没有订阅时为空。
	Active string `json:"active,omitempty"`

	// APISecret 是内核 HTTP API（面板经它访问）的 secret，第一次发布配置时生成。
	APISecret string `json:"api_secret,omitempty"`

	// Kernels 是已知可用的内核版本，最近确认的在前。
	Kernels []Kernel `json:"kernels,omitempty"`

	// Tun 是透明代理（TUN）的开关：空为关闭，TunPending 为已开启、正在检测，TunOn 为已确认。
	// 只由 tun on/off 和防失联回滚修改，不放在用户可以手改的设置中，以免绕过检测。
	Tun string `json:"tun,omitempty"`
	// TunDeadline 是 TunPending 的期限：过期仍未确认时，由回滚定时器或下次启动内核前关闭。
	TunDeadline time.Time `json:"tun_deadline,omitzero"`

	// Gateway 是网关模式的开关，nil 为关闭。只由 gateway on/off、tun off 和防失联回滚修改。
	Gateway *Gateway `json:"gateway,omitempty"`

	// PanelLAN 是面板的局域网访问开关，nil 为关闭（只有本机能访问）。只由 panel lan on/off 修改。
	PanelLAN *PanelLAN `json:"panel_lan,omitempty"`

	// ProxyLAN 是代理端口的局域网访问开关，nil 为关闭（代理端口只监听本机）。只由 proxy lan on/off 修改。
	ProxyLAN *ProxyLAN `json:"proxy_lan,omitempty"`

	// Devices 是用户起过名称或设置过走法的局域网设备。只由 tunlr devices、tunlr filter device 修改。
	Devices []Device `json:"devices,omitempty"`

	// Filter 是全局过滤项，零值为都关闭。只由 tunlr filter 修改。
	Filter Filter `json:"filter,omitzero"`

	// Updates 记录定时更新任务（键为任务名，目前只有 geo；订阅的记在各自的 FetchedAt、LastErrorAt 中）
	// 最近一次执行的情况，用来判断下次何时到期。只记最近一次，历次的情况在 journal 中。
	Updates map[string]UpdateRun `json:"updates,omitempty"`
}

// UpdateRun 是一次定时更新任务的执行情况。
type UpdateRun struct {
	At time.Time `json:"at"`
	// Status 是 ok、skipped 或 failed，Message 是说明。
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// 透明代理开关的状态，关闭时为空。
const (
	TunPending = "pending"
	TunOn      = "on"
)

// TunEnabled 报告配置中是否开启 TUN：已确认或正在检测时都开启。
func (s *State) TunEnabled() bool {
	return s.Tun == TunOn || s.Tun == TunPending
}

// SetTun 设置透明代理开关。TunPending 时 deadline 是期限，其他状态忽略它。
func (s *State) SetTun(v string, deadline time.Time) {
	s.Tun, s.TunDeadline = v, time.Time{}
	if v == TunPending {
		s.TunDeadline = deadline.UTC()
	}
}

// Gateway 是开启网关模式时的设置。
type Gateway struct {
	// Interface 是局域网网卡。
	Interface string `json:"interface"`
	// ForwardBefore 是开启前 net.ipv4.ip_forward 的值（"0" 或 "1"），关闭时据此决定是否恢复。
	ForwardBefore string `json:"forward_before"`
	// Since 是开启的时间。
	Since time.Time `json:"since"`
}

// PanelLAN 是开启面板局域网访问时的设置。
type PanelLAN struct {
	// Since 是开启的时间。
	Since time.Time `json:"since"`
}

// ProxyLAN 是开启代理端口局域网访问时的设置。
type ProxyLAN struct {
	// Since 是开启的时间。
	Since time.Time `json:"since"`
}

// RouteDirect 是设备的走法“全部直连”；空为默认的“按规则分流”。
const RouteDirect = "direct"

// Device 是一台记录下来的局域网设备。
type Device struct {
	// MAC 用来在地址变化后认出设备；预先按地址设置、还没见过的设备为空。
	MAC string `json:"mac,omitempty"`
	// IP 是分流规则按来源匹配的地址（内核没有按 MAC 匹配的规则）。
	IP   string `json:"ip"`
	Name string `json:"name,omitempty"`
	// Route 为 RouteDirect 或空。
	Route string `json:"route,omitempty"`
}

// DirectIPs 返回走法为直连的设备的地址，按记录的顺序。
func (s *State) DirectIPs() []string {
	var ips []string
	for _, d := range s.Devices {
		if d.Route == RouteDirect {
			ips = append(ips, d.IP)
		}
	}
	return ips
}

// Filter 是全局过滤项（DESIGN.md 9 第 3 条）。
type Filter struct {
	// Sources 是全部直连的来源地址段（如 "172.17.0.0/16"），按添加的顺序。
	Sources []string `json:"sources,omitempty"`
	// QUIC 拒绝 UDP 443 的连接，让浏览器改用 TCP。
	QUIC bool `json:"quic,omitempty"`
	// Ports 开启时目标端口不在 PortList 中的连接直连。
	Ports bool `json:"ports,omitempty"`
	// PortList 是只代理的端口（如 "443"、"8000-9000"），为空时用默认的列表；关闭 Ports 时保留。
	PortList []string `json:"port_list,omitempty"`
}

// Kernel 是一个已知可用的内核版本，二进制存放在 paths.Layout.KernelBinary(Version)。
type Kernel struct {
	// Version 形如 v1.19.33。
	Version string `json:"version"`
	// Asset 是它在官方发布中的资产名，如 mihomo-linux-amd64-v3-v1.19.33.gz。
	Asset string `json:"asset"`
	// SHA256 是解压后的二进制的 sha256。
	SHA256 string `json:"sha256"`
	// VerifiedAt 是它通过健康检查或核对的时间。
	VerifiedAt time.Time `json:"verified_at"`
}

// FindKernel 返回二进制的 sha256 为 sum 的已知可用版本，不存在时返回 nil。
func (s *State) FindKernel(sum string) *Kernel {
	for i := range s.Kernels {
		if s.Kernels[i].SHA256 == sum {
			return &s.Kernels[i]
		}
	}
	return nil
}

// Subscription 是一个已保存的订阅。抓取到的内容单独存放在 paths.Layout.SubscriptionFile 中。
type Subscription struct {
	Name string `json:"name"`
	// URL 通常带有机场的访问令牌，不能出现在输出和日志中。
	URL string `json:"url"`
	// UserAgent 为空时使用默认值。机场按 User-Agent 决定返回的格式。
	UserAgent string `json:"user_agent,omitempty"`
	// Update 是自动更新的周期（如 "6h"、"1d"，"off" 为不自动更新），为空时为 DefaultUpdate。
	// 不论是否启用都按它更新；只有启用的订阅更新后才发布配置。
	Update  string    `json:"update,omitempty"`
	AddedAt time.Time `json:"added_at"`

	// FetchedAt 是最近一次成功抓取的时间。
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	// UserInfo 来自最近一次成功抓取的 subscription-userinfo 响应头，机场没有提供时为空。
	UserInfo *UserInfo `json:"userinfo,omitempty"`
	// Counts 统计最近一次成功抓取的内容，列出订阅时不必重新解析大订阅。
	// 较早版本写入的状态文件中没有，这时由读取方解析内容得到。
	Counts *Counts `json:"counts,omitempty"`
	// LastError 是最近一次抓取失败的原因，再次成功后清空。
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
}

// DefaultUpdate 是订阅自动更新的默认周期。
const DefaultUpdate = "1d"

// UpdatePeriod 返回订阅的自动更新周期（没有设置时为默认值）。
func (s *Subscription) UpdatePeriod() string {
	if s.Update == "" {
		return DefaultUpdate
	}
	return s.Update
}

// Counts 是订阅内容中路由部分各项的数量，含义同 config.SubscriptionSummary。
type Counts struct {
	Proxies        int `json:"proxies"`
	ProxyProviders int `json:"proxy_providers"`
	ProxyGroups    int `json:"proxy_groups"`
	Rules          int `json:"rules"`
}

// UserInfo 是机场报告的流量与到期时间。
type UserInfo struct {
	Upload   int64     `json:"upload"`
	Download int64     `json:"download"`
	Total    int64     `json:"total"`
	Expire   time.Time `json:"expire,omitzero"`
}

// Find 返回名为 name 的订阅，不存在时返回 nil。
func (s *State) Find(name string) *Subscription {
	for i := range s.Subscriptions {
		if s.Subscriptions[i].Name == name {
			return &s.Subscriptions[i]
		}
	}
	return nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Load 读取状态文件，按需把旧版本升级到当前版本。文件不存在时返回的错误满足
// errors.Is(err, fs.ErrNotExist)。
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := decode(data, Version, migrations)
	if err != nil {
		return nil, fmt.Errorf("state file %s: %w", path, err)
	}
	return s, nil
}

// LoadOrNew 读取状态文件；不存在时返回带新实例 ID 的状态，但不写入磁盘，
// 由调用方在操作成功后用 Save 保存，失败的操作因此不会留下状态文件。created 报告是否为新建。
func LoadOrNew(path string, now time.Time) (s *State, created bool, err error) {
	s, err = Load(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return s, false, err
	}
	id, err := newInstanceID()
	if err != nil {
		return nil, false, err
	}
	return &State{Version: Version, InstanceID: id, CreatedAt: now.UTC()}, true, nil
}

// Save 原子地写入状态文件，必要时创建所在目录。
func Save(path string, s *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFile(path, append(data, '\n'), 0o600)
}

// decode 解析状态文件并升级到 target 版本。版本号比 target 新时拒绝，
// 避免旧版工具读写新版工具生成的文件。
func decode(data []byte, target int, migrations map[int]func(map[string]any) error) (*State, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	v, ok := raw["version"].(float64)
	if !ok || v < 1 || v != float64(int(v)) {
		return nil, errors.New("missing or invalid version")
	}
	version := int(v)
	if version > target {
		return nil, fmt.Errorf("version %d is newer than this tunlr supports (%d); upgrade tunlr", version, target)
	}
	for ; version < target; version++ {
		migrate, ok := migrations[version]
		if !ok {
			return nil, fmt.Errorf("no migration from version %d", version)
		}
		if err := migrate(raw); err != nil {
			return nil, fmt.Errorf("migrate from version %d: %w", version, err)
		}
	}
	raw["version"] = target

	upgraded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(upgraded, &s); err != nil {
		return nil, err
	}
	if !uuidPattern.MatchString(s.InstanceID) {
		return nil, fmt.Errorf("invalid instance_id %q", s.InstanceID)
	}
	return &s, nil
}

// newInstanceID 生成随机的 UUIDv4。
func newInstanceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40 // 版本 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 变体
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
