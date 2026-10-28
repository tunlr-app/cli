package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/netfilter"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// deviceEditView 是 devices set/name/forget 在 --json 输出中的形式。
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
		Short: "List the LAN devices that use this server; set them to go through proxies or directly",
		Long: "Devices that use this server as their gateway or HTTP/SOCKS5 proxy are recorded as they send\n" +
			"traffic (nftables table inet " + netfilter.DevicesTable + ") and forgotten after 24 hours without any.\n" +
			"A device is online if it sent traffic in the last 5 minutes. Names come from tunlr devices name,\n" +
			"or from the router's reverse DNS. The list starts empty after a reboot.\n\n" +
			"All their traffic goes through mihomo. By default it follows the rules (proxy); a device set to\n" +
			"direct goes direct for everything, by a rule matching its address placed before all others\n" +
			"(the managed mixin tunlr:devices). mihomo cannot match by MAC address, so reserve the address\n" +
			"for the device on your router.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error { return a.listDevices(cmd.Context(), all) },
	}
	cmd.Flags().BoolVar(&all, "all", false, "also list other devices on the LAN (from the neighbor table)")
	cmd.AddCommand(a.withFullDiff(a.newDevicesSetCommand()), a.newDevicesNameCommand(), a.withFullDiff(a.newDevicesForgetCommand()))
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

func (a *app) listDevices(ctx context.Context, all bool) error {
	list, err := a.loadDevices(ctx, all)
	if err != nil {
		return err
	}
	return a.render(list, func() error {
		var b bytes.Buffer
		switch {
		case !list.Gateway && !list.ProxyLAN:
			fmt.Fprintln(&b, "No LAN device can use this server: gateway mode and LAN access to the proxy port are both off\n"+
				"(tunlr gateway on, tunlr proxy lan on).")
		case !list.Tracking:
			fmt.Fprintf(&b, "%s the nftables table inet %s that records devices is missing (see tunlr doctor).\n", a.style.warn("Warning:"), netfilter.DevicesTable)
		}
		if len(list.Devices) == 0 {
			if list.Gateway || list.ProxyLAN {
				fmt.Fprintln(&b, "No device has used this server in the last 24 hours.")
			}
			_, err := a.stdout.Write(b.Bytes())
			return err
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		a.writeDevices(&b, list.Devices)
		for _, d := range list.Devices {
			if d.RuleIP != "" && d.Route == setup.RouteDirect {
				fmt.Fprintf(&b, "\n%s %s now has the address %s, but its rule still matches %s;\n"+
					"tunlr devices set %s direct updates it.", a.style.warn("Warning:"), deviceLabel(d), d.IP, d.RuleIP, deviceRef(d))
			}
		}
		fmt.Fprintln(&b, "\n(tunlr devices set <device> direct sends a device's traffic directly; --all also lists other LAN devices)")
		_, err := a.stdout.Write(b.Bytes())
		return err
	})
}

func (a *app) writeDevices(b *bytes.Buffer, devices []setup.Device) {
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tIP\tMAC\tUSES\tSTATE\tLAST SEEN\tCONN\tROUTE")
	for _, d := range devices {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", a.deviceName(d), d.IP, orDash(d.MAC), orDash(strings.Join(d.Uses, ",")),
			a.describeDeviceState(d.State), describeLastSeen(d.LastSeen, time.Now()), d.Connections, a.describeRoute(d.Route))
	}
	tw.Flush()
}

func (a *app) deviceName(d setup.Device) string {
	switch {
	case d.Name == "":
		return "-"
	case d.NameFromDNS:
		// 暗色的控制符长度固定，同一列中混用时 tabwriter 会差几格，因此只用括号区分。
		return "(" + d.Name + ")"
	}
	return d.Name
}

func (a *app) describeDeviceState(s string) string {
	switch s {
	case setup.DeviceOnline:
		return a.style.good(s)
	case setup.DeviceLAN:
		return "not using"
	}
	return s
}

func (a *app) describeRoute(r string) string {
	if r == setup.RouteDirect {
		return a.style.warn(r)
	}
	return r
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

// chooseDevice 取得命令要操作的设备：给出时在列表中查找，否则在终端里从列表选择（光标在最近出现的设备上）。
func (a *app) chooseDevice(ctx context.Context, command string, args []string) (*setup.Device, error) {
	if len(args) == 0 && !a.interactive() {
		return nil, usageErrorf("devices %s needs the device (an address, MAC address or name)", command)
	}
	list, err := a.loadDevices(ctx, true)
	if err != nil {
		return nil, err
	}
	if len(args) > 0 {
		return list.FindDevice(args[0])
	}
	if len(list.Devices) == 0 {
		return nil, usageErrorf("no device is known yet; give its address: tunlr devices %s <address>", command)
	}
	rows := make([][]string, len(list.Devices))
	for k, d := range list.Devices {
		rows[k] = []string{a.deviceName(d), d.IP, orDash(d.MAC), a.describeDeviceState(d.State), d.Route}
	}
	k, err := a.prompt.Choose(ctx, "Which device?", columns(rows), 0)
	if err != nil {
		return nil, err
	}
	return &list.Devices[k], nil
}

func (a *app) newDevicesSetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set [device] [proxy|direct]",
		Short: "Set whether a device follows the rules (proxy) or goes direct for everything",
		Long: "The device is an address, a MAC address or a name. An address no device has used yet can be set\n" +
			"in advance. Setting it to proxy again removes the rule. The configuration is published after\n" +
			"the diff is confirmed.",
		Args: usageArgs(cobra.MaximumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 && args[1] != setup.RouteProxy && args[1] != setup.RouteDirect {
				return usageErrorf("the route must be proxy or direct, got %q", args[1])
			}
			if len(args) < 2 && !a.interactive() {
				return usageErrorf("devices set needs the device and the route (proxy or direct)")
			}
			if err := a.requireConfirmation("devices set"); err != nil {
				return err
			}
			ctx := cmd.Context()
			d, err := a.chooseDevice(ctx, "set", args)
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
			return a.editDevice(ctx, d, setup.DeviceEdit{Route: &route}, "Publish the configuration with this device's route?",
				func(d setup.Device) string {
					if route == setup.RouteDirect {
						return fmt.Sprintf("%s now goes direct for everything.\n"+
							"The rule matches %s: reserve this address for the device on your router (DHCP reservation)\n"+
							"so it keeps it.", deviceLabel(d), d.IP)
					}
					return fmt.Sprintf("%s follows the rules again.", deviceLabel(d))
				})
		},
	}
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
			d, err := a.chooseDevice(ctx, "name", args)
			if stop, err := a.cancelled(err); stop {
				return err
			}
			var name string
			if len(args) == 2 {
				name = args[1]
			} else {
				name, err = a.prompt.Input(ctx, "Name for "+deviceLabel(*d), "Up to 32 characters; leave empty to remove the name.",
					func(s string) error {
						if s == "" {
							return nil
						}
						return setup.ValidateDeviceName(s)
					})
				if stop, err := a.cancelled(err); stop {
					return err
				}
			}
			return a.editDevice(ctx, d, setup.DeviceEdit{Name: &name}, "Publish the configuration?", func(d setup.Device) string {
				if name == "" {
					return fmt.Sprintf("%s has no name now.", d.IP)
				}
				return fmt.Sprintf("%s is now called %s.", d.IP, name)
			})
		},
	}
}

func (a *app) newDevicesForgetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "forget [device]",
		Short: "Remove what tunlr records about a device (its name; it follows the rules again)",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			d, err := a.chooseDevice(ctx, "forget", args)
			if stop, err := a.cancelled(err); stop {
				return err
			}
			if !d.Saved {
				return a.render(deviceEditView{Device: *d}, func() error {
					_, err := fmt.Fprintf(a.stdout, "Nothing is recorded about %s.\n", deviceLabel(*d))
					return err
				})
			}
			return a.editDevice(ctx, d, setup.DeviceEdit{Forget: true}, "Publish the configuration without this device's rule?",
				func(setup.Device) string { return fmt.Sprintf("Forgot %s.", deviceLabel(*d)) })
		},
	}
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
