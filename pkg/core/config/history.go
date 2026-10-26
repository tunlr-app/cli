package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

// revisionVersion 是修订记录文件的结构版本。
const revisionVersion = 1

// 修订的来源。
const (
	// ReasonApply 表示由 apply 发布。
	ReasonApply = "apply"
	// ReasonRollback 表示由 rollback 发布，RollbackOf 指明恢复的是哪个修订。
	ReasonRollback = "rollback"
	// ReasonFound 表示发布前在磁盘上发现了不是由 tunlr 发布的配置（例如手工修改过），
	// 覆盖前先存档，以便找回。
	ReasonFound = "found"
)

// ErrNoRevision 表示要找的修订不存在。
var ErrNoRevision = errors.New("revision not found")

// Revision 是一份已发布配置的记录。修订号从 1 开始递增，不复用。
type Revision struct {
	Version      int       `json:"version"`
	Number       int       `json:"revision"`
	CreatedAt    time.Time `json:"created_at"`
	Reason       string    `json:"reason"`
	RollbackOf   int       `json:"rollback_of,omitempty"`
	Subscription string    `json:"subscription,omitempty"`
	SHA256       string    `json:"sha256"`
	// ToolVersion 是发布它的 tunlr 版本。
	ToolVersion string `json:"tunlr_version,omitempty"`
}

// History 管理已发布配置的历史：每个修订是 <号>.yaml（配置）与 <号>.json（记录）两个文件，
// 只保留最近 Keep 个。配置含机场凭据，权限 0600。
type History struct {
	Dir  string
	Keep int
}

// Checksum 返回配置内容的 sha256，用于判断磁盘上的配置是否与某个修订一致。
func Checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// List 返回所有修订，最新的在前。目录不存在时返回空列表；缺少配置文件或记录损坏的修订被跳过。
func (h *History) List() ([]Revision, error) {
	entries, err := os.ReadDir(h.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var revs []Revision
	for _, e := range entries {
		n, ok := revisionNumber(e.Name(), ".json")
		if !ok {
			continue
		}
		rev, err := h.readRecord(n)
		if err != nil {
			continue
		}
		if _, err := os.Stat(h.configPath(n)); err != nil {
			continue
		}
		revs = append(revs, *rev)
	}
	slices.SortFunc(revs, func(a, b Revision) int { return b.Number - a.Number })
	return revs, nil
}

// Read 返回修订 n 的配置和记录。
func (h *History) Read(n int) ([]byte, *Revision, error) {
	rev, err := h.readRecord(n)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("%w: %d", ErrNoRevision, n)
	}
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(h.configPath(n))
	if err != nil {
		return nil, nil, fmt.Errorf("revision %d: %w", n, err)
	}
	if Checksum(data) != rev.SHA256 {
		return nil, nil, fmt.Errorf("revision %d: content does not match its recorded checksum", n)
	}
	return data, rev, nil
}

// Add 把 data 存为新修订，填写 rev 的版本、修订号与校验和，然后清理超出 Keep 的旧修订。
// 先写配置、后写记录，中途失败时不会留下指向缺失配置的记录。
func (h *History) Add(data []byte, rev Revision) (*Revision, error) {
	if err := os.MkdirAll(h.Dir, 0o700); err != nil {
		return nil, err
	}
	revs, err := h.List()
	if err != nil {
		return nil, err
	}
	rev.Version = revisionVersion
	rev.Number = 1
	if len(revs) > 0 {
		rev.Number = revs[0].Number + 1
	}
	rev.SHA256 = Checksum(data)
	if err := fsutil.WriteFile(h.configPath(rev.Number), data, 0o600); err != nil {
		return nil, err
	}
	record, err := json.MarshalIndent(rev, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := fsutil.WriteFile(h.recordPath(rev.Number), append(record, '\n'), 0o600); err != nil {
		return nil, err
	}
	return &rev, h.prune(append([]Revision{rev}, revs...))
}

// prune 删除超出 Keep 的旧修订；revs 按从新到旧排列。
func (h *History) prune(revs []Revision) error {
	if h.Keep <= 0 || len(revs) <= h.Keep {
		return nil
	}
	for _, r := range revs[h.Keep:] {
		// 先删记录，删到一半时剩下的配置文件会被 List 忽略。
		if err := os.Remove(h.recordPath(r.Number)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Remove(h.configPath(r.Number)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (h *History) readRecord(n int) (*Revision, error) {
	data, err := os.ReadFile(h.recordPath(n))
	if err != nil {
		return nil, err
	}
	var rev Revision
	if err := json.Unmarshal(data, &rev); err != nil {
		return nil, fmt.Errorf("revision %d: %w", n, err)
	}
	if rev.Version > revisionVersion {
		return nil, fmt.Errorf("revision %d: version %d is newer than this tunlr supports (%d)", n, rev.Version, revisionVersion)
	}
	if rev.Number != n {
		return nil, fmt.Errorf("revision %d: record says %d", n, rev.Number)
	}
	return &rev, nil
}

func (h *History) configPath(n int) string { return filepath.Join(h.Dir, strconv.Itoa(n)+".yaml") }
func (h *History) recordPath(n int) string { return filepath.Join(h.Dir, strconv.Itoa(n)+".json") }

// revisionNumber 从 <号><ext> 形式的文件名中取出修订号。
func revisionNumber(name, ext string) (int, bool) {
	s, ok := strings.CutSuffix(name, ext)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || strconv.Itoa(n) != s {
		return 0, false
	}
	return n, true
}
