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

// LoadOrCreate 读取状态文件；不存在时生成新的实例 ID 并写入。created 报告是否为新建。
func LoadOrCreate(path string, now time.Time) (s *State, created bool, err error) {
	s, err = Load(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return s, false, err
	}
	id, err := newInstanceID()
	if err != nil {
		return nil, false, err
	}
	s = &State{Version: Version, InstanceID: id, CreatedAt: now.UTC()}
	if err := Save(path, s); err != nil {
		return nil, false, err
	}
	return s, true, nil
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
