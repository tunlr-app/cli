package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"charm.land/huh/v2"

	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// cancelled 把用户在选择列表中取消（Esc、Ctrl-C）转换为正常结束：告诉用户什么都没做。
// 其他错误原样返回；err 为 nil 时返回 false。
func (a *app) cancelled(err error) (bool, error) {
	if errors.Is(err, huh.ErrUserAborted) {
		fmt.Fprintln(a.stdout, "Cancelled; nothing changed.")
		return true, nil
	}
	return err != nil, err
}

// columns 把每行以制表符分隔的字段对齐成列，供选择列表的选项使用（列表里不能用 tabwriter 直接输出）。
func columns(rows [][]string) []string {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return lines
}

// subscriptionOptions 是订阅的选择列表：名称、状态（启用、上次更新失败）、节点数、流量、到期日。
// 要能放进 80 列的终端，更新时间等细节留给 sub list。
func subscriptionOptions(entries []subscription.Entry) []string {
	rows := make([][]string, len(entries))
	for i, e := range entries {
		var notes []string
		if e.Active {
			notes = append(notes, "active")
		}
		if e.LastError != "" {
			notes = append(notes, "update failed")
		}
		usage, expires := formatUsage(e.UserInfo), formatExpire(e.UserInfo)
		if expires != "-" {
			expires = "until " + expires
		}
		rows[i] = []string{e.Name, strings.Join(notes, ", "), fmt.Sprintf("%d proxies", e.Summary.Proxies), usage, expires}
	}
	return columns(rows)
}

// revisionOptions 是历史修订的选择列表：编号、时间、来源、订阅，标出与生效配置相同的那一份。
func revisionOptions(revs []config.Revision, current int) []string {
	rows := make([][]string, len(revs))
	for i, r := range revs {
		sub := r.Subscription
		if sub == "" {
			sub = "-"
		}
		note := ""
		if r.Number == current {
			note = "(current)"
		}
		rows[i] = []string{
			fmt.Sprintf("#%d", r.Number), r.CreatedAt.Local().Format(time.DateTime), describeReason(r), sub, note,
		}
	}
	return columns(rows)
}
