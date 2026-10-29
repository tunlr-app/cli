package subscription

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tunlr-app/cli/pkg/core"
)

// Source 是订阅的来源：地址、本地文件或标准输入，见 ParseSource。
type Source struct {
	URL string
	// Path 是本地文件的绝对路径，或 PathStdin。
	Path string
}

// ParseSource 解析 sub add、sub update --source 给出的来源：含 "://" 的是地址（只认 http、https），
// "-" 是标准输入，其余都是本地文件的路径（转为绝对路径，必须是存在的普通文件）。
func ParseSource(s string) (Source, error) {
	switch {
	case s == PathStdin:
		return Source{Path: PathStdin}, nil
	case strings.Contains(s, "://"):
		if err := ValidateURL(s); err != nil {
			return Source{}, err
		}
		return Source{URL: s}, nil
	case s == "":
		return Source{}, fmt.Errorf("%w: give the subscription URL, a file, or - for standard input", core.ErrInvalidArgument)
	}
	abs, err := filepath.Abs(s)
	if err != nil {
		return Source{}, fmt.Errorf("%w: %v", core.ErrInvalidArgument, err)
	}
	fi, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Source{}, fmt.Errorf("%w: no such file: %s (a URL must start with http:// or https://)", core.ErrInvalidArgument, abs)
	case err != nil:
		return Source{}, fmt.Errorf("%w: %v", core.ErrInvalidArgument, err)
	case !fi.Mode().IsRegular():
		return Source{}, fmt.Errorf("%w: not a regular file: %s", core.ErrInvalidArgument, abs)
	}
	return Source{Path: abs}, nil
}

// Describe 返回订阅来源的展示形式：地址只保留 scheme://host/...，文件为路径，标准输入为 "stdin"。
func Describe(s *Def) string {
	switch {
	case s.URL != "":
		return Redact(s.URL)
	case s.Path == PathStdin:
		return "stdin"
	}
	return s.Path
}

// ErrNoSource 表示从标准输入导入的订阅没有可以重读的来源。
var ErrNoSource = errors.New("imported from standard input; nothing to read again")

// read 读取本地文件或标准输入（stdin 为 nil 时报告 ErrNoSource），大小限制同抓取。
func read(s *Def, stdin io.Reader) ([]byte, error) {
	r := stdin
	name := "standard input"
	if s.Path != PathStdin {
		f, err := os.Open(s.Path)
		if err != nil {
			var pe *fs.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			return nil, fmt.Errorf("read %s: %w", s.Path, err)
		}
		defer f.Close()
		r, name = f, s.Path
	} else if stdin == nil {
		return nil, fmt.Errorf("%s: %w; give it again with: tunlr sub update %s --source -", s.Name, ErrNoSource, s.Name)
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if len(data) > maxBody {
		return nil, fmt.Errorf("read %s: larger than %d MB", name, maxBody>>20)
	}
	return data, nil
}
