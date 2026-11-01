package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// deviceEditView 是 devices name/route/forget 在 --json 输出中的形式。
type deviceEditView struct {
	Device   setup.Device     `json:"device"`
	Plan     *planView        `json:"plan,omitempty"`
	Applied  bool             `json:"applied"`
	Revision *config.Revision `json:"revision,omitempty"`
}

func (a *app) newDevicesCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "List the LAN devices that use this server; name, route, forget",
		Long: "Devices that use this server as their gateway or HTTP/SOCKS5 proxy are recorded as they send\n" +
			"traffic (nftables table inet " + netfilter.DevicesTable + ") and forgotten after 24 hours without any.\n" +
			"A device is online if it sent traffic in the last 5 minutes. Names come from tunlr devices name,\n" +
			"or from the router's reverse DNS (in parentheses). The list starts empty after a reboot.\n\n" +
			"All their traffic goes through mihomo and follows the rules, unless the device is set to go\n" +
			"direct for everything with tunlr devices route.\n\n" +
			"In a terminal the devices are shown in a table: choose one to name it, change how it goes or\n" +
			"forget it; a enters the address of a device that has not used this server yet. --all lists\n" +
			"the other devices on the LAN too (from the neighbor table), without the table to choose from.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all || !a.interactive() {
				return a.listDevices(cmd.Context(), all)
			}
			return a.manageDevices(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "also list other devices on the LAN (from the neighbor table)")
	cmd.AddCommand(a.newDevicesNameCommand(), a.withFullDiff(a.newDevicesRouteCommand()), a.withFullDiff(a.newDevicesForgetCommand()))
	return cmd
}

// loadDevices 同步记录设备的表（开关或代理端口可能变了）后列出设备。
func (a *app) loadDevices(ctx context.Context, all bool) (*setup.DeviceList, error) {
	inst := a.installer()
	if err := inst.SyncDevices(ctx); err != nil {
		a.log.Warn("update the nftables table that records LAN devices", "err", err)
	}
	return inst.Devices(ctx, setup.DeviceOptions{All: all, Names: true})
}

// splitDevices 把设备分为用本机的（包括 tunlr 记录的）和局域网中其他的（只在 --all 时有）。
func splitDevices(devices []setup.Device) (using, others []setup.Device) {
	for _, d := range devices {
		if d.State == setup.DeviceLAN {
			others = append(others, d)
		} else {
			using = append(using, d)
		}
	}
	return using, others
}

// devicesTitle 是设备表格的标题，按开着的是网关、代理端口还是都开着。
func devicesTitle(list *setup.DeviceList) string {
	switch {
	case list.Gateway && list.ProxyLAN:
		return "Devices using this server as their gateway or proxy"
	case list.Gateway:
		return "Devices using this server as their gateway"
	case list.ProxyLAN:
		return "Devices using this server's proxy port"
	}
	return "Devices recorded by tunlr"
}

// devicesOffNote 是网关和代理端口的局域网访问都关闭时的说明。
const devicesOffNote = "No LAN device can use this server: gateway mode and LAN access to the proxy port are both off\n" +
	"(tunlr tun gateway on, tunlr proxy lan on)."

func (a *app) listDevices(ctx context.Context, all bool) error {
	list, err := a.loadDevices(ctx, all)
	if err != nil {
		return err
	}
	return a.render(list, func() error {
		var b bytes.Buffer
		using, others := splitDevices(list.Devices)
		switch {
		case !list.Gateway && !list.ProxyLAN:
			fmt.Fprintln(&b, devicesOffNote)
		case !list.Tracking:
			fmt.Fprintf(&b, "%s the nftables table inet %s that records devices is missing (see tunlr doctor).\n", a.style.warn("Warning:"), netfilter.DevicesTable)
		}
		if len(using) > 0 || list.Gateway || list.ProxyLAN {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintln(&b, a.style.title(devicesTitle(list)+":"))
			if len(using) == 0 {
				fmt.Fprintln(&b, "none in the last 24 hours.")
			} else {
				header, rows := a.deviceTable(list, using, true)
				writeTable(&b, header, rows)
			}
		}
		if len(others) > 0 {
			fmt.Fprintf(&b, "\n%s\n", a.style.title("Other devices on the LAN (not using this server):"))
			rows := make([][]string, len(others))
			for i, d := range others {
				rows[i] = []string{a.deviceName(d), d.IP, orDash(d.MAC)}
			}
			writeTable(&b, []string{"NAME", "IP", "MAC"}, rows)
		}
		for _, d := range using {
			if d.RuleIP != "" && d.Route == setup.RouteDirect {
				fmt.Fprintf(&b, "\n%s %s now has the address %s, but its rule still matches %s;\n"+
					"tunlr devices route %s direct updates it.\n", a.style.warn("Warning:"), deviceLabel(d), d.IP, d.RuleIP, deviceRef(d))
			}
		}
		if len(using) > 0 || len(others) > 0 {
			more := ""
			if !all {
				more = "; --all also lists the other LAN devices"
			}
			fmt.Fprintf(&b, "\n(tunlr devices route <device> direct sends a device's traffic directly%s)\n", more)
		}
		_, err := a.stdout.Write(b.Bytes())
		return err
	})
}

// deviceTable 是设备表格的列名和各行。总是一样的列不列出：两种用法都开着时才有 USES；有设备直连时才有 ROUTE，
// 只在直连的设备上写 direct（其余按规则）。styled 时状态和直连带颜色（选择用的表格不带：所在行整行上色）。
func (a *app) deviceTable(list *setup.DeviceList, devices []setup.Device, styled bool) ([]string, [][]string) {
	uses := list.Gateway && list.ProxyLAN
	route := slices.ContainsFunc(devices, func(d setup.Device) bool { return d.Route == setup.RouteDirect })
	header := []string{"NAME", "IP", "MAC"}
	if uses {
		header = append(header, "USES")
	}
	header = append(header, "STATE", "LAST SEEN", "CONN")
	if route {
		header = append(header, "ROUTE")
	}
	now := time.Now()
	rows := make([][]string, len(devices))
	for i, d := range devices {
		state, direct := d.State, setup.RouteDirect
		if styled && state == setup.DeviceOnline {
			state = a.style.good(state)
		}
		if styled {
			direct = a.style.warn(direct)
		}
		row := []string{a.deviceName(d), d.IP, orDash(d.MAC)}
		if uses {
			row = append(row, orDash(strings.Join(d.Uses, ",")))
		}
		row = append(row, state, describeLastSeen(d.LastSeen, now), strconv.Itoa(d.Connections))
		if route {
			if d.Route == setup.RouteDirect {
				row = append(row, direct)
			} else {
				row = append(row, "-")
			}
		}
		rows[i] = row
	}
	return header, rows
}

func (a *app) deviceName(d setup.Device) string {
	switch {
	case d.Name == "":
		return "-"
	case d.NameFromDNS:
		return "(" + d.Name + ")"
	}
	return d.Name
}

// manageDevices 是终端中的 tunlr devices（主菜单的“LAN devices”）：在表格中选一台设备，再选做什么；做完回到
// 刷新过的表格，Esc 结束。
func (a *app) manageDevices(ctx context.Context) error {
	var last *setup.Device
	for {
		list, err := a.loadDevices(ctx, false)
		if err != nil {
			return err
		}
		using, _ := splitDevices(list.Devices)
		if !list.Gateway && !list.ProxyLAN && len(using) == 0 {
			_, err := fmt.Fprintln(a.stdout, devicesOffNote)
			return err
		}
		d, err := a.pickDevice(ctx, list, using, &last)
		if errors.Is(err, huh.ErrUserAborted) {
			// 浏览完离开不算取消，即使中途取消过某个操作。
			a.aborted = false
			return nil
		}
		if err != nil {
			return err
		}
		if d != nil {
			err := a.deviceAction(ctx, d)
			if errors.Is(err, core.ErrInvalidArgument) {
				// 如名称与别的设备重复：说明原因，回到表格。
				fmt.Fprintf(a.stdout, "%s %v\n", a.style.warn("Not changed:"), err)
			} else if err != nil {
				return err
			}
		}
		fmt.Fprintln(a.stdout)
	}
}

// pickDevice 在表格中选一台用本机的设备（using），光标停在 last（上次操作的设备，可以为 nil）；按 a 时输入地址，
// 指定还没用过本机的设备。在输入地址时取消返回 nil、nil（回到表格）。
func (a *app) pickDevice(ctx context.Context, list *setup.DeviceList, using []setup.Device, last **setup.Device) (*setup.Device, error) {
	header, rows := a.deviceTable(list, using, false)
	sum := &setup.DeviceSummary{}
	labels := make([]string, len(using))
	initial := 0
	for i, d := range using {
		switch d.State {
		case setup.DeviceOnline:
			sum.Online++
		case setup.DeviceIdle:
			sum.Idle++
		}
		labels[i] = deviceLabel(d)
		if l := *last; l != nil && (l.MAC != "" && d.MAC == l.MAC || d.IP == l.IP) {
			initial = i
		}
	}
	k, key, err := a.prompt.ChooseRow(ctx, rowChoice{
		title: devicesTitle(list), note: a.describeDevices(sum), header: header, rows: rows,
		labels: labels, initial: initial, empty: "None in the last 24 hours.",
		keys: []rowKey{{"a", "another device by its address"}},
	})
	if err != nil {
		return nil, err
	}
	if k >= 0 && k < len(using) {
		*last = &using[k]
	}
	if key == "" {
		return &using[k], nil
	}
	var d *setup.Device
	_, err = a.prompt.Input(ctx, "Address of the device", "A private IPv4 address on the LAN; reserve it for the device on your router.", "",
		func(s string) error {
			found, err := list.FindDevice(s)
			d = found
			return err
		})
	if errors.Is(err, huh.ErrUserAborted) {
		return nil, nil
	}
	return d, err
}

// deviceAction 让用户选择对设备 d 做什么：起名、改走法、忘记它的记录；Esc 或 Back 回到表格。
func (a *app) deviceAction(ctx context.Context, d *setup.Device) error {
	name := "Name it"
	if d.Name != "" && !d.NameFromDNS {
		name = "Rename it (now " + d.Name + "); an empty name removes it"
	}
	route, to := "Go direct for everything, skipping the rules", setup.RouteDirect
	if d.Route == setup.RouteDirect {
		route, to = "Follow the rules again (now direct for everything)", setup.RouteProxy
	}
	forget := "Forget it: drop its name and setting"
	options := []string{name, route}
	if d.Saved {
		options = append(options, forget)
	}
	options = append(options, menuBack)
	k, err := a.prompt.Choose(ctx, "What to do with "+deviceLabel(*d)+"?", options, 0)
	if errors.Is(err, huh.ErrUserAborted) {
		return nil
	}
	if err != nil {
		return err
	}
	switch options[k] {
	case name:
		return a.nameDevice(ctx, d, nil)
	case route:
		return a.routeDevice(ctx, d, to)
	case forget:
		return a.forgetDevice(ctx, d)
	}
	return nil
}

// describeLastSeen 用相对时间描述最后出现的时间。
func describeLastSeen(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	switch d := now.Sub(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// deviceLabel 是输出中称呼设备的方式：有名称时“名称 (地址)”，否则地址。
func deviceLabel(d setup.Device) string {
	if d.Name != "" {
		return fmt.Sprintf("%s (%s)", d.Name, d.IP)
	}
	return d.IP
}

// deviceRef 是在命令中指代设备的参数：自定义名称，否则 MAC，再否则地址。
func deviceRef(d setup.Device) string {
	switch {
	case d.Name != "" && !d.NameFromDNS:
		if strings.ContainsAny(d.Name, " '\"") {
			return fmt.Sprintf("%q", d.Name)
		}
		return d.Name
	case d.MAC != "":
		return d.MAC
	}
	return d.IP
}

// chooseDevice 取得命令要操作的设备：给出时在全部设备（含局域网中其他的）中查找，否则在终端里从用本机的设备的
// 表格中选择（同 tunlr devices，a 输入地址）。
func (a *app) chooseDevice(ctx context.Context, command string, args []string) (*setup.Device, error) {
	if len(args) == 0 && !a.interactive() {
		return nil, usageErrorf("%s needs the device (an address, MAC address or name)", command)
	}
	list, err := a.loadDevices(ctx, len(args) > 0)
	if err != nil {
		return nil, err
	}
	if len(args) > 0 {
		return list.FindDevice(args[0])
	}
	using, _ := splitDevices(list.Devices)
	var last *setup.Device
	d, err := a.pickDevice(ctx, list, using, &last)
	if d == nil && err == nil {
		return nil, huh.ErrUserAborted
	}
	return d, err
}

func (a *app) newDevicesNameCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "name [device] [name]",
		Short: "Give a device a name (an empty name removes it)",
		Args:  usageArgs(cobra.MaximumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 && !a.interactive() {
				return usageErrorf("devices name needs the device and the name")
			}
			ctx := cmd.Context()
			d, err := a.chooseDevice(ctx, "devices name", args)
			if stop, err := a.cancelled(err); stop {
				return err
			}
			var name *string
			if len(args) == 2 {
				name = &args[1]
			}
			return a.nameDevice(ctx, d, name)
		},
	}
}

// nameDevice 给设备 d 起名（空字符串去掉名称）；name 为 nil 时询问，预先填好现在的名称。
func (a *app) nameDevice(ctx context.Context, d *setup.Device, name *string) error {
	if name == nil {
		current := ""
		if !d.NameFromDNS {
			current = d.Name
		}
		s, err := a.prompt.Input(ctx, "Name for "+deviceLabel(*d), "Up to 32 characters; leave empty to remove the name.", current,
			func(s string) error {
				if s == "" {
					return nil
				}
				return setup.ValidateDeviceName(s)
			})
		if stop, err := a.cancelled(err); stop {
			return err
		}
		name = &s
	}
	return a.editDevice(ctx, d, setup.DeviceEdit{Name: name}, "Publish the configuration?", func(d setup.Device) string {
		if *name == "" {
			return fmt.Sprintf("%s has no name now.", d.IP)
		}
		return fmt.Sprintf("%s is now called %s.", d.IP, *name)
	})
}

func (a *app) newDevicesRouteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "route [device] [proxy|direct]",
		Short: "Set whether a device follows the rules (proxy) or goes direct for everything",
		Long: "The device is an address, a MAC address or a name (see tunlr devices). An address no device\n" +
			"has used yet can be set in advance. The rule matches the device's address (mihomo cannot match\n" +
			"by MAC address), so reserve the address for the device on your router. Setting it to proxy\n" +
			"again removes the rule. The configuration is published after the diff is confirmed. The rule\n" +
			"goes before all others (tunlr filter lists it with the other filters).",
		Args: usageArgs(cobra.MaximumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 && args[1] != setup.RouteProxy && args[1] != setup.RouteDirect {
				return usageErrorf("the route must be proxy or direct, got %q", args[1])
			}
			if len(args) < 2 && !a.interactive() {
				return usageErrorf("devices route needs the device and the route (proxy or direct)")
			}
			if err := a.requireConfirmation("devices route"); err != nil {
				return err
			}
			ctx := cmd.Context()
			d, err := a.chooseDevice(ctx, "devices route", args)
			if stop, err := a.cancelled(err); stop {
				return err
			}
			route := ""
			if len(args) == 2 {
				route = args[1]
			} else {
				routes := []string{setup.RouteProxy, setup.RouteDirect}
				initial := 1
				if d.Route == setup.RouteDirect {
					initial = 0
				}
				k, err := a.prompt.Choose(ctx, "How should "+deviceLabel(*d)+" go out?", []string{
					"proxy   follow the rules (default)", "direct  everything direct, no proxies"}, initial)
				if stop, err := a.cancelled(err); stop {
					return err
				}
				route = routes[k]
			}
			return a.routeDevice(ctx, d, route)
		},
	}
}

// routeDevice 把设备 d 的走法设为 route（RouteProxy 或 RouteDirect）。
func (a *app) routeDevice(ctx context.Context, d *setup.Device, route string) error {
	return a.editDevice(ctx, d, setup.DeviceEdit{Route: &route}, "Publish the configuration with this device's route?",
		func(d setup.Device) string {
			if route == setup.RouteDirect {
				return fmt.Sprintf("%s now goes direct for everything.\n"+
					"The rule matches %s: reserve this address for the device on your router (DHCP reservation)\n"+
					"so it keeps it.", deviceLabel(d), d.IP)
			}
			return fmt.Sprintf("%s follows the rules again.", deviceLabel(d))
		})
}

func (a *app) newDevicesForgetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "forget [device]",
		Short: "Remove what tunlr records about a device (its name; it follows the rules again)",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			d, err := a.chooseDevice(ctx, "devices forget", args)
			if stop, err := a.cancelled(err); stop {
				return err
			}
			return a.forgetDevice(ctx, d)
		},
	}
}

// forgetDevice 删除 tunlr 对设备 d 的记录（名称和走法）。
func (a *app) forgetDevice(ctx context.Context, d *setup.Device) error {
	if !d.Saved {
		return a.render(deviceEditView{Device: *d}, func() error {
			_, err := fmt.Fprintf(a.stdout, "Nothing is recorded about %s.\n", deviceLabel(*d))
			return err
		})
	}
	return a.editDevice(ctx, d, setup.DeviceEdit{Forget: true}, "Publish the configuration without this device's rule?",
		func(setup.Device) string { return fmt.Sprintf("Forgot %s.", deviceLabel(*d)) })
}

// editDevice 是修改设备记录的流程：合成新配置；配置没有变化时直接保存记录，否则预览、确认后与记录一起发布。
func (a *app) editDevice(ctx context.Context, d *setup.Device, edit setup.DeviceEdit, question string, done func(setup.Device) string) error {
	inst := a.installer()
	var records []state.Device
	plan, err := a.plan(func() (p *pipeline.Plan, err error) {
		records, p, err = inst.PlanDeviceEdit(ctx, d, edit)
		return p, err
	})
	if err != nil {
		return err
	}
	after := *d
	if edit.Name != nil {
		after.Name, after.NameFromDNS = *edit.Name, false
	}
	if edit.Route != nil {
		after.Route = *edit.Route
	}
	v := deviceEditView{Device: after}
	if !plan.Changed() {
		if a.opts.DryRun {
			return a.render(v, func() error {
				_, err := fmt.Fprintln(a.stdout, "Dry run: nothing changed (the configuration stays the same).")
				return err
			})
		}
		if _, err := inst.SaveDevices(ctx, records, nil); err != nil {
			return err
		}
		return a.render(v, func() error {
			_, err := fmt.Fprintln(a.stdout, done(after))
			return err
		})
	}
	pv := newPlanView(plan, false)
	v.Plan = &pv
	if !a.opts.JSON {
		a.printPlan(pv)
	}
	if err := checkExit(plan, false); err != nil || a.opts.DryRun {
		if a.opts.JSON {
			if rerr := a.render(v, nil); rerr != nil {
				return rerr
			}
		} else if err != nil {
			fmt.Fprintln(a.stdout, "\nNot changed.")
		} else {
			fmt.Fprintln(a.stdout, "\nDry run: nothing changed.")
		}
		return err
	}
	if err := a.requireConfirmation("this change"); err != nil {
		return err
	}
	if ok, err := a.ask(ctx, question, "", "Not changed."); !ok {
		return err
	}
	err = a.spin("Publishing the configuration", func() (err error) {
		v.Revision, err = inst.SaveDevices(ctx, records, plan)
		return err
	})
	if errors.Is(err, core.ErrRolledBack) {
		return fmt.Errorf("%w; the previous configuration is back in place", err)
	}
	if err != nil {
		return err
	}
	v.Applied = true
	return a.render(v, func() error {
		fmt.Fprintf(a.stdout, "\n%s\n", a.style.good(done(after)))
		if v.Revision != nil {
			fmt.Fprintf(a.stdout, "Published revision %d.\n", v.Revision.Number)
		}
		return nil
	})
}
