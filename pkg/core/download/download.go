// Package download 下载 GitHub release 中的资产。字节可以经镜像传输，
// 但校验值只取自官方 API：镜像返回的内容与官方 sha256 不一致时一律丢弃。
package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

const (
	defaultAPIBase = "https://api.github.com"
	apiTimeout     = 30 * time.Second
	// assetTimeout 是单个资产下载的总时限。直连 GitHub 很慢时，应配置镜像而不是放宽时限。
	assetTimeout = 10 * time.Minute
	// maxAssetSize 限制单个资产的大小，防止 API 或镜像返回异常数据时耗尽内存。
	maxAssetSize = 256 << 20
)

// ErrChecksumMismatch 表示下载内容与官方 sha256 不一致。
var ErrChecksumMismatch = errors.New("checksum mismatch")

// Client 下载 GitHub release 资产。零值可用：直连 GitHub。
type Client struct {
	// HTTP 为空时使用 http.DefaultClient（遵循 HTTPS_PROXY 等环境变量）。
	HTTP *http.Client
	// Mirror 是 GitHub 下载镜像的前缀，例如 "https://gh-proxy.com/"；
	// 下载地址为 Mirror 后接完整的 github.com 地址。为空时直连。只用于下载字节，不用于 API。
	Mirror string
	// APIBase 为空时使用 https://api.github.com，测试时替换。
	APIBase string
}

// Asset 是 release 中的一个文件。
type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Digest 形如 "sha256:<hex>"，由 GitHub 计算。
	Digest string `json:"digest"`
	URL    string `json:"browser_download_url"`
}

// FetchAsset 下载 repo（形如 "owner/name"）在 tag 下名为 name 的资产，按官方 sha256 校验后
// 原子写入 dst。dst 所在目录必须已存在。
func (c *Client) FetchAsset(ctx context.Context, repo, tag, name, dst string, perm os.FileMode) error {
	data, err := c.Fetch(ctx, repo, tag, name)
	if err != nil {
		return err
	}
	return fsutil.WriteFile(dst, data, perm)
}

// Fetch 下载 repo 在 tag 下名为 name 的资产并按官方 sha256 校验，返回其内容。
// tag 为空时使用最新的正式发布。
func (c *Client) Fetch(ctx context.Context, repo, tag, name string) ([]byte, error) {
	asset, err := c.lookup(ctx, repo, tag, name)
	if err != nil {
		return nil, err
	}
	want, ok := strings.CutPrefix(asset.Digest, "sha256:")
	if !ok || len(want) != sha256.Size*2 {
		return nil, fmt.Errorf("%s %s: GitHub provides no sha256 digest for %s; refusing to download unverified", repo, releaseName(tag), name)
	}
	data, err := c.get(ctx, MirrorURL(c.Mirror, asset.URL), asset.Size)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("download %s: %w (want %s, got %s)", name, ErrChecksumMismatch, want, got)
	}
	return data, nil
}

func releaseName(tag string) string {
	if tag == "" {
		return "latest release"
	}
	return tag
}

// MirrorURL 返回经镜像下载 url（完整的 github.com 地址）的地址：镜像前缀后接 url。
// mirror 为空时原样返回。
func MirrorURL(mirror, url string) string {
	if mirror == "" {
		return url
	}
	return strings.TrimSuffix(mirror, "/") + "/" + url
}

// lookup 通过官方 API 查询资产的地址、大小和 sha256。
func (c *Client) lookup(ctx context.Context, repo, tag, name string) (*Asset, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	base := c.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	path := "/releases/tags/" + tag
	if tag == "" {
		path = "/releases/latest"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/repos/"+repo+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("query release %s %s: %w", repo, releaseName(tag), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query release %s %s: HTTP %s", repo, releaseName(tag), resp.Status)
	}
	var rel struct {
		Assets []Asset `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("query release %s %s: %w", repo, releaseName(tag), err)
	}
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			return &rel.Assets[i], nil
		}
	}
	return nil, fmt.Errorf("release %s %s has no asset %s", repo, releaseName(tag), name)
}

func (c *Client) get(ctx context.Context, url string, size int64) ([]byte, error) {
	if size <= 0 || size > maxAssetSize {
		return nil, fmt.Errorf("unexpected asset size %d", size)
	}
	ctx, cancel := context.WithTimeout(ctx, assetTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("got %d bytes, want %d", len(data), size)
	}
	return data, nil
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}
