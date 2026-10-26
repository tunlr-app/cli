package mihomo

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/download"
)

// kernelRepo 是内核的发布仓库。
const kernelRepo = "MetaCubeX/mihomo"

// maxKernelSize 限制解压后的内核大小，防止异常的压缩包耗尽内存。
const maxKernelSize = 256 << 20

// Arch 是内核构建的平台。
type Arch struct {
	// GOARCH 是 amd64 或 arm64。
	GOARCH string
	// Level 是 amd64 的 x86-64 微架构级别（1–3），arm64 为 0。
	Level int
}

// DetectArch 返回本机的平台；amd64 按 /proc/cpuinfo 判断级别。
func DetectArch() (Arch, error) {
	switch runtime.GOARCH {
	case "arm64":
		return Arch{GOARCH: "arm64"}, nil
	case "amd64":
		data, err := os.ReadFile("/proc/cpuinfo")
		if err != nil {
			return Arch{}, fmt.Errorf("detect the CPU level: %w", err)
		}
		return Arch{GOARCH: "amd64", Level: CPULevel(data)}, nil
	}
	return Arch{}, fmt.Errorf("unsupported architecture %s: only amd64 and arm64 are supported", runtime.GOARCH)
}

// x86-64 各级别要求的 CPU 特性，用 Linux 在 /proc/cpuinfo 中的名称（pni 即 SSE3，abm 含 LZCNT）。
var (
	v2Flags = []string{"cx16", "lahf_lm", "popcnt", "pni", "sse4_1", "sse4_2", "ssse3"}
	v3Flags = []string{"avx", "avx2", "bmi1", "bmi2", "f16c", "fma", "abm", "movbe", "xsave"}
)

// CPULevel 按 /proc/cpuinfo 的内容返回 x86-64 微架构级别（1–3）。v4 的构建 mihomo 没有提供，按 v3 处理。
func CPULevel(cpuinfo []byte) int {
	var flags map[string]bool
	for line := range strings.Lines(string(cpuinfo)) {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "flags" {
			continue
		}
		flags = map[string]bool{}
		for f := range strings.FieldsSeq(value) {
			flags[f] = true
		}
		break
	}
	has := func(names []string) bool {
		for _, n := range names {
			if !flags[n] {
				return false
			}
		}
		return true
	}
	switch {
	case has(v2Flags) && has(v3Flags):
		return 3
	case has(v2Flags):
		return 2
	}
	return 1
}

func (a Arch) String() string {
	if a.GOARCH == "amd64" {
		return fmt.Sprintf("amd64 (x86-64-v%d)", a.Level)
	}
	return a.GOARCH
}

// KernelAsset 返回 version 的内核在官方发布中的资产名。amd64 使用标注级别的构建，
// 与面板触发升级时内核自己选择的资产一致。
func (a Arch) KernelAsset(version string) string {
	if a.GOARCH == "amd64" {
		return fmt.Sprintf("mihomo-linux-amd64-v%d-%s.gz", a.Level, version)
	}
	return fmt.Sprintf("mihomo-linux-%s-%s.gz", a.GOARCH, version)
}

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// NormalizeVersion 检查用户给出的内核版本，补上开头的 v（1.19.33 → v1.19.33）。
func NormalizeVersion(v string) (string, error) {
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !versionPattern.MatchString(v) {
		return "", fmt.Errorf("%w: invalid mihomo version %q (e.g. v1.19.33)", core.ErrInvalidArgument, v)
	}
	return v, nil
}

// Kernel 是下载并校验过的内核。
type Kernel struct {
	Version string
	Asset   string
	Binary  []byte
}

// SHA256 返回二进制的 sha256（十六进制）。
func (k *Kernel) SHA256() string { return Checksum(k.Binary) }

// Checksum 返回 data 的 sha256（十六进制）。
func Checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// FetchKernel 下载 version 的内核（为空时为最新的正式发布），按官方 sha256 校验后解压。
func FetchKernel(ctx context.Context, d *download.Client, arch Arch, version string) (*Kernel, error) {
	rel, err := d.Release(ctx, kernelRepo, version)
	if err != nil {
		return nil, err
	}
	if !versionPattern.MatchString(rel.Tag) {
		return nil, fmt.Errorf("unexpected mihomo release tag %q", rel.Tag)
	}
	name := arch.KernelAsset(rel.Tag)
	asset, err := rel.Asset(name)
	if err != nil {
		return nil, err
	}
	gz, err := d.Get(ctx, asset)
	if err != nil {
		return nil, err
	}
	bin, err := gunzip(gz)
	if err != nil {
		return nil, fmt.Errorf("unpack %s: %w", name, err)
	}
	return &Kernel{Version: rel.Tag, Asset: name, Binary: bin}, nil
}

func gunzip(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, maxKernelSize+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxKernelSize {
		return nil, fmt.Errorf("larger than %d bytes", maxKernelSize)
	}
	return out, nil
}
