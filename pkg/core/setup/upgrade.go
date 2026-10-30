package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// ToolRepo 是 tunlr 自己的 GitHub 仓库：tunlr upgrade 从它的 release 下载，每日的版本查询也查它。
const ToolRepo = "tunlr-app/cli"

// DevVersion 是没有经发布流程注入版本号的构建（go build）报告的版本。这样的构建不知道自己比哪个发布新或旧，
// 升级时必须指明版本，版本查询也不与它比较。
const DevVersion = "dev"

const (
	// maxToolSize 限制解压后的 tunlr 二进制大小，防止异常的压缩包耗尽内存。
	maxToolSize = 64 << 20
	// toolTimeout 是运行下载好的二进制（--version、init）的时限。init 要下载内核、发布配置和重启内核。
	toolTimeout = 15 * time.Minute
	// checkInterval 是查询最新版本的间隔：定时更新每小时唤醒一次，每天查一次。
	checkInterval = 24 * time.Hour
)

var toolVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// NormalizeToolVersion 检查用户给出的 tunlr 版本，补上开头的 v（0.2.0 → v0.2.0），返回 release 的 tag。
func NormalizeToolVersion(v string) (string, error) {
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !toolVersionPattern.MatchString(v) {
		return "", fmt.Errorf("%w: invalid tunlr version %q (e.g. v0.2.0)", core.ErrInvalidArgument, v)
	}
	return v, nil
}

// ToolAsset 返回 tag 的 tunlr 在 goarch 上的发布资产名，如 tunlr_0.2.0_linux_amd64.tar.gz（见 .goreleaser.yaml，
// 文件名中的版本不带 v）。
func ToolAsset(tag, goarch string) string {
	return fmt.Sprintf("tunlr_%s_linux_%s.tar.gz", strings.TrimPrefix(tag, "v"), goarch)
}

// UpgradeResult 是 tunlr upgrade 的结果。版本都是 tunlr --version 的写法（不带 v）。
type UpgradeResult struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Changed 报告是否换了二进制；已经是目标版本时为 false。
	Changed bool `json:"changed"`
	// Asset 是下载的发布资产名。
	Asset string `json:"asset,omitempty"`
	// Path 是替换的二进制。
	Path string `json:"path"`
	// Reinstalled 报告替换后是否由新版本执行了 init（服务已安装时），重写 unit 并按需迁移。
	Reinstalled bool `json:"reinstalled"`
}

// UpgradeTarget 返回 Upgrade 将换到的版本（不带 v）：version 为空时为最新的正式发布。开发版构建没有指明版本时拒绝。
func (i *Installer) UpgradeTarget(ctx context.Context, version string) (string, error) {
	rel, err := i.toolRelease(ctx, version)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(rel.Tag, "v"), nil
}

// toolRelease 查询要升级到的 tunlr release（version 是 tag，为空时为最新的正式发布）。
func (i *Installer) toolRelease(ctx context.Context, version string) (*download.Release, error) {
	if version == "" && i.Version == DevVersion {
		return nil, fmt.Errorf("%w: this tunlr is a development build; pass --version to install a release", core.ErrInvalidArgument)
	}
	d, err := i.downloader()
	if err != nil {
		return nil, err
	}
	rel, err := d.Release(ctx, ToolRepo, version)
	if err != nil {
		return nil, err
	}
	if !toolVersionPattern.MatchString(rel.Tag) {
		return nil, fmt.Errorf("unexpected tunlr release tag %q", rel.Tag)
	}
	return rel, nil
}

// Upgrade 把 /usr/local/bin/tunlr 换成 version（tag，为空时为最新的正式发布）：下载本平台的发布资产并按官方
// sha256 校验 → 解出二进制写到旁边的临时文件 → 运行它的 --version 确认能运行且版本正确 → 原子替换 → 服务已安装
// 时由新版本执行 init --yes，重写 unit。已经是目标版本时什么都不做。正在运行的进程不受影响（Linux 保留旧的 inode）。
// 没有回退命令：要换回旧版本，用 --version 指明它。
func (i *Installer) Upgrade(ctx context.Context, version string) (*UpgradeResult, error) {
	res := &UpgradeResult{From: i.Version, Path: i.Layout.ToolBinary()}
	rel, err := i.toolRelease(ctx, version)
	if err != nil {
		return nil, err
	}
	d, err := i.downloader()
	if err != nil {
		return nil, err
	}
	res.To = strings.TrimPrefix(rel.Tag, "v")
	if res.To == strings.TrimPrefix(i.Version, "v") {
		return res, nil
	}
	res.Asset = ToolAsset(rel.Tag, runtime.GOARCH)
	asset, err := rel.Asset(res.Asset)
	if err != nil {
		return nil, err
	}

	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	data, err := d.Get(ctx, asset)
	if err != nil {
		l.Release()
		return nil, err
	}
	bin, err := extractTool(data)
	if err != nil {
		l.Release()
		return nil, fmt.Errorf("unpack %s: %w", res.Asset, err)
	}
	dst := res.Path
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		l.Release()
		return nil, err
	}
	// 写在目标旁边，确认能运行后 rename：同一个目录才能原子替换。
	tmp := filepath.Join(filepath.Dir(dst), ".tunlr-new")
	if err := fsutil.WriteFile(tmp, bin, 0o755); err != nil {
		l.Release()
		return nil, err
	}
	defer os.Remove(tmp)
	if got, err := i.toolVersion(ctx, tmp); err != nil {
		l.Release()
		return nil, fmt.Errorf("the downloaded tunlr does not run: %w", err)
	} else if got != res.To {
		l.Release()
		return nil, fmt.Errorf("the downloaded tunlr reports version %q, expected %s", got, res.To)
	}
	if err := os.Rename(tmp, dst); err != nil {
		l.Release()
		return nil, err
	}
	res.Changed = true
	// init 自己取锁（装内核、发布、同步 timer），先放开。
	l.Release()

	if !i.systemd().Installed() {
		return res, nil
	}
	if out, err := i.runTool(ctx, dst, "init", "--yes"); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			err = fmt.Errorf("%w\n%s", err, msg)
		}
		return res, fmt.Errorf("tunlr %s is installed at %s, but its init failed (run it again: tunlr init): %w", res.To, dst, err)
	}
	res.Reinstalled = true
	return res, nil
}

// extractTool 从发布的 tar.gz 中取出 tunlr 二进制。
func extractTool(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no tunlr binary in the archive")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg || filepath.Base(h.Name) != "tunlr" {
			continue
		}
		bin, err := io.ReadAll(io.LimitReader(tr, maxToolSize+1))
		if err != nil {
			return nil, err
		}
		if len(bin) > maxToolSize {
			return nil, fmt.Errorf("larger than %d bytes", maxToolSize)
		}
		return bin, nil
	}
}

// toolVersion 运行 path 处的 tunlr --version --json，返回它报告的版本。
func (i *Installer) toolVersion(ctx context.Context, path string) (string, error) {
	out, err := i.runTool(ctx, path, "--version", "--json")
	if err != nil {
		return "", err
	}
	var env struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &env); err != nil || env.Data.Version == "" {
		return "", fmt.Errorf("unexpected output of --version --json: %s", bytes.TrimSpace(out))
	}
	return env.Data.Version, nil
}

func (i *Installer) runTool(ctx context.Context, path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	if i.RunTool != nil {
		return i.RunTool(ctx, path, args...)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
	}
	return out, err
}

// CheckUpdates 查询 tunlr（ToolRepo）和 mihomo 的最新正式发布（各一次 API 请求，不下载）并记入状态文件，
// 由每小时的定时更新调用：距上次查询不到一天，或还没有状态文件（没有安装）时不查，checked 为 false。
// 一个查询失败时保留它上次的结果，另一个照常记录；返回的错误说明失败的查询。查询不取锁，只在写状态时取。
func (i *Installer) CheckUpdates(ctx context.Context) (uc *state.UpdateCheck, checked bool, err error) {
	st, err := state.Load(i.Layout.StateFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	now := i.now()
	if st.UpdateCheck != nil && now.Sub(st.UpdateCheck.At) < checkInterval {
		return st.UpdateCheck, false, nil
	}
	d, err := i.downloader()
	if err != nil {
		return nil, false, err
	}
	var errs []error
	var tool, kernel string
	if rel, err := d.Release(ctx, ToolRepo, ""); err != nil {
		errs = append(errs, err)
	} else if !toolVersionPattern.MatchString(rel.Tag) {
		errs = append(errs, fmt.Errorf("unexpected tunlr release tag %q", rel.Tag))
	} else {
		tool = strings.TrimPrefix(rel.Tag, "v")
	}
	if v, err := i.kernels(nil, mihomo.Arch{}).UpgradeTarget(ctx, ""); err != nil {
		errs = append(errs, err)
	} else {
		kernel = v
	}
	if len(errs) == 2 {
		return st.UpdateCheck, true, fmt.Errorf("check for updates: %w", errors.Join(errs...))
	}

	l, err := i.lock(ctx)
	if err != nil {
		return nil, true, err
	}
	defer l.Release()
	if st, err = state.Load(i.Layout.StateFile()); err != nil {
		return nil, true, err
	}
	uc = &state.UpdateCheck{At: now.UTC()}
	if st.UpdateCheck != nil {
		uc.Tool, uc.Kernel = st.UpdateCheck.Tool, st.UpdateCheck.Kernel
	}
	if tool != "" {
		uc.Tool = tool
	}
	if kernel != "" {
		uc.Kernel = kernel
	}
	st.UpdateCheck = uc
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		return nil, true, err
	}
	if len(errs) > 0 {
		return uc, true, fmt.Errorf("check for updates: %w", errs[0])
	}
	return uc, true, nil
}

// UpdateCheck 是最近一次版本查询的结果（见 state.UpdateCheck）和据此可用的更新。
type UpdateCheck struct {
	At time.Time `json:"at"`
	// ToolLatest、KernelLatest 是查到的最新版本，还没有查到过时为空。
	ToolLatest   string `json:"tool_latest,omitempty"`
	KernelLatest string `json:"kernel_latest,omitempty"`
	// Tool 是比正在运行的 tunlr 新的版本（tunlr upgrade 装它），没有更新或 tunlr 是开发版时为空；
	// Kernel 是比运行中（没有运行时为已知可用的当前）的 mihomo 新的版本（tunlr core upgrade 装它）。
	Tool   string `json:"tool,omitempty"`
	Kernel string `json:"kernel,omitempty"`
}

// updateCheck 按查询结果算出可用的更新；没有查询过时为 nil。
func updateCheck(uc *state.UpdateCheck, tool string, cs *CoreStatus) *UpdateCheck {
	if uc == nil {
		return nil
	}
	c := &UpdateCheck{At: uc.At, ToolLatest: uc.Tool, KernelLatest: uc.Kernel}
	if newerVersion(uc.Tool, tool) {
		c.Tool = uc.Tool
	}
	cur := cs.Running
	if cur == "" && cs.Kernel != nil && cs.Kernel.KnownGood {
		cur = cs.Kernel.Version
	}
	if newerVersion(uc.Kernel, cur) {
		c.Kernel = uc.Kernel
	}
	return c
}

// newerVersion 报告 latest 是否比 current 新。任一个不是 x.y.z（可带 v）的形式时为 false：开发版和预发布版
// 不知道比哪个新。
func newerVersion(latest, current string) bool {
	l, ok1 := parseVersion(latest)
	c, ok2 := parseVersion(current)
	if !ok1 || !ok2 {
		return false
	}
	for k := range l {
		if l[k] != c[k] {
			return l[k] > c[k]
		}
	}
	return false
}

// parseVersion 解析 "1.2.3" 或 "v1.2.3"。
func parseVersion(v string) (parts [3]int, ok bool) {
	fields := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(fields) != 3 {
		return parts, false
	}
	for k, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 || (len(f) > 1 && f[0] == '0') {
			return parts, false
		}
		parts[k] = n
	}
	return parts, true
}
