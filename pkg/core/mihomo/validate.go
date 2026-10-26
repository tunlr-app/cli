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
	"strconv"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

// defaultTestTimeout 是 mihomo -t 的时限。geo 数据预先备好时校验只需几十毫秒；
// 超时通常意味着内核在现场下载数据。
const defaultTestTimeout = 60 * time.Second

// Validator 用 mihomo -t 校验配置。
type Validator struct {
	// Binary 是用于校验的内核，应与将要运行该配置的内核一致。
	Binary string
	// HomeDir 是内核的工作目录。其中的 geo 文件提供给校验使用，缺失的会下载到这里。
	HomeDir string
	// Download 用于下载缺失的 geo 文件；为空时不下载。
	Download *download.Client
	// RunAs 是运行 mihomo -t 的身份，也是下载到 HomeDir 的 geo 文件的属主。内核二进制可被内核用户改写，
	// root 不能直接运行它，因此降权到内核用户；为 nil 时以当前身份运行（尚未创建内核用户，或测试）。
	RunAs *fsutil.Owner
	// Timeout 为零时使用 defaultTestTimeout。
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

// Validate 准备配置所需的 geo 数据，然后运行 mihomo -t。
// 配置有误时返回 *CheckError；内核不存在时返回 ErrNotInstalled；
// 准备数据失败或无法运行内核时返回其他错误。
func (v *Validator) Validate(ctx context.Context, cfg []byte) error {
	if _, err := os.Stat(v.Binary); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w (expected at %s)", ErrNotInstalled, v.Binary)
	}
	if v.Download != nil {
		// 解析失败的配置交给 mihomo -t 报告，这里不提前返回。
		if needs, err := config.DetectGeo(cfg); err == nil {
			if _, err := EnsureGeo(ctx, v.Download, v.HomeDir, GeoFilesFor(needs), v.RunAs); err != nil {
				return err
			}
		}
	}
	return v.test(ctx, cfg)
}

// test 在临时工作目录中运行 mihomo -t。不直接使用内核的工作目录，因为 -t 可能写入缓存，
// 与运行中的内核冲突；工作目录中已有的 geo 文件以符号链接提供。
func (v *Validator) test(ctx context.Context, cfg []byte) error {
	tmp, err := os.MkdirTemp("", "tunlr-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := linkGeo(v.HomeDir, tmp); err != nil {
		return err
	}
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		return err
	}
	if err := v.RunAs.ChownTree(tmp); err != nil {
		return err
	}

	timeout := v.Timeout
	if timeout == 0 {
		timeout = defaultTestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, v.Binary, "-t", "-d", tmp, "-f", cfgPath)
	cmd.WaitDelay = 5 * time.Second
	if err := runAs(cmd, v.RunAs); err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
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

// linkGeo 把 home 中的 geo 文件以符号链接放进 dst；文件系统不支持符号链接时复制。
func linkGeo(home, dst string) error {
	if home == "" {
		return nil
	}
	for _, f := range AllGeoFiles {
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
