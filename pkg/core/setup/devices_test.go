package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/state"
)

const (
	macPhone  = "aa:bb:cc:dd:ee:01"
	macTV     = "aa:bb:cc:dd:ee:02"
	macLaptop = "aa:bb:cc:dd:ee:03"
)

// seenSet 生成 nft -j list set 的输出，elems 为 MAC、地址和距今的秒数。
func seenSet(elems ...any) string {
	var list []string
	for k := 0; k+2 < len(elems); k += 3 {
		list = append(list, fmt.Sprintf(`{"elem": {"val": {"concat": [%q, %q]}, "timeout": 86400, "expires": %d}}`,
			elems[k], elems[k+1], 86400-elems[k+2].(int)))
	}
	return `{"nftables": [{"set": {"timeout": 86400, "elem": [` + strings.Join(list, ", ") + `]}}]}`
}

func newDevicesInstaller(t *testing.T) (*Installer, *fakeHost) {
	t.Helper()
	i, h := newGatewayInstaller(t, "1")
	gatewayOn(t, i, "ens3")
	fakeNIC(t, i.Layout, "ens3", true)
	fakeNIC(t, i.Layout, "docker0", false, "veth1")
	h.seen = map[string]string{
		"gateway": seenSet(macPhone, "192.168.1.5", 30, macTV, "192.168.1.6", 7200),
		"proxy":   seenSet(macPhone, "192.168.1.5", 10),
	}
	h.neigh = `[{"dst":"192.168.1.5","dev":"ens3","lladdr":"` + macPhone + `","state":["REACHABLE"]},
		{"dst":"192.168.1.7","dev":"ens3","lladdr":"` + macLaptop + `","state":["STALE"]},
		{"dst":"172.17.0.2","dev":"docker0","lladdr":"02:42:ac:11:00:02","state":["REACHABLE"]}]`
	i.Connections = func(context.Context) (map[string]int, error) { return map[string]int{"192.168.1.5": 3}, nil }
	i.LookupNames = func(_ context.Context, ips []string) map[string]string {
		return map[string]string{"192.168.1.5": "phone", "192.168.1.7": "laptop"}
	}
	st := tunSwitch(t, i)
	st.Devices = []state.Device{
		{MAC: macTV, IP: "192.168.1.9", Name: "tv", Route: state.RouteDirect},
		{IP: "192.168.1.50", Route: state.RouteDirect},
	}
	if err := state.Save(i.Layout.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	return i, h
}

func describeDevices(list *DeviceList) string {
	var rows []string
	for _, d := range list.Devices {
		rows = append(rows, fmt.Sprintf("%s %s %s %s %v %s %d %s saved=%v rule=%s",
			d.Name, d.IP, d.MAC, d.State, d.Uses, d.LastSeen.Format("15:04:05"), d.Connections, d.Route, d.Saved, d.RuleIP))
	}
	return strings.Join(rows, "\n")
}

func TestDevices(t *testing.T) {
	i, _ := newDevicesInstaller(t)
	ctx := context.Background()
	list, err := i.Devices(ctx, DeviceOptions{Names: true})
	if err != nil {
		t.Fatal(err)
	}
	if !list.Gateway || list.ProxyLAN || !list.Tracking {
		t.Errorf("list = %+v", list)
	}
	// 在线的在前；记录中的电视按 MAC 认出，地址变了；预先设置、没来过的设备在后。
	want := "phone 192.168.1.5 aa:bb:cc:dd:ee:01 online [gateway proxy] 08:59:50 3 proxy saved=false rule=\n" +
		"tv 192.168.1.6 aa:bb:cc:dd:ee:02 idle [gateway] 07:00:00 0 direct saved=true rule=192.168.1.9\n" +
		" 192.168.1.50  away [] 00:00:00 0 direct saved=true rule="
	if got := describeDevices(list); got != want {
		t.Fatalf("devices:\n%s\nwant:\n%s", got, want)
	}
	if !list.Devices[0].NameFromDNS || list.Devices[1].NameFromDNS {
		t.Errorf("name sources: %+v", list.Devices[:2])
	}

	// --all 加上邻居表中没有使用本机的设备，容器网卡上的不算。
	list, err = i.Devices(ctx, DeviceOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(list.Devices); n != 4 || list.Devices[3].IP != "192.168.1.7" || list.Devices[3].State != DeviceLAN || list.Devices[3].Name != "" {
		t.Fatalf("all:\n%s", describeDevices(list))
	}

	// status 的摘要。
	s := i.deviceSummary(ctx, tunSwitch(t, i))
	if s == nil || *s != (DeviceSummary{Online: 1, Idle: 1}) {
		t.Errorf("summary = %+v", s)
	}
}

func TestFindDevice(t *testing.T) {
	i, _ := newDevicesInstaller(t)
	list, err := i.Devices(context.Background(), DeviceOptions{Names: true})
	if err != nil {
		t.Fatal(err)
	}
	for query, ip := range map[string]string{
		"TV": "192.168.1.6", "AA-BB-CC-DD-EE-02": "192.168.1.6", "phone": "192.168.1.5",
		"192.168.1.50": "192.168.1.50", "192.168.1.99": "192.168.1.99",
	} {
		if d, err := list.FindDevice(query); err != nil || d.IP != ip {
			t.Errorf("%s: %+v %v", query, d, err)
		}
	}
	for _, query := range []string{"8.8.8.8", "nope", "aa:bb:cc:dd:ee:99"} {
		if _, err := list.FindDevice(query); !errors.Is(err, core.ErrInvalidArgument) {
			t.Errorf("%s: %v", query, err)
		}
	}
}

func TestEditDevices(t *testing.T) {
	records := []state.Device{
		{MAC: macTV, IP: "192.168.1.9", Name: "tv", Route: state.RouteDirect},
		{IP: "192.168.1.50", Route: state.RouteDirect},
	}
	phone := &Device{MAC: macPhone, IP: "192.168.1.5"}
	tv := &Device{MAC: macTV, IP: "192.168.1.6", RuleIP: "192.168.1.9", Name: "tv"}
	direct, proxy, empty := RouteDirect, RouteProxy, ""

	got, err := EditDevices(records, phone, DeviceEdit{Route: &direct})
	if err != nil || len(got) != 3 || got[2] != (state.Device{MAC: macPhone, IP: "192.168.1.5", Route: state.RouteDirect}) {
		t.Fatalf("set direct: %+v %v", got, err)
	}
	if records[0].IP != "192.168.1.9" || len(records) != 2 {
		t.Fatalf("records modified: %+v", records)
	}
	// 再设为 proxy 时记录中的地址更新为现在的地址；名称保留。
	got, err = EditDevices(records, tv, DeviceEdit{Route: &proxy})
	if err != nil || !slices.Equal(got, []state.Device{{MAC: macTV, IP: "192.168.1.6", Name: "tv"}, records[1]}) {
		t.Fatalf("set proxy: %+v %v", got, err)
	}
	// 没有名称、走法为默认时去掉记录。
	got, err = EditDevices(got, tv, DeviceEdit{Name: &empty})
	if err != nil || !slices.Equal(got, records[1:]) {
		t.Fatalf("remove name: %+v %v", got, err)
	}
	if got, err = EditDevices(records, &Device{IP: "192.168.1.50"}, DeviceEdit{Forget: true}); err != nil || !slices.Equal(got, records[:1]) {
		t.Fatalf("forget: %+v %v", got, err)
	}
	for _, name := range []string{"TV", " x", "192.168.1.1", strings.Repeat("a", 33), "a\tb"} {
		if _, err := EditDevices(records, phone, DeviceEdit{Name: &name}); !errors.Is(err, core.ErrInvalidArgument) {
			t.Errorf("name %q: %v", name, err)
		}
	}
	bad := "tunnel"
	if _, err := EditDevices(records, phone, DeviceEdit{Route: &bad}); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("route: %v", err)
	}
}

func TestSaveDevices(t *testing.T) {
	i, _ := newDevicesInstaller(t)
	ctx := context.Background()
	phone := &Device{MAC: macPhone, IP: "192.168.1.5"}
	direct := RouteDirect
	records, plan, err := i.PlanDeviceEdit(ctx, phone, DeviceEdit{Route: &direct})
	if err != nil {
		t.Fatal(err)
	}
	// 规则在最前，按记录的顺序。
	rules := "rules:\n  - SRC-IP-CIDR,192.168.1.9/32,DIRECT\n  - SRC-IP-CIDR,192.168.1.50/32,DIRECT\n  - SRC-IP-CIDR,192.168.1.5/32,DIRECT\n"
	if !plan.Changed() || !strings.Contains(string(plan.Config), rules) {
		t.Fatalf("config:\n%s", plan.Config)
	}
	rev, err := i.SaveDevices(ctx, records, plan)
	if err != nil || rev == nil {
		t.Fatalf("save: %v %v", rev, err)
	}
	if st := tunSwitch(t, i); len(st.Devices) != 3 {
		t.Errorf("records = %+v", st.Devices)
	}
	if data, _ := os.ReadFile(i.Layout.CoreConfig()); !strings.Contains(string(data), rules) {
		t.Errorf("published:\n%s", data)
	}

	// 只起名：配置不变，只保存记录。
	name := "phone"
	records, plan, err = i.PlanDeviceEdit(ctx, phone, DeviceEdit{Name: &name})
	if err != nil || plan.Changed() {
		t.Fatalf("name: %v %v", plan.Changed(), err)
	}
	if rev, err := i.SaveDevices(ctx, records, plan); err != nil || rev != nil {
		t.Fatalf("save name: %v %v", rev, err)
	}
	if st := tunSwitch(t, i); st.Devices[2].Name != "phone" {
		t.Errorf("records = %+v", st.Devices)
	}

	// 发布失败：记录恢复原状。
	before := tunSwitch(t, i).Devices
	records, plan, err = i.PlanDeviceEdit(ctx, phone, DeviceEdit{Forget: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(i.Layout.CoreConfig(), []byte("mode: rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := i.SaveDevices(ctx, records, plan); err == nil {
		t.Fatal("published over a changed configuration")
	}
	if st := tunSwitch(t, i); !slices.Equal(st.Devices, before) {
		t.Errorf("records = %+v, want %+v", st.Devices, before)
	}
}
