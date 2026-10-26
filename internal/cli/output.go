package cli

import (
	"encoding/json"
	"errors"
	"fmt"
)

// schemaVersion 是 --json 输出结构的版本号，结构发生不兼容变化时递增。
const schemaVersion = 1

// envelope 是所有 --json 输出的外层结构：成功时带 data，失败时带 error。
type envelope struct {
	Version int        `json:"version"`
	Data    any        `json:"data,omitempty"`
	Error   *jsonError `json:"error,omitempty"`
}

type jsonError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// render 输出命令结果：--json 时把 data 包进 envelope 写到标准输出，否则调用 text 渲染。
func (a *app) render(data any, text func() error) error {
	if a.opts.JSON {
		return a.writeJSON(envelope{Version: schemaVersion, Data: data})
	}
	return text()
}

func (a *app) writeJSON(v envelope) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printError 报告命令失败。--json 时错误同样以 envelope 写到标准输出，
// 方便调用方只解析一个流；否则写到标准错误。
func (a *app) printError(err error, code int) {
	if a.opts.JSON {
		if a.writeJSON(envelope{Version: schemaVersion, Error: &jsonError{Code: code, Message: err.Error()}}) == nil {
			return
		}
	}
	fmt.Fprintf(a.stderr, "tunlr: %v\n", err)
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintln(a.stderr, "Run 'tunlr --help' for usage.")
	}
}
