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
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

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

// FileChecksum 返回文件的 sha256（十六进制）。边读边算，不把整个文件读进内存：内核二进制有几十 MB，
// 菜单每次刷新状态都要算一次。文件不存在时返回的错误满足 errors.Is(err, fs.ErrNotExist)。
func FileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sumCache 记住最近一次算过的文件校验值。主菜单每次刷新状态都要核对内核，二进制没变时不必再读一遍。
var sumCache struct {
	sync.Mutex
	path string
	info os.FileInfo
	sum  string
}

// cachedFileChecksum 同 FileChecksum，但文件仍是上次那个（同一个 inode，大小和修改时间不变）时
// 直接返回上次的结果。替换内核用临时文件加 rename，换了文件就是另一个 inode。
func cachedFileChecksum(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	sumCache.Lock()
	defer sumCache.Unlock()
	if c := &sumCache; c.path == path && os.SameFile(c.info, info) && c.info.Size() == info.Size() &&
		c.info.ModTime().Equal(info.ModTime()) {
		return c.sum, nil
	}
	sum, err := FileChecksum(path)
	if err != nil {
		return "", err
	}
	sumCache.path, sumCache.info, sumCache.sum = path, info, sum
	return sum, nil
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

// kernelAssetPattern 匹配官方发布的内核资产名：mihomo-linux-amd64-v3-v1.19.33.gz、mihomo-linux-arm64-v1.19.33.gz。
var kernelAssetPattern = regexp.MustCompile(`^mihomo-linux-(amd64-v([123])|arm64)-(v[0-9]+\.[0-9]+\.[0-9]+)\.gz$`)

// ParseKernelAsset 从官方资产名中得出它的平台和版本（KernelAsset 的反向）。
func ParseKernelAsset(name string) (Arch, string, error) {
	m := kernelAssetPattern.FindStringSubmatch(name)
	if m == nil {
		return Arch{}, "", fmt.Errorf("%w: %q is not a mihomo release asset (expected e.g. mihomo-linux-amd64-v3-v1.19.33.gz or mihomo-linux-arm64-v1.19.33.gz)", core.ErrInvalidArgument, name)
	}
	if m[1] == "arm64" {
		return Arch{GOARCH: "arm64"}, m[3], nil
	}
	return Arch{GOARCH: "amd64", Level: int(m[2][0] - '0')}, m[3], nil
}

// LoadKernelFile 读取事先下载好的官方内核资产 path（离线安装）。文件名必须是官方发布中的资产名，
// 从中得知版本和平台；平台必须能在本机运行（amd64 的级别不高于本机的）。内容仍按官方 sha256 校验：
// 只查询发布信息（直连 API，连不上时经镜像），不下载资产。
func LoadKernelFile(ctx context.Context, d *download.Client, arch Arch, path string) (*Kernel, error) {
	name := filepath.Base(path)
	fileArch, version, err := ParseKernelAsset(name)
	if err != nil {
		return nil, err
	}
	switch {
	case fileArch.GOARCH != arch.GOARCH:
		return nil, fmt.Errorf("%w: %s is built for %s, this server is %s", core.ErrInvalidArgument, name, fileArch.GOARCH, arch.GOARCH)
	case fileArch.Level > arch.Level:
		return nil, fmt.Errorf("%w: %s needs x86-64-v%d, this CPU is %s; use %s", core.ErrInvalidArgument, name, fileArch.Level, arch, arch.KernelAsset(version))
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxKernelSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxKernelSize)
	}
	rel, err := d.Release(ctx, kernelRepo, version)
	if err != nil {
		return nil, err
	}
	asset, err := rel.Asset(name)
	if err != nil {
		return nil, err
	}
	gz, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := asset.Verify(gz); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	bin, err := gunzip(gz)
	if err != nil {
		return nil, fmt.Errorf("unpack %s: %w", name, err)
	}
	return &Kernel{Version: version, Asset: name, Binary: bin}, nil
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
