package setup

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/service"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// saveLANAccess 记下面板的局域网访问开着、放行 ens3，模拟 panel lan on 之后的状态。
func saveLANAccess(t *testing.T, i *Installer) {
	t.Helper()
	st, _, err := state.LoadOrNew(i.Layout.StateFile(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	st.PanelLAN = &state.PanelLAN{Since: testNow}
	st.LANAccess = &state.LANAccess{Interfaces: []string{"ens3"}}
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
}

// TestFirewall 验证 nftables.service 重新加载之后，_firewall 按状态补上 tunlr 的表：访问限制，内核在运行时
// 还有 inet tunlr；不重启内核。
func TestFirewall(t *testing.T) {
	ctx := context.Background()
	i, h, _ := newTunInstaller(t)
	saveLANAccess(t, i)
	h.active = true

	res, err := i.Firewall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.tables, netfilter.AccessTable) || !slices.Contains(h.tables, netfilter.Table) || len(res.Actions) != 2 {
		t.Errorf("actions %q, tables %v", res.Actions, h.tables)
	}
	if !strings.Contains(h.access, "elements = { 9090 }") || h.called("systemctl restart") {
		t.Errorf("calls %q, access table:\n%s", h.calls, h.access)
	}
	// 表都在时仍然重新加载访问限制（端口可能变了），但不算作补上。
	if res, err := i.Firewall(ctx); err != nil || len(res.Actions) != 0 {
		t.Errorf("second run: %+v, %v", res, err)
	}

	// 什么都没开时什么都不做。
	i2, h2, _ := newTunInstaller(t)
	if res, err := i2.Firewall(ctx); err != nil || len(res.Actions) != 0 || h2.called("nft -f") {
		t.Errorf("nothing on: %+v, %v, %q", res, err, h2.calls)
	}
}

// TestCheckLANAccessOnly 验证只开着局域网访问时 _check 不停用自己，补上访问限制的表，并拉起没在运行的
// tunlr-firewall.service（nftables.service 停过再启动时 PartOf 不会启动它）。
func TestCheckLANAccessOnly(t *testing.T) {
	ctx := context.Background()
	i, h, _ := newTunInstaller(t)
	saveLANAccess(t, i)

	res, err := i.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.called("systemctl disable --now "+service.CheckTimer) || !slices.Contains(h.tables, netfilter.AccessTable) ||
		!h.called("systemctl enable --now "+service.FirewallUnit) ||
		!slices.Equal(res.Actions, []string{"reloaded the nftables table inet " + netfilter.AccessTable, "started " + service.FirewallUnit}) {
		t.Errorf("actions %q, calls %q", res.Actions, h.calls)
	}

	// 单元在运行时不再动它。
	h.active = true
	h.calls = nil
	if _, err := i.Check(ctx); err != nil || h.called("systemctl enable --now "+service.FirewallUnit) {
		t.Errorf("active unit: %v, calls %q", err, h.calls)
	}
}
