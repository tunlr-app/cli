package mihomo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

// defaultTestTimeout 是 mihomo -t 的时限。geo 数据都在时校验只需几十毫秒。
const defaultTestTimeout = 60 * time.Second

// geoTestTimeout 是 mihomo -t 要现场下载 geo 文件时的时限：内核下载单个文件的时限是 90 秒，
// 经 GitHub 镜像通常十几秒。
const geoTestTimeout = 5 * time.Minute

// Validator 用 mihomo -t 校验配置。tunlr 不下载 geo 数据：缺失的由 mihomo -t 按配置中的 geox-url 现场下载，
// 下载到的按 Keep、Staged 保留下来，下一次校验和内核启动时不必再下载。
type Validator struct {
	// Binary 是用于校验的内核，应与将要运行该配置的内核一致。
	Binary string
	// HomeDir 是内核的工作目录。其中的 geo 文件提供给校验使用。
	HomeDir string
	// Fresh 是来源变了的 geo 文件（见 GeoSourceChanges）：不提供 HomeDir 中的旧文件，由 mihomo -t 按新的
	// geox-url 下载。
	Fresh []GeoFile
	// Staged 不为空时，mihomo -t 下载的 Fresh 中的文件存到这里，发布时换进 HomeDir（见 InstallStagedGeo）。
	Staged string
	// Keep 为 true 时，mihomo -t 下载的、HomeDir 中缺失的 geo 文件存进 HomeDir；为 false 时丢弃（只读的检查）。
	Keep bool
	// RunAs 是运行 mihomo -t 的身份，也是保留下来的 geo 文件的属主。内核二进制可被内核用户改写，
	// root 不能直接运行它，因此降权到内核用户；为 nil 时以当前身份运行（尚未创建内核用户，或测试）。
	RunAs *fsutil.Owner
	// Timeout 为零时使用 defaultTestTimeout，要下载 geo 文件时使用 geoTestTimeout。
	Timeout time.Duration
}

// CheckError 是 mihomo -t 未通过的结果。
type CheckError struct {
	// Messages 是内核报告的错误。
	Messages []string
	// MissingData 表示内核在校验时需要现场下载数据（如 geo 文件）且下载失败，
	// 与配置写法无关，不应报告为配置错误。
	MissingData bool
}

func (e *CheckError) Error() string {
	prefix := "configuration check failed"
	if e.MissingData {
		prefix = "configuration check could not complete because mihomo failed to download data it needs"
	}
	return prefix + ": " + strings.Join(e.Messages, "; ")
}

// Unwrap 让配置错误满足 errors.Is(err, core.ErrCheckFailed)；数据缺失则不是检查未通过。
func (e *CheckError) Unwrap() error {
	if e.MissingData {
		return nil
	}
	return core.ErrCheckFailed
}

// ErrNotInstalled 表示用于校验的内核不存在。
var ErrNotInstalled = errors.New("mihomo is not installed")

// Validate 运行 mihomo -t。geo 是配置需要的 geo 数据（如合成时提取的 config.Facts.Geo），为 nil 时解析 cfg
// 判断，只用于决定时限：要现场下载时放宽。
// 配置有误时返回 *CheckError；内核不存在时返回 ErrNotInstalled；无法运行内核时返回其他错误。
func (v *Validator) Validate(ctx context.Context, cfg []byte, geo *config.GeoNeeds) error {
	if _, err := os.Stat(v.Binary); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w (expected at %s)", ErrNotInstalled, v.Binary)
	}
	if geo == nil {
		// 解析失败的配置交给 mihomo -t 报告。
		if needs, err := config.DetectGeo(cfg); err == nil {
			geo = &needs
		}
	}
	timeout := v.Timeout
	if timeout == 0 {
		timeout = defaultTestTimeout
		if geo != nil && v.downloads(*geo) {
			timeout = geoTestTimeout
		}
	}
	return v.test(ctx, cfg, timeout)
}

// downloads 报告 mihomo -t 是否要现场下载 geo 文件：配置用到的文件有缺失的，或来源变了。
func (v *Validator) downloads(geo config.GeoNeeds) bool {
	for _, f := range GeoFilesFor(geo) {
		if hasGeo(v.Fresh, f) {
			return true
		}
		if p, err := findGeo(v.HomeDir, f); err == nil && p == "" {
			return true
		}
	}
	return false
}

// test 在临时工作目录中运行 mihomo -t。不直接使用内核的工作目录，因为 -t 可能写入缓存，
// 与运行中的内核冲突；工作目录中已有的 geo 文件以符号链接提供。
func (v *Validator) test(ctx context.Context, cfg []byte, timeout time.Duration) error {
	tmp, err := os.MkdirTemp("", "tunlr-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := linkGeo(v.HomeDir, tmp, v.Fresh); err != nil {
		return err
	}
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		return err
	}
	if err := v.RunAs.ChownTree(tmp); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, v.Binary, "-t", "-d", tmp, "-f", cfgPath)
	cmd.WaitDelay = 5 * time.Second
	if err := runAs(cmd, v.RunAs); err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	// 校验没通过也保留下载到的文件：与配置写法无关，下次不必再下载。
	if kerr := v.keep(tmp); kerr != nil {
		return fmt.Errorf("keep the geo data mihomo downloaded: %w", kerr)
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("mihomo -t did not finish within %s", timeout)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return fmt.Errorf("run %s: %w", v.Binary, err)
	}
	return parseTestOutput(string(out), ee.ExitCode())
}

// keep 把 mihomo -t 在 tmp 中下载的 geo 文件存下来（见 Keep、Staged）。符号链接是 HomeDir 中原有的文件。
func (v *Validator) keep(tmp string) error {
	for _, f := range AllGeoFiles {
		p, err := findGeo(tmp, f)
		if err != nil || p == "" {
			continue
		}
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		var dst string
		switch {
		case hasGeo(v.Fresh, f):
			if v.Staged == "" {
				continue
			}
			if err := os.MkdirAll(v.Staged, 0o755); err != nil {
				return err
			}
			dst = filepath.Join(v.Staged, f.Name)
		case v.Keep:
			// 文件系统不支持符号链接时 HomeDir 中的文件被复制进来，不是下载的。
			if have, err := findGeo(v.HomeDir, f); err != nil || have != "" {
				continue
			}
			if err := EnsureHome(v.HomeDir, v.RunAs); err != nil {
				return err
			}
			dst = filepath.Join(v.HomeDir, f.Name)
		default:
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := fsutil.WriteFileOwned(dst, data, 0o644, v.RunAs); err != nil {
			return err
		}
	}
	return nil
}

// hasGeo 报告 files 中有没有 f。
func hasGeo(files []GeoFile, f GeoFile) bool {
	return slices.ContainsFunc(files, func(g GeoFile) bool { return g.Name == f.Name })
}

// linkGeo 把 home 中的 geo 文件以符号链接放进 dst，skip 中的除外；文件系统不支持符号链接时复制。
func linkGeo(home, dst string, skip []GeoFile) error {
	for _, f := range AllGeoFiles {
		if hasGeo(skip, f) {
			continue
		}
		p, err := findGeo(home, f)
		if err != nil || p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.Base(p))
		if os.Symlink(abs, target) == nil {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// logMsg 匹配 mihomo 日志中的错误：level=error msg="..."。
var logMsg = regexp.MustCompile(`level=(?:error|fatal) msg=("(?:[^"\\]|\\.)*")`)

// parseTestOutput 从 mihomo -t 的输出中提取错误，并判断是否属于下载数据失败。
func parseTestOutput(out string, exitCode int) *CheckError {
	e := &CheckError{}
	for _, m := range logMsg.FindAllStringSubmatch(out, -1) {
		msg, err := strconv.Unquote(m[1])
		if err != nil {
			msg = m[1]
		}
		e.Messages = append(e.Messages, msg)
		if strings.Contains(msg, "can't download") {
			e.MissingData = true
		}
	}
	if len(e.Messages) == 0 {
		// 没有结构化的错误时，保留输出中最后一行内容。
		lines := strings.Split(strings.TrimSpace(out), "\n")
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "" {
			last = fmt.Sprintf("mihomo -t exited with status %d", exitCode)
		}
		e.Messages = []string{last}
	}
	return e
}
