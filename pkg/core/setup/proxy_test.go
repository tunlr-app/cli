package setup

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestProxyLAN(t *testing.T) {
	ctx := context.Background()
	i, _, _ := newTunInstaller(t)
	i.Interfaces = lanInterface
	installUnit(t, i)

	pre, err := i.ProxyLANPreflight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pre.On || pre.Port != 7890 || pre.Interface != "ens3" || len(pre.Addresses) != 1 || len(pre.PublicAddresses) != 0 {
		t.Errorf("preflight = %+v", pre)
	}

	plan, err := i.pipeline(nil).PlanProxyLAN(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.SetProxyLAN(ctx, true, plan); err != nil {
		t.Fatal(err)
	}
	if st := tunSwitch(t, i); st.ProxyLAN == nil {
		t.Error("switch not saved")
	}
	if cfg, _ := os.ReadFile(i.Layout.CoreConfig()); !strings.Contains(string(cfg), "\nallow-lan: true\n") {
		t.Errorf("config:\n%s", cfg)
	}
	if s, err := i.Status(ctx); err != nil || !s.ProxyLAN {
		t.Errorf("status = %+v, %v", s, err)
	}

	plan, err = i.pipeline(nil).PlanProxyLAN(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.SetProxyLAN(ctx, false, plan); err != nil {
		t.Fatal(err)
	}
	if st := tunSwitch(t, i); st.ProxyLAN != nil {
		t.Error("switch still on")
	}
	if cfg, _ := os.ReadFile(i.Layout.CoreConfig()); !strings.Contains(string(cfg), "\nallow-lan: false\n") {
		t.Errorf("config:\n%s", cfg)
	}
}
