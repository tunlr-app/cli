package config

import (
	"fmt"
	"testing"
)

func TestListen(t *testing.T) {
	cfg := `mixed-port: 7890
socks-port: 0
port: 7891
external-controller: 127.0.0.1:9090
external-controller-unix: /run/tunlr/core.sock
authentication:
  - "user:pass"
dns:
  enable: true
  listen: 0.0.0.0:1053
`
	l, err := Listen([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	want := "[{mixed-port tcp 7890} {port tcp 7891} {external-controller tcp 9090} {dns.listen udp 1053}]"
	if got := fmt.Sprint(l.Listeners); got != want {
		t.Errorf("listeners = %s\nwant %s", got, want)
	}
	if l.MixedPort != 7890 || !l.Auth {
		t.Errorf("mixed-port %d, auth %v", l.MixedPort, l.Auth)
	}

	// DNS 没有启用时不算；authentication 为空时不要求认证。
	l, err = Listen([]byte("mixed-port: 7890\nauthentication: []\ndns:\n  listen: :53\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Listeners) != 1 || l.Auth {
		t.Errorf("listeners %v, auth %v", l.Listeners, l.Auth)
	}

	if l, err := Listen(nil); err != nil || len(l.Listeners) != 0 {
		t.Errorf("empty: %v, %v", l, err)
	}
	if _, err := Listen([]byte("a: [")); err == nil {
		t.Error("invalid YAML accepted")
	}
}
