package netfilter

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"text/template"

	"github.com/tunlr-app/cli/templates"
)

// PanelTable 是面板对局域网开放时的 nftables 表（inet 族）：API 端口只接受私有地址来的连接。
const PanelTable = "tunlr_panel"

// RenderPanel 生成 inet tunlr_panel 表的 nft 脚本，port 是内核 API 的端口。
func RenderPanel(port int) ([]byte, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port %d", port)
	}
	t, err := template.New("panel.nft").Option("missingkey=error").Parse(string(templates.PanelNft))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = t.Execute(&b, privateVars(map[string]string{"Table": PanelTable, "Port": strconv.Itoa(port)}))
	return b.Bytes(), err
}

// PanelUp 加载（或替换）inet tunlr_panel 表。可以重复执行。
func (r *Rules) PanelUp(ctx context.Context, port int) error {
	script, err := RenderPanel(port)
	if err != nil {
		return err
	}
	_, err = r.run(ctx, script, "nft", "-f", "-")
	return err
}

// PanelDown 删除 inet tunlr_panel 表，不存在时什么都不做。
func (r *Rules) PanelDown(ctx context.Context) error {
	tables, err := r.Tables(ctx)
	if err != nil || !tables[PanelTable] {
		return err
	}
	_, err = r.run(ctx, nil, "nft", "delete", "table", "inet", PanelTable)
	return err
}
