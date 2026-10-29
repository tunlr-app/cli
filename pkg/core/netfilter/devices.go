package netfilter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/tunlr-app/cli/templates"
)

const (
	// DevicesTable 是记录局域网设备的 nftables 表（inet 族），只记录、不改动包。
	DevicesTable = "tunlr_devices"
	// DeviceTimeout 是设备没有流量后从记录中过期的时间。
	DeviceTimeout = 24 * time.Hour
)

// 设备使用本机的方式，也是 inet tunlr_devices 中集合的名称。
const (
	UseGateway = "gateway"
	UseProxy   = "proxy"
)

// RenderDevices 生成 inet tunlr_devices 表的 nft 脚本。集合 lan、ports 为空，由 DevicesSync 填入。
func RenderDevices() ([]byte, error) {
	t, err := template.New("devices.nft").Option("missingkey=error").Parse(string(templates.DevicesNft))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = t.Execute(&b, privateVars(map[string]string{
		"Table":   DevicesTable,
		"Timeout": fmt.Sprintf("%dh", int(DeviceTimeout.Hours())),
	}))
	return b.Bytes(), err
}

// DevicesSync 确保 inet tunlr_devices 表存在（不存在时加载），并把集合 lan 设为 ifaces（网关的局域网
// 网卡）、ports 设为 ports（对局域网开放的代理端口）。表已存在时只替换这两个集合，已记下的设备保留。
// 两者都为空时删除整张表。可以重复执行。
func (r *Rules) DevicesSync(ctx context.Context, ifaces []string, ports []int) error {
	if len(ifaces) == 0 && len(ports) == 0 {
		return r.DevicesDown(ctx)
	}
	tables, err := r.Tables(ctx)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if !tables[DevicesTable] {
		script, err := RenderDevices()
		if err != nil {
			return err
		}
		b.Write(script)
	}
	fmt.Fprintf(&b, "flush set inet %s lan\nflush set inet %s ports\n", DevicesTable, DevicesTable)
	if len(ifaces) > 0 {
		quoted := make([]string, len(ifaces))
		for i, name := range ifaces {
			if err := ValidInterface(name); err != nil {
				return err
			}
			quoted[i] = strconv.Quote(name)
		}
		fmt.Fprintf(&b, "add element inet %s lan { %s }\n", DevicesTable, strings.Join(quoted, ", "))
	}
	if len(ports) > 0 {
		list := make([]string, len(ports))
		for i, p := range ports {
			if p <= 0 || p > 65535 {
				return fmt.Errorf("invalid port %d", p)
			}
			list[i] = strconv.Itoa(p)
		}
		fmt.Fprintf(&b, "add element inet %s ports { %s }\n", DevicesTable, strings.Join(list, ", "))
	}
	_, err = r.run(ctx, b.Bytes(), "nft", "-f", "-")
	return err
}

// DevicesDown 删除 inet tunlr_devices 表，不存在时什么都不做。
func (r *Rules) DevicesDown(ctx context.Context) error {
	tables, err := r.Tables(ctx)
	if err != nil || !tables[DevicesTable] {
		return err
	}
	_, err = r.run(ctx, nil, "nft", "delete", "table", "inet", DevicesTable)
	return err
}

// Seen 是 inet tunlr_devices 记下的一台设备的一种用法。
type Seen struct {
	MAC string
	IP  string
	// Use 是 UseGateway 或 UseProxy。
	Use string
	// LastSeen 是最后一个包的时间，由剩余的过期时间推算（精确到秒）。
	LastSeen time.Time
}

// SeenDevices 返回 inet tunlr_devices 记下的设备；表不存在时 present 为 false。
func (r *Rules) SeenDevices(ctx context.Context, now time.Time) (seen []Seen, present bool, err error) {
	tables, err := r.Tables(ctx)
	if err != nil || !tables[DevicesTable] {
		return nil, false, err
	}
	for _, set := range []string{UseGateway, UseProxy} {
		out, err := r.run(ctx, nil, "nft", "-j", "list", "set", "inet", DevicesTable, set)
		if err != nil {
			return nil, true, err
		}
		list, err := parseSeen(out, set, now)
		if err != nil {
			return nil, true, fmt.Errorf("nft -j list set inet %s %s: %w", DevicesTable, set, err)
		}
		seen = append(seen, list...)
	}
	return seen, true, nil
}

// parseSeen 解析 nft -j list set 的输出，元素形如
// {"elem": {"val": {"concat": ["aa:bb:cc:dd:ee:ff", "192.168.1.5"]}, "timeout": 86400, "expires": 86321}}。
func parseSeen(out []byte, use string, now time.Time) ([]Seen, error) {
	var doc struct {
		Nftables []struct {
			Set *struct {
				Timeout int `json:"timeout"`
				Elem    []struct {
					Elem struct {
						Val struct {
							Concat []string `json:"concat"`
						} `json:"val"`
						Timeout int `json:"timeout"`
						Expires int `json:"expires"`
					} `json:"elem"`
				} `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	var seen []Seen
	for _, item := range doc.Nftables {
		if item.Set == nil {
			continue
		}
		for _, e := range item.Set.Elem {
			c := e.Elem.Val.Concat
			if len(c) != 2 {
				continue
			}
			mac, err := net.ParseMAC(c[0])
			if err != nil || net.ParseIP(c[1]) == nil {
				continue
			}
			timeout := e.Elem.Timeout
			if timeout == 0 {
				timeout = item.Set.Timeout
			}
			idle := time.Duration(max(timeout-e.Elem.Expires, 0)) * time.Second
			seen = append(seen, Seen{MAC: mac.String(), IP: c[1], Use: use, LastSeen: now.Add(-idle).Truncate(time.Second)})
		}
	}
	return seen, nil
}

// Neighbor 是邻居表（ip neigh）中的一项。
type Neighbor struct {
	IP, MAC, Interface string
	// Reachable 报告最近确认过可达（REACHABLE、DELAY、PROBE）。
	Reachable bool
}

// Neighbors 返回 IPv4 邻居表中有 MAC 地址的项。
func (r *Rules) Neighbors(ctx context.Context) ([]Neighbor, error) {
	out, err := r.run(ctx, nil, "ip", "-j", "-4", "neigh", "show")
	if err != nil {
		return nil, err
	}
	return parseNeighbors(out)
}

// parseNeighbors 解析 ip -j neigh show 的输出，项形如
// {"dst":"192.168.1.5","dev":"ens3","lladdr":"aa:bb:cc:dd:ee:ff","state":["REACHABLE"]}。
func parseNeighbors(out []byte) ([]Neighbor, error) {
	var list []struct {
		Dst    string   `json:"dst"`
		Dev    string   `json:"dev"`
		LLAddr string   `json:"lladdr"`
		State  []string `json:"state"`
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("ip -j neigh show: %w", err)
	}
	var res []Neighbor
	for _, n := range list {
		mac, err := net.ParseMAC(n.LLAddr)
		if err != nil || net.ParseIP(n.Dst) == nil {
			continue
		}
		nb := Neighbor{IP: n.Dst, MAC: mac.String(), Interface: n.Dev}
		for _, s := range n.State {
			if s == "REACHABLE" || s == "DELAY" || s == "PROBE" {
				nb.Reachable = true
			}
		}
		res = append(res, nb)
	}
	return res, nil
}

// DefaultGateway 返回 IPv4 默认路由的下一跳（通常是路由器），没有时为空。
func (r *Rules) DefaultGateway(ctx context.Context) string {
	out, err := r.run(ctx, nil, "ip", "-4", "route", "show", "default")
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "via" && net.ParseIP(f[i+1]) != nil {
				return f[i+1]
			}
		}
	}
	return ""
}
