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
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

const (
	defaultAPIBase = "https://api.github.com"
	apiTimeout     = 30 * time.Second
	// defaultStallTimeout 是下载资产时允许多久收不到任何数据：超过就断开这一轮，从已收到的位置续传。
	// 不限总时长，慢但一直有数据的下载（直连 GitHub 常只有几十 KB/s）照样能完成。
	defaultStallTimeout = 60 * time.Second
	// maxStalledAttempts 是连续多少轮没有收到新数据就放弃。有进展的一轮把计数清零。
	maxStalledAttempts = 5
	// defaultRetryDelay 是重试前的等待，随连续失败的次数加倍，最多 30 秒。
	defaultRetryDelay = 2 * time.Second
	// maxAssetSize 限制单个资产的大小，防止 API 或镜像返回异常数据时耗尽内存。
	maxAssetSize = 256 << 20
)

// ErrChecksumMismatch 表示下载内容与官方 sha256 不一致。
var ErrChecksumMismatch = errors.New("checksum mismatch")

// ProgressFunc 报告一个资产的下载进度：已收到 done 字节，共 total 字节。
type ProgressFunc func(name string, done, total int64)

type progressKey struct{}

// WithProgress 返回带进度回调的 context：经它下载资产时，每收到一块数据调用一次 fn。
// 进度由界面层显示，经 context 传入，下载的调用方不必层层转交。
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// Client 下载 GitHub release 资产。零值可用：直连 GitHub。
type Client struct {
	// HTTP 为空时使用 http.DefaultClient（遵循 HTTPS_PROXY 等环境变量）。
	HTTP *http.Client
	// Mirror 是 GitHub 下载镜像的前缀，例如 "https://axisnow.gh-proxy.org/"；
	// 下载地址为 Mirror 后接完整的 github.com 地址。为空时直连。只用于下载字节，不用于 API。
	Mirror string
	// APIBase 为空时使用 https://api.github.com，测试时替换。
	APIBase string
	// StallTimeout、RetryDelay 为零时使用默认值（defaultStallTimeout、defaultRetryDelay），测试时缩短。
	StallTimeout, RetryDelay time.Duration
	// Log 记录中断与续传；为空时不记录。
	Log *slog.Logger
}

// Asset 是 release 中的一个文件。
type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Digest 形如 "sha256:<hex>"，由 GitHub 计算。
	Digest string `json:"digest"`
	URL    string `json:"browser_download_url"`
}

// SHA256 返回 GitHub 提供的 sha256（十六进制）。没有时返回错误：没有官方校验值的资产不能下载。
func (a *Asset) SHA256() (string, error) {
	sum, ok := strings.CutPrefix(a.Digest, "sha256:")
	if !ok || len(sum) != sha256.Size*2 {
		return "", fmt.Errorf("GitHub provides no sha256 digest for %s; refusing to download unverified", a.Name)
	}
	return sum, nil
}

// Release 是一个 GitHub release。
type Release struct {
	Repo string `json:"-"`
	Tag  string `json:"tag_name"`
	// requested 是查询时给出的 tag，为空表示最新的正式发布，用于错误信息。
	requested string
	Assets    []Asset `json:"assets"`
}

// Asset 返回名为 name 的资产。
func (r *Release) Asset(name string) (*Asset, error) {
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i], nil
		}
	}
	return nil, fmt.Errorf("release %s %s has no asset %s", r.Repo, releaseName(r.requested), name)
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
	rel, err := c.Release(ctx, repo, tag)
	if err != nil {
		return nil, err
	}
	asset, err := rel.Asset(name)
	if err != nil {
		return nil, err
	}
	return c.Get(ctx, asset)
}

// Get 下载已查询到的资产并按官方 sha256 校验，返回其内容。
func (c *Client) Get(ctx context.Context, asset *Asset) ([]byte, error) {
	// 没有官方校验值的资产不下载。
	if _, err := asset.SHA256(); err != nil {
		return nil, err
	}
	if asset.Size <= 0 {
		return nil, fmt.Errorf("download %s: unexpected asset size %d", asset.Name, asset.Size)
	}
	data, err := c.get(ctx, asset.Name, MirrorURL(c.Mirror, asset.URL), asset.Size)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset.Name, err)
	}
	if err := asset.Verify(data); err != nil {
		return nil, fmt.Errorf("download %s: %w", asset.Name, err)
	}
	return data, nil
}

// Verify 检查 data 是不是这个资产的内容：大小和官方 sha256 都要一致。
// 不一致时返回满足 errors.Is(err, ErrChecksumMismatch) 的错误。
func (a *Asset) Verify(data []byte) error {
	want, err := a.SHA256()
	if err != nil {
		return err
	}
	if int64(len(data)) != a.Size {
		return fmt.Errorf("%w: %d bytes, GitHub says %d", ErrChecksumMismatch, len(data), a.Size)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("%w (want %s, got %s)", ErrChecksumMismatch, want, got)
	}
	return nil
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

// Release 通过官方 API 查询 repo 在 tag 下的发布（tag 为空时为最新的正式发布），包括各资产的地址、
// 大小和 sha256。只直连 api.github.com，不经镜像：镜像转发的 API 请求按镜像的出口 IP 计入 GitHub 的
// 限额，所有用户共用，基本总是用完的，而且 sha256 也会经它的手（2026-10-31 决定不做兜底）。
func (c *Client) Release(ctx context.Context, repo, tag string) (*Release, error) {
	base := c.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	path := "/releases/tags/" + tag
	if tag == "" {
		path = "/releases/latest"
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
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
		return nil, fmt.Errorf("query release %s %s: %w", repo, releaseName(tag), httpError{resp.StatusCode, resp.Status, apiDetail(resp)})
	}
	rel := &Release{Repo: repo, requested: tag}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(rel); err != nil {
		return nil, fmt.Errorf("query release %s %s: %w", repo, releaseName(tag), err)
	}
	return rel, nil
}

// httpError 是 API 非 200 的响应，detail 是对它的解释（如限流何时解除），可为空。
type httpError struct {
	code   int
	status string
	detail string
}

func (e httpError) Error() string {
	if e.detail != "" {
		return "HTTP " + e.status + ": " + e.detail
	}
	return "HTTP " + e.status
}

// apiDetail 解释 API 的错误响应：未认证的请求每个 IP 每小时只能查 60 次，用完时 GitHub 回 403，
// 响应头给出解除时间；其他情况取响应正文中的 message。
func apiDetail(resp *http.Response) string {
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		detail := "GitHub API rate limit exceeded for this IP (60 unauthenticated requests per hour)"
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			detail += "; it resets at " + time.Unix(reset, 0).Local().Format("15:04")
		}
		return detail
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&body); err == nil {
		return body.Message
	}
	return ""
}

// errStalled 表示一轮下载在 StallTimeout 内没有收到任何数据。
var errStalled = errors.New("no data received")

// permanentError 是重试也没有用的错误（如 HTTP 404、内容超出大小）。
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// get 下载 url 的内容。size 是预期的大小，为 0 时不知道大小，只限制在 maxAssetSize 以内。
//
// 慢速线路（直连 GitHub、繁忙的镜像）上常见的是速度很低或中途断开，而不是彻底不通，所以不设总时限：
// 只要还在收到数据就继续；一轮在 StallTimeout 内收不到数据，或连接出错、遇到 408、429、5xx 时，带
// Range 从已收到的位置续传（服务器不支持续传时从头开始）。连续 maxStalledAttempts 轮没有新数据才放弃。
// 已收到的内容只在这次调用的内存中，完整性由调用方按官方 sha256 校验。
func (c *Client) get(ctx context.Context, name, url string, size int64) ([]byte, error) {
	if size < 0 || size > maxAssetSize {
		return nil, fmt.Errorf("unexpected asset size %d", size)
	}
	limit := size
	if size == 0 {
		limit = maxAssetSize
	}
	progress, _ := ctx.Value(progressKey{}).(ProgressFunc)
	report := func(done int64) {
		if progress != nil && size > 0 {
			progress(name, done, size)
		}
	}
	report(0)
	buf := make([]byte, 0, size)
	stalled := 0
	for {
		before := len(buf)
		err := c.getOnce(ctx, url, &buf, limit, report)
		if err == nil {
			if size > 0 && int64(len(buf)) != size {
				return nil, fmt.Errorf("got %d bytes, want %d", len(buf), size)
			}
			return buf, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if perm, ok := errors.AsType[permanentError](err); ok {
			return nil, perm.err
		}
		if len(buf) > before {
			stalled = 0
		} else {
			stalled++
		}
		if stalled >= maxStalledAttempts {
			return nil, fmt.Errorf("%w (gave up after %d attempts without progress, %d of %d bytes received)", err, stalled, len(buf), size)
		}
		delay := min(orDefault(c.RetryDelay, defaultRetryDelay)<<max(stalled-1, 0), 30*time.Second)
		if c.Log != nil {
			c.Log.Warn("download interrupted, resuming", "asset", name, "received", len(buf), "size", size, "error", err, "retry_in", delay)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// getOnce 是 get 的一轮：*buf 中已有内容时请求其后的部分，收到的数据追加到 *buf。服务器忽略 Range、
// 返回的位置不对或 416 时清空 *buf，下一轮从头开始。
func (c *Client) getOnce(ctx context.Context, url string, buf *[]byte, limit int64, report func(int64)) error {
	stall := orDefault(c.StallTimeout, defaultStallTimeout)
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// 连接、等待响应头和读取都受它约束：每收到一块数据重新计时。
	watchdog := time.AfterFunc(stall, func() { cancel(errStalled) })
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(actx, http.MethodGet, url, nil)
	if err != nil {
		return permanentError{err}
	}
	offset := len(*buf)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return stallCause(actx, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		if offset > 0 {
			*buf = (*buf)[:0]
			report(0)
		}
	case http.StatusPartialContent:
		if start, ok := rangeStart(resp.Header.Get("Content-Range")); !ok || start != int64(offset) {
			*buf = (*buf)[:0]
			report(0)
			return fmt.Errorf("unexpected Content-Range %q", resp.Header.Get("Content-Range"))
		}
	case http.StatusRequestedRangeNotSatisfiable:
		*buf = (*buf)[:0]
		report(0)
		return fmt.Errorf("HTTP %s", resp.Status)
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return fmt.Errorf("HTTP %s", resp.Status)
	default:
		if resp.StatusCode >= 500 {
			return fmt.Errorf("HTTP %s", resp.Status)
		}
		return permanentError{fmt.Errorf("HTTP %s", resp.Status)}
	}
	chunk := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(chunk)
		if n > 0 {
			if int64(len(*buf)+n) > limit {
				return permanentError{fmt.Errorf("larger than %d bytes", limit)}
			}
			*buf = append(*buf, chunk[:n]...)
			watchdog.Reset(stall)
			report(int64(len(*buf)))
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return stallCause(actx, err)
		}
	}
}

// stallCause 在这一轮因为收不到数据被取消时，把错误换成 errStalled。
func stallCause(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errStalled) {
		return errStalled
	}
	return err
}

// rangeStart 解析 Content-Range（"bytes 100-199/1000"）的起始位置。
func rangeStart(v string) (int64, bool) {
	v, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return 0, false
	}
	start, _, ok := strings.Cut(v, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(start, 10, 64)
	return n, err == nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}
