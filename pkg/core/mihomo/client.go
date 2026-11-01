package mihomo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ErrNotRunning 表示内核没有在运行：API socket 不存在，或存在但无人监听
// （内核退出后 socket 文件会留下）。
var ErrNotRunning = errors.New("mihomo is not running")

// ErrNoResponse 表示内核在时限内没有响应某个请求（如重载配置卡住）。
var ErrNoResponse = errors.New("no response from mihomo")

const (
	// apiTimeout 是普通 API 请求的时限。
	apiTimeout = 10 * time.Second
	// reloadTimeout 是重新加载配置的时限。内核在加载时同步初始化 proxy-providers 和
	// rule-providers，新的 provider 需要现场抓取，可能较慢。
	reloadTimeout = 2 * time.Minute
)

// APIError 是内核 API 返回的错误。
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("mihomo API: HTTP %d", e.Status)
	}
	return "mihomo API: " + e.Message
}

// Client 经 Unix socket（external-controller-unix）访问内核的 REST API。经 socket 访问不需要 secret。
type Client struct {
	socket string
	http   *http.Client
}

// NewClient 返回经 socket 访问内核 API 的客户端。
func NewClient(socket string) *Client {
	var d net.Dialer
	return &Client{
		socket: socket,
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return d.DialContext(ctx, "unix", socket)
			},
			// 每个命令只发少量请求，不保留空闲连接，避免持有已被内核关闭的连接。
			DisableKeepAlives: true,
		}},
	}
}

// Version 返回运行中的内核版本，例如 v1.19.33。
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, apiTimeout, http.MethodGet, "/version", nil, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// TunConfig 是运行中的内核的 tun 设置（GET /configs 的 tun 段）中工具关心的部分。
type TunConfig struct {
	Enable bool `json:"enable"`
	// AutoDetectInterface 与 ExcludeUID 是防止内核自己的连接回到 TUN 的两道设置（见 DESIGN.md 8.4）。
	AutoDetectInterface bool  `json:"auto-detect-interface"`
	ExcludeUID          []int `json:"exclude-uid"`
}

// Tun 返回运行中的内核的 tun 设置。面板可以在运行时切换 enable，所以与配置文件不一定一致。
func (c *Client) Tun(ctx context.Context) (*TunConfig, error) {
	var v struct {
		Tun TunConfig `json:"tun"`
	}
	if err := c.do(ctx, apiTimeout, http.MethodGet, "/configs", nil, &v); err != nil {
		return nil, err
	}
	return &v.Tun, nil
}

// TunEnabled 报告内核实际是否开启了 TUN（GET /configs 的 tun.enable），见 Tun。
func (c *Client) TunEnabled(ctx context.Context) (bool, error) {
	t, err := c.Tun(ctx)
	if err != nil {
		return false, err
	}
	return t.Enable, nil
}

// ReloadConfig 让内核重新加载 path（绝对路径，须位于内核工作目录内）的配置。
// 使用 force，端口等监听设置的变化也会生效。内核解析失败时返回 *APIError，并继续运行原来的配置。
//
// 内核加载时不报告监听端口失败等问题，所以成功返回只表示内核接受了这份配置。
func (c *Client) ReloadConfig(ctx context.Context, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	return c.do(ctx, reloadTimeout, http.MethodPut, "/configs?force=true", map[string]string{"path": abs}, nil)
}

// ConnectionsBySource 返回当前连接按来源地址的计数（GET /connections）。
func (c *Client) ConnectionsBySource(ctx context.Context) (map[string]int, error) {
	var v struct {
		Connections []struct {
			Metadata struct {
				SourceIP string `json:"sourceIP"`
			} `json:"metadata"`
		} `json:"connections"`
	}
	if err := c.do(ctx, apiTimeout, http.MethodGet, "/connections", nil, &v); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, conn := range v.Connections {
		if ip := conn.Metadata.SourceIP; ip != "" {
			counts[ip]++
		}
	}
	return counts, nil
}

// Resolve 让内核解析 name 的 IPv4 地址（GET /dns/query）：按配置中的 DNS 设置问上游，不返回 fake-ip。
// 上游返回错误码或没有地址时报错。
func (c *Client) Resolve(ctx context.Context, name string) ([]string, error) {
	var v struct {
		Status int `json:"Status"`
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := c.do(ctx, apiTimeout, http.MethodGet, "/dns/query?type=A&name="+url.QueryEscape(name), nil, &v); err != nil {
		return nil, err
	}
	if v.Status != 0 {
		return nil, fmt.Errorf("%s: DNS error code %d", name, v.Status)
	}
	var addrs []string
	for _, a := range v.Answer {
		if a.Type == 1 { // A
			addrs = append(addrs, a.Data)
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s: no addresses", name)
	}
	return addrs, nil
}

// geoUpdateTimeout 是 UpdateGeo 的时限。内核逐个下载 geo 文件，单个文件的时限是 90 秒。
const geoUpdateTimeout = 10 * time.Minute

// UpdateGeo 让内核按 geox-url 下载并加载配置用到的 geo 数据（POST /configs/geo）。请求同步执行；
// 内容与磁盘上相同的文件内核不做任何事。内核下载时不做任何校验。
func (c *Client) UpdateGeo(ctx context.Context) error {
	return c.do(ctx, geoUpdateTimeout, http.MethodPost, "/configs/geo", nil, nil)
}

func (c *Client) do(ctx context.Context, timeout time.Duration, method, path string, in, out any) error {
	if _, err := os.Stat(c.socket); errors.Is(err, fs.ErrNotExist) {
		return ErrNotRunning
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	// 主机名只用于 HTTP 请求行，连接总是走 socket。
	req, err := http.NewRequestWithContext(ctx, method, "http://mihomo"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
			return ErrNotRunning
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%w: mihomo API %s %s: no response within %s", ErrNoResponse, method, path, timeout)
		}
		// 去掉 url.Error 附带的请求地址，它只是占位的 http://mihomo。
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return fmt.Errorf("mihomo API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("mihomo API %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		e := &APIError{Status: resp.StatusCode}
		var msg struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &msg) == nil {
			e.Message = strings.TrimSpace(msg.Message)
		}
		return e
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("mihomo API %s %s: %w", method, path, err)
		}
	}
	return nil
}
