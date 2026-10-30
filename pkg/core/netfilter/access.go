package netfilter

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/tunlr-app/cli/templates"
)

// AccessTable 是代理端口、面板对其他设备开放时的 nftables 表（inet 族）：这些端口只接受从选定网卡进来、
// 来自私有地址的连接，其余丢弃。
const AccessTable = "tunlr_access"

// Access 是 inet tunlr_access 表的内容。
type Access struct {
	// Ports 是对其他设备开放的端口，为空时不需要这张表。
	Ports []int
	// Interfaces 是放行的入口网卡（选定的网卡和网关模式的局域网网卡）。
	Interfaces []string
	// Containers 放行本机容器网桥（docker0、br-*）进来的连接。
	Containers bool
}

// RenderAccess 生成 inet tunlr_access 表的 nft 脚本。端口和网卡去重并排序，同样的内容生成同样的脚本。
func RenderAccess(a Access) ([]byte, error) {
	if len(a.Ports) == 0 {
		return nil, fmt.Errorf("no port to guard")
	}
	ports := slices.Clone(a.Ports)
	slices.Sort(ports)
	ports = slices.Compact(ports)
	list := make([]string, len(ports))
	for i, p := range ports {
		if p <= 0 || p > 65535 {
			return nil, fmt.Errorf("invalid port %d", p)
		}
		list[i] = strconv.Itoa(p)
	}
	ifaces := slices.Clone(a.Interfaces)
	slices.Sort(ifaces)
	ifaces = slices.Compact(ifaces)
	quoted := make([]string, len(ifaces))
	for i, name := range ifaces {
		if err := ValidInterface(name); err != nil {
			return nil, err
		}
		quoted[i] = strconv.Quote(name)
	}
	t, err := template.New("access.nft").Option("missingkey=error").Parse(string(templates.AccessNft))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = t.Execute(&b, struct {
		Table, Ports, Interfaces, Private, Private6 string
		Containers                                  bool
	}{AccessTable, strings.Join(list, ", "), strings.Join(quoted, ", "),
		strings.Join(PrivateIPv4, ", "), strings.Join(PrivateIPv6, ", "), a.Containers})
	return b.Bytes(), err
}

// AccessUp 加载（或整个替换）inet tunlr_access 表；a.Ports 为空时删除它。可以重复执行。
func (r *Rules) AccessUp(ctx context.Context, a Access) error {
	if len(a.Ports) == 0 {
		return r.AccessDown(ctx)
	}
	script, err := RenderAccess(a)
	if err != nil {
		return err
	}
	_, err = r.run(ctx, script, "nft", "-f", "-")
	return err
}

// AccessDown 删除 inet tunlr_access 表，不存在时什么都不做。
func (r *Rules) AccessDown(ctx context.Context) error {
	tables, err := r.Tables(ctx)
	if err != nil || !tables[AccessTable] {
		return err
	}
	_, err = r.run(ctx, nil, "nft", "delete", "table", "inet", AccessTable)
	return err
}
