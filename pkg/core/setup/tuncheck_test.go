package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseSSHConnection(t *testing.T) {
	client, server, err := parseSSHConnection("192.168.11.41 51234 10.11.3.153 22")
	if err != nil || client.String() != "192.168.11.41:51234" || server.String() != "10.11.3.153:22" {
		t.Errorf("v4: %v %v %v", client, server, err)
	}
	client, server, err = parseSSHConnection("2001:db8::1 51234 2001:db8::2 2222")
	if err != nil || client.String() != "[2001:db8::1]:51234" || server.String() != "[2001:db8::2]:2222" {
		t.Errorf("v6: %v %v %v", client, server, err)
	}
	client, _, err = parseSSHConnection("::ffff:192.168.11.41 51234 ::ffff:10.11.3.153 22")
	if err != nil || client.String() != "192.168.11.41:51234" {
		t.Errorf("mapped: %v %v", client, err)
	}
	for _, bad := range []string{"", "1.2.3.4 1 5.6.7.8", "1.2.3.4 x 5.6.7.8 22", "a b c d"} {
		if _, _, err := parseSSHConnection(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestReadSSConn(t *testing.T) {
	out := "0      0      10.11.3.153:22   192.168.11.41:51234\n" +
		"\t cubic wscale:7,7 rto:204 rtt:1.2/0.5 bytes_sent:5120 bytes_acked:5121 bytes_received:3000 segs_out:40\n"
	conn, err := readSSConn(context.Background(), func(context.Context, ...string) ([]byte, error) {
		return []byte(out), nil
	}, nil)
	if err != nil || conn.acked != 5121 || conn.sendQ != 0 {
		t.Errorf("conn = %+v, %v", conn, err)
	}

	// 带 State 列、内核不报告 bytes_acked。
	out = "ESTAB  0  36  10.11.3.153:22  192.168.11.41:51234\n\t cubic rto:204\n"
	conn, err = readSSConn(context.Background(), func(context.Context, ...string) ([]byte, error) {
		return []byte(out), nil
	}, nil)
	if err != nil || conn.acked != -1 || conn.sendQ != 36 {
		t.Errorf("conn = %+v, %v", conn, err)
	}

	if _, err := readSSConn(context.Background(), func(context.Context, ...string) ([]byte, error) {
		return nil, nil
	}, nil); err == nil {
		t.Error("empty output: no error")
	}
}

// fakeSS 模拟 ss：每次查询返回当前的 bytes_acked，probed 之后按 ackAfterProbe 增加。
type fakeSS struct {
	acked         int
	ackAfterProbe int
	probed        bool
	args          []string
}

func (f *fakeSS) run(_ context.Context, args ...string) ([]byte, error) {
	f.args = args
	if f.probed {
		f.acked += f.ackAfterProbe
	}
	return fmt.Appendf(nil, "0 0 10.0.0.2:22 10.0.0.1:5000\n\t cubic bytes_acked:%d\n", f.acked), nil
}

func TestCheckSSH(t *testing.T) {
	defer func(d time.Duration) { sshTimeout = d }(sshTimeout)
	sshTimeout = time.Second
	ctx := context.Background()

	ok := &fakeSS{acked: 100, ackAfterProbe: 50}
	c := checkSSH(ctx, TunOptions{SSHConnection: "10.0.0.1 5000 10.0.0.2 22", Probe: func() { ok.probed = true }}, ok.run)
	if c.Status != StepOK {
		t.Errorf("acknowledged: %+v", c)
	}
	if got := strings.Join(ok.args, " "); got != "-Htni state established src 10.0.0.2:22 dst 10.0.0.1:5000" {
		t.Errorf("ss args = %q", got)
	}

	stuck := &fakeSS{acked: 100}
	c = checkSSH(ctx, TunOptions{SSHConnection: "10.0.0.1 5000 10.0.0.2 22", Probe: func() { stuck.probed = true }}, stuck.run)
	if c.Status != StepFailed || !stuck.probed {
		t.Errorf("not acknowledged: %+v", c)
	}

	for _, opts := range []TunOptions{
		{},
		{SSHConnection: "10.0.0.1 5000 10.0.0.2 22", SkipSSH: "running inside tmux", Probe: func() {}},
		{SSHConnection: "10.0.0.1 5000 10.0.0.2 22"},
	} {
		if c := checkSSH(ctx, opts, ok.run); c.Status != StepSkipped {
			t.Errorf("%+v: %+v", opts, c)
		}
	}
	gone := func(context.Context, ...string) ([]byte, error) { return nil, errors.New("exit status 1") }
	if c := checkSSH(ctx, TunOptions{SSHConnection: "10.0.0.1 5000 10.0.0.2 22", Probe: func() {}}, gone); c.Status != StepSkipped {
		t.Errorf("connection not found: %+v", c)
	}
}
