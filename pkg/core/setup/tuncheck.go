package setup

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tunlr-app/cli/pkg/core/mihomo"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
)

const (
	// tunSettle 是等待内核开启 TUN（网卡和规则出现）的最长时间。
	tunSettle = 5 * time.Second
	// probeTimeout 是 DNS、HTTP 检测各自的时限。
	probeTimeout = 10 * time.Second
)

// sshTimeout 是等待 SSH 客户端确认收到数据的最长时间（测试时缩短）。
var sshTimeout = 8 * time.Second

// probeURLs 是 HTTP 检测访问的地址，任意一个有响应即通过：一个国内、一个国外，按规则通常一个直连、
// 一个走代理。检测的是开启 TUN 后网络没有断，不是代理节点可用。
var probeURLs = []string{"https://www.baidu.com", "https://www.gstatic.com/generate_204"}

// checkNetwork 是开启透明代理后的默认检测：TUN 已生效，DNS、HTTP 可用，SSH 会话仍然通畅。
func (i *Installer) checkNetwork(ctx context.Context, opts TunOptions) []TunCheck {
	return []TunCheck{
		i.checkTunUp(ctx),
		checkDNS(ctx),
		checkHTTP(ctx),
		checkSSH(ctx, opts, runSS),
	}
}

// checkTunUp 检查内核开启了 TUN，网卡、内核的规则和工具的规则都在。
func (i *Installer) checkTunUp(ctx context.Context) TunCheck {
	c := TunCheck{Name: "tun"}
	client := mihomo.NewClient(i.Layout.CoreSocket())
	var missing []string
	for deadline := time.Now().Add(tunSettle); ; {
		missing = missing[:0]
		if on, err := client.TunEnabled(ctx); err != nil {
			missing = append(missing, "mihomo: "+err.Error())
		} else if !on {
			missing = append(missing, "mihomo reports TUN disabled")
		}
		if !netfilter.DeviceUp() {
			missing = append(missing, "no network interface "+netfilter.Device)
		}
		if tables, err := i.rules().Tables(ctx); err != nil {
			missing = append(missing, err.Error())
		} else {
			for _, t := range []string{netfilter.Table, netfilter.KernelTable} {
				if !tables[t] {
					missing = append(missing, "no nftables table inet "+t)
				}
			}
		}
		if len(missing) == 0 || time.Now().After(deadline) || sleep(ctx, 500*time.Millisecond) != nil {
			break
		}
	}
	if len(missing) > 0 {
		c.Status, c.Message = StepFailed, strings.Join(missing, "; ")
		return c
	}
	c.Status, c.Message = StepOK, "interface "+netfilter.Device+" is up"
	return c
}

// checkDNS 经系统的解析器（/etc/resolv.conf）解析一个域名；TUN 劫持了 53 端口，实际由内核解析。
func checkDNS(ctx context.Context) TunCheck {
	c := TunCheck{Name: "dns"}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	const host = "www.baidu.com"
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		c.Status, c.Message = StepFailed, err.Error()
		return c
	}
	c.Status, c.Message = StepOK, fmt.Sprintf("%s resolved to %s", host, strings.Join(addrs, ", "))
	return c
}

// checkHTTP 不经代理（忽略 HTTP_PROXY 等环境变量）同时访问 probeURLs，任意一个有响应即通过。
func checkHTTP(ctx context.Context) TunCheck {
	c := TunCheck{Name: "http"}
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		Timeout:   probeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	results := make([]string, len(probeURLs))
	ok := make([]bool, len(probeURLs))
	var wg sync.WaitGroup
	for n, u := range probeURLs {
		wg.Go(func() {
			start := time.Now()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				results[n] = err.Error()
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				results[n] = err.Error()
				return
			}
			resp.Body.Close()
			ok[n] = true
			results[n] = fmt.Sprintf("%s: HTTP %d in %s", u, resp.StatusCode, time.Since(start).Round(time.Millisecond))
		})
	}
	wg.Wait()
	// 逐个报告；只要有一个能访问就通过，另一个失败可能只是节点的问题。
	c.Status, c.Message = StepFailed, strings.Join(results, "; ")
	if slices.Contains(ok, true) {
		c.Status = StepOK
	}
	return c
}

// ssRunner 执行 ss 并返回输出，测试时替换。
type ssRunner func(ctx context.Context, args ...string) ([]byte, error)

func runSS(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "ss", args...).Output()
}

// checkSSH 检查运行 tunlr 的 SSH 会话在开启 TUN 后仍然通畅：让用户的终端输出一些文字（Probe），
// 看 sshd 发出的数据是否得到客户端确认（ss 报告的 bytes_acked 增加）。回包被 TUN 截走时客户端收不到，
// 不会确认。不在 SSH 会话中、找不到这条连接时跳过。
func checkSSH(ctx context.Context, opts TunOptions, run ssRunner) TunCheck {
	c := TunCheck{Name: "ssh", Status: StepSkipped}
	if opts.SkipSSH != "" {
		c.Message = opts.SkipSSH
		return c
	}
	if opts.SSHConnection == "" {
		c.Message = "not running in an SSH session"
		return c
	}
	client, server, err := parseSSHConnection(opts.SSHConnection)
	if err != nil {
		c.Message = err.Error()
		return c
	}
	if opts.Probe == nil {
		c.Message = "cannot write to the SSH session"
		return c
	}
	args := []string{"-Htni", "state", "established", "src", server.String(), "dst", client.String()}
	before, err := readSSConn(ctx, run, args)
	if err != nil {
		c.Message = "find the SSH connection: " + err.Error()
		return c
	}

	start := time.Now()
	opts.Probe()
	for {
		if sleep(ctx, 300*time.Millisecond) != nil {
			break
		}
		now, err := readSSConn(ctx, run, args)
		switch {
		case err != nil:
			c.Status, c.Message = StepFailed, "the SSH connection is gone: "+err.Error()
			return c
		case now.acked >= 0 && now.acked > before.acked:
			c.Status, c.Message = StepOK, "the SSH client acknowledged new data"
			return c
		case now.acked < 0 && now.sendQ == 0 && time.Since(start) >= time.Second:
			// 内核太旧，不报告 bytes_acked：发出的数据都已确认。
			c.Status, c.Message = StepOK, "the SSH client acknowledged all data sent"
			return c
		}
		if time.Since(start) >= sshTimeout {
			break
		}
	}
	c.Status = StepFailed
	c.Message = fmt.Sprintf("the SSH client %s did not acknowledge data within %s: replies to it are probably routed into TUN", client.Addr(), sshTimeout)
	return c
}

// parseSSHConnection 解析 $SSH_CONNECTION（客户端地址 客户端端口 本机地址 本机端口）。
func parseSSHConnection(s string) (client, server netip.AddrPort, err error) {
	f := strings.Fields(s)
	if len(f) != 4 {
		return client, server, fmt.Errorf("cannot parse SSH_CONNECTION %q", s)
	}
	parse := func(addr, port string) (netip.AddrPort, error) {
		a, err := netip.ParseAddr(addr)
		if err != nil {
			return netip.AddrPort{}, err
		}
		p, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return netip.AddrPort{}, err
		}
		return netip.AddrPortFrom(a.Unmap().WithZone(""), uint16(p)), nil
	}
	if client, err = parse(f[0], f[1]); err == nil {
		server, err = parse(f[2], f[3])
	}
	if err != nil {
		return client, server, fmt.Errorf("cannot parse SSH_CONNECTION %q: %w", s, err)
	}
	return client, server, nil
}

// ssConn 是 ss 报告的一条 TCP 连接：acked 是 bytes_acked（不报告时为 -1），sendQ 是未确认的字节数。
type ssConn struct {
	acked int64
	sendQ int64
}

var bytesAcked = regexp.MustCompile(`\bbytes_acked:(\d+)`)

// readSSConn 读取 ss -Htni 的输出。只过滤一种状态时 ss 不输出 State 列：第一行为
// Recv-Q Send-Q 本地地址 对端地址，第二行为 -i 的详细信息。
func readSSConn(ctx context.Context, run ssRunner, args []string) (ssConn, error) {
	out, err := run(ctx, args...)
	if err != nil {
		return ssConn{}, fmt.Errorf("ss: %w", err)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return ssConn{}, fmt.Errorf("no such established connection")
	}
	conn := ssConn{acked: -1, sendQ: -1}
	if f := strings.Fields(strings.SplitN(text, "\n", 2)[0]); len(f) >= 2 {
		if _, err := strconv.ParseInt(f[0], 10, 64); err != nil && len(f) >= 3 {
			f = f[1:] // 带 State 列的输出
		}
		conn.sendQ, _ = strconv.ParseInt(f[1], 10, 64)
	}
	if m := bytesAcked.FindStringSubmatch(text); m != nil {
		conn.acked, _ = strconv.ParseInt(m[1], 10, 64)
	}
	return conn, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
