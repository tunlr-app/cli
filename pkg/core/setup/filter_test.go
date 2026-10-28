package setup

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/state"
)

func TestEditFilter(t *testing.T) {
	on, off := true, false
	f, err := EditFilter(state.Filter{}, FilterEdit{QUIC: &on, PortList: []string{"443"}})
	if err != nil || !f.QUIC || f.Ports || !slices.Equal(f.PortList, []string{"443"}) {
		t.Fatalf("f = %+v %v", f, err)
	}
	// 与默认相同的列表记为默认；关闭时列表保留。
	f, _ = EditFilter(f, FilterEdit{Ports: &on, PortList: pipeline.DefaultPorts})
	if !f.Ports || f.PortList != nil {
		t.Fatalf("default: %+v", f)
	}
	f, _ = EditFilter(state.Filter{Ports: true, PortList: []string{"80"}}, FilterEdit{Ports: &off})
	if f.Ports || !slices.Equal(f.PortList, []string{"80"}) {
		t.Fatalf("off: %+v", f)
	}

	// 来源地址段：加入已有的不变，去掉后为空；不修改原来的。
	orig := state.Filter{Sources: []string{"10.8.0.0/24"}}
	f, err = EditFilter(orig, FilterEdit{AddSource: "172.17.0.0/16"})
	if err != nil || !slices.Equal(f.Sources, []string{"10.8.0.0/24", "172.17.0.0/16"}) {
		t.Fatalf("add: %+v %v", f, err)
	}
	if f, _ = EditFilter(f, FilterEdit{AddSource: "172.17.0.0/16"}); len(f.Sources) != 2 {
		t.Fatalf("duplicate: %+v", f)
	}
	if f, _ = EditFilter(orig, FilterEdit{RemoveSource: "10.8.0.0/24"}); f.Sources != nil || len(orig.Sources) != 1 {
		t.Fatalf("remove: %+v, orig %+v", f, orig)
	}
	if f, _ = EditFilter(state.Filter{}, FilterEdit{AddSource: "192.168.1.5"}); !slices.Equal(f.Sources, []string{"192.168.1.5/32"}) {
		t.Fatalf("address: %+v", f)
	}
	for _, e := range []FilterEdit{{AddSource: "0.0.0.0/0"}, {AddSource: "nope"}, {AddSource: "172.17.0.9/16"},
		{RemoveSource: "10.9.0.0/24"}} {
		if _, err := EditFilter(orig, e); !errors.Is(err, core.ErrInvalidArgument) {
			t.Errorf("%+v: %v", e, err)
		}
	}
}

func TestSaveFilter(t *testing.T) {
	i, _ := newDevicesInstaller(t)
	ctx := context.Background()
	on := true
	before, after, plan, err := i.PlanFilter(ctx, FilterEdit{QUIC: &on})
	if err != nil {
		t.Fatal(err)
	}
	// 设为直连的设备在前，QUIC 紧随其后。
	rules := "  - SRC-IP-CIDR,192.168.1.50/32,DIRECT\n  - AND,((NETWORK,UDP),(DST-PORT,443)),REJECT\n"
	if before.QUIC || !after.QUIC || !plan.Changed() || !strings.Contains(string(plan.Config), rules) {
		t.Fatalf("plan:\n%s", plan.Config)
	}
	if _, err := i.SaveFilter(ctx, after, plan); err != nil {
		t.Fatal(err)
	}
	s, err := i.Filter()
	if err != nil || !s.QUIC || s.Ports || !s.DefaultPorts || len(s.Direct) != 2 || !s.Active() {
		t.Fatalf("status = %+v %v", s, err)
	}

	// 发布失败：过滤项恢复原状。
	_, after, plan, err = i.PlanFilter(ctx, FilterEdit{Ports: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(i.Layout.CoreConfig(), []byte("mode: rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := i.SaveFilter(ctx, after, plan); err == nil {
		t.Fatal("published over a changed configuration")
	}
	if s, _ := i.Filter(); s.Ports || !s.QUIC {
		t.Errorf("status = %+v", s)
	}
}
