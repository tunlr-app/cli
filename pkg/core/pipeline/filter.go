package pipeline

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/tunlr-app/cli/pkg/core/state"
)

// DefaultPorts 是只代理常用端口时默认代理的端口（同 ShellCrash）：SSH、HTTP、HTTPS、邮件、DoT、XMPP 等。
var DefaultPorts = []string{"22", "80", "143", "194", "443", "465", "587", "853", "993", "995", "5222", "8080", "8443"}

// ParsePorts 解析端口列表：端口或范围（8000-9000），以逗号、空格或 / 分隔。返回去重后的列表，保持顺序。
func ParsePorts(s string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '/' || r == ' ' || r == '\t' }) {
		lo, hi, isRange := strings.Cut(f, "-")
		a, err := parsePort(lo)
		if err != nil {
			return nil, err
		}
		b := a
		if isRange {
			if b, err = parsePort(hi); err != nil {
				return nil, err
			}
			if b < a {
				return nil, fmt.Errorf("the port range %s is reversed", f)
			}
		}
		p := strconv.Itoa(a)
		if b != a {
			p += "-" + strconv.Itoa(b)
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no ports given")
	}
	return out, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%q is not a port (1-65535)", s)
	}
	return n, nil
}

// ParseSource 解析来源地址段：CIDR（如 172.17.0.0/16），或单个地址（记为 /32、/128）。主机位不为零的
// CIDR（如 172.17.0.9/16）有歧义（整个网段，还是只有这个地址），不接受，错误中给出两种写法。也不接受
// 整个地址空间（/0）。
func ParseSource(s string) (string, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		a, aerr := netip.ParseAddr(s)
		if aerr != nil || a.Zone() != "" {
			return "", fmt.Errorf("%q is not an address or a network like 172.17.0.0/16", s)
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if m := p.Masked(); m != p {
		return "", fmt.Errorf("%s is not a network address: use %s for the whole network, or %s for this address only", s, m, p.Addr())
	}
	if p.Bits() == 0 {
		return "", fmt.Errorf("%s covers every address; turn the transparent proxy off instead", p)
	}
	return p.String(), nil
}

// PortsOf 返回只代理的端口：f 中保存的列表，没有时为默认的列表。
func PortsOf(f state.Filter) []string {
	if len(f.PortList) > 0 {
		return f.PortList
	}
	return DefaultPorts
}

// filterRules 生成受管 mixin tunlr:filter 插在规则最前的规则：设为直连的设备和来源地址段在前，不受其他
// 过滤项影响；再拒绝 QUIC（排在端口之前，端口列表不含 443 时 QUIC 仍被拒绝）；最后让其他端口直连。
func filterRules(sw switches) []string {
	var rules []string
	for _, ip := range sw.direct {
		rules = append(rules, "SRC-IP-CIDR,"+ip+"/32,DIRECT")
	}
	for _, p := range sw.filter.Sources {
		rules = append(rules, "SRC-IP-CIDR,"+p+",DIRECT")
	}
	if sw.filter.QUIC {
		rules = append(rules, "AND,((NETWORK,UDP),(DST-PORT,443)),REJECT")
	}
	if sw.filter.Ports {
		rules = append(rules, "NOT,((DST-PORT,"+strings.Join(PortsOf(sw.filter), "/")+")),DIRECT")
	}
	return rules
}
