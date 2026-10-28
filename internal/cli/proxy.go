package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
)

// proxyLANView 是 proxy lan on/off 在 --json 输出中的形式。
type proxyLANView struct {
	Plan      *planView             `json:"plan,omitempty"`
	Preflight *setup.ProxyLANPlan   `json:"preflight,omitempty"`
	Applied   bool                  `json:"applied"`
	Result    *setup.ProxyLANResult `json:"result,omitempty"`
}

func (a *app) newProxyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Show the proxy address for other devices; lan on/off lets LAN devices use it",
		Long: "mihomo's proxy port (mixed-port, HTTP and SOCKS5 on one port) listens on 127.0.0.1 only by\n" +
			"default. With LAN access on (tunlr proxy lan on), devices at home can set this server as\n" +
			"their HTTP or SOCKS5 proxy; mihomo refuses connections from addresses that are not private.\n" +
			"To route devices through this server without setting a proxy on each, see tunlr tun gateway.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error { return a.showProxyLAN(cmd.Context()) },
	}
	lan := &cobra.Command{
		Use:   "lan",
		Short: "Show whether devices on the LAN can use the proxy port; turn it on or off",
		Long: "With LAN access on, the configuration has allow-lan: true and lan-allowed-ips limited to\n" +
			"private addresses (10/8, 172.16/12, 192.168/16, 100.64/10 including Tailscale, 169.254/16,\n" +
			"this server itself; IPv6 link-local and fc00::/7), so no authentication is needed at home.\n" +
			"Both keys are managed by tunlr; mixins cannot set them. Add authentication in a mixin if\n" +
			"you want a password anyway.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error { return a.showProxyLAN(cmd.Context()) },
	}
	lan.AddCommand(a.withFullDiff(a.newProxyLANSwitchCommand(true)), a.withFullDiff(a.newProxyLANSwitchCommand(false)))
	cmd.AddCommand(lan, a.newProxyEnvCommand())
	return cmd
}

// proxyEnvView 是 proxy env 在 --json 输出中的形式。
type proxyEnvView struct {
	Variables []setup.EnvVar `json:"variables,omitempty"`
	Unset     []string       `json:"unset,omitempty"`
}

func (a *app) newProxyEnvCommand() *cobra.Command {
	var unset bool
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Print the shell commands that make programs in the current shell use the proxy port",
		Long: "A program cannot change the environment of the shell that runs it, so this prints export\n" +
			"statements for the shell to run:\n\n" +
			"  eval \"$(tunlr proxy env)\"           # as root\n" +
			"  eval \"$(sudo tunlr proxy env)\"      # as another user\n" +
			"  eval \"$(tunlr proxy env --unset)\"   # stop using the proxy\n\n" +
			"http_proxy and https_proxy point to the proxy port (mixed-port) as an HTTP proxy, all_proxy as\n" +
			"SOCKS5 (socks5h: names are resolved by the proxy), each also in upper case; no_proxy keeps this\n" +
			"server and private addresses direct. Only the current shell and the programs it starts are\n" +
			"affected. With the transparent proxy on (tunlr tun on), this server's traffic goes through\n" +
			"mihomo anyway.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if unset {
				return a.render(proxyEnvView{Unset: setup.ProxyEnvNames}, func() error {
					_, err := fmt.Fprintf(a.stdout, "# eval \"$(tunlr proxy env --unset)\" removes these from the current shell\nunset %s\n",
						strings.Join(setup.ProxyEnvNames, " "))
					return err
				})
			}
			vars, err := a.installer().ProxyEnv()
			if errors.Is(err, setup.ErrConfigUnreadable) {
				return fmt.Errorf("%w (it holds node passwords); run: eval \"$(sudo tunlr proxy env)\"", err)
			}
			if err != nil {
				return err
			}
			return a.render(proxyEnvView{Variables: vars}, func() error {
				var b strings.Builder
				b.WriteString("# eval \"$(tunlr proxy env)\" sets these in the current shell; --unset removes them\n")
				for _, v := range vars {
					fmt.Fprintf(&b, "export %s='%s'\n", v.Name, v.Value)
				}
				_, err := io.WriteString(a.stdout, b.String())
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&unset, "unset", false, "print the command that removes the variables instead")
	return cmd
}

func (a *app) showProxyLAN(ctx context.Context) error {
	pre, err := a.installer().ProxyLANPreflight(ctx)
	if err != nil {
		return err
	}
	return a.render(pre, func() error {
		fmt.Fprintf(a.stdout, "Proxy LAN access: %s\n", a.describeProxyLAN(pre.On))
		if pre.On {
			a.printProxyAddresses(pre)
			fmt.Fprintln(a.stdout, "(tunlr proxy lan off lets only this server use it)")
		} else {
			if pre.Port > 0 {
				fmt.Fprintf(a.stdout, "The proxy port is 127.0.0.1:%d, only reachable from this server.\n", pre.Port)
			}
			fmt.Fprintln(a.stdout, "(tunlr proxy lan on lets devices on the LAN use it)")
		}
		return nil
	})
}

// printProxyAddresses 输出局域网设备要填写的代理地址。
func (a *app) printProxyAddresses(pre *setup.ProxyLANPlan) {
	if pre.Port == 0 {
		fmt.Fprintln(a.stdout, "The configuration has no mixed-port, so there is no proxy port to use.")
		return
	}
	if len(pre.Addresses) == 0 {
		fmt.Fprintf(a.stdout, "On devices, set the HTTP or SOCKS5 proxy to this server's LAN address, port %d.\n", pre.Port)
		return
	}
	addrs := make([]string, len(pre.Addresses))
	for i, ip := range pre.Addresses {
		addrs[i] = net.JoinHostPort(ip, strconv.Itoa(pre.Port))
	}
	fmt.Fprintf(a.stdout, "On devices, set the HTTP or SOCKS5 proxy to %s\n", strings.Join(addrs, " or "))
	fmt.Fprintf(a.stdout, "(server %s, port %d; Wi-Fi settings → proxy → manual on phones).\n", strings.Join(pre.Addresses, " or "), pre.Port)
}

// describeProxyLAN 用于 status 和菜单的代理端口访问状态。
func (a *app) describeProxyLAN(on bool) string {
	if on {
		return a.style.good("LAN access on")
	}
	return a.style.dim("this server only")
}

func (a *app) newProxyLANSwitchCommand(on bool) *cobra.Command {
	use, short := "off", "Let only this server use the proxy port again"
	if on {
		use, short = "on", "Let devices on the LAN use the proxy port (private addresses only)"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireConfirmation("proxy lan " + use); err != nil {
				return err
			}
			return a.proxyLAN(cmd.Context(), on)
		},
	}
}

// proxyLAN 是 proxy lan on/off 的流程：预览、确认、发布。init 选择局域网代理或网关时也走它。
func (a *app) proxyLAN(ctx context.Context, on bool) error {
	inst := a.installer()
	pre, err := inst.ProxyLANPreflight(ctx)
	if err != nil {
		return err
	}
	plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().PlanProxyLAN(ctx, on) })
	if err != nil {
		return err
	}
	pv := newPlanView(plan, false)
	v := proxyLANView{Plan: &pv, Preflight: pre}
	state := map[bool]string{true: "on", false: "off"}[on]
	if pre.On == on && (!plan.Changed() || plan.Current == nil && !on) {
		return a.render(v, func() error {
			fmt.Fprintf(a.stdout, "LAN access to the proxy port is already %s.\n", state)
			if on {
				a.printProxyAddresses(pre)
			}
			return nil
		})
	}
	if !a.opts.JSON {
		a.printPlan(pv)
	}
	if err := checkExit(plan, !on); err != nil || a.opts.DryRun {
		if a.opts.JSON {
			if rerr := a.render(v, nil); rerr != nil {
				return rerr
			}
		} else if err != nil {
			fmt.Fprintf(a.stdout, "\nLAN access not turned %s.\n", state)
		} else {
			fmt.Fprintln(a.stdout, "\nDry run: nothing changed.")
		}
		return err
	}
	declined := fmt.Sprintf("LAN access not turned %s.", state)
	if on {
		if !a.opts.JSON {
			fmt.Fprintln(a.stdout, "\nDevices with a private address can use the proxy port without a password; mihomo refuses\n"+
				"other addresses (lan-allowed-ips).")
		}
		if len(pre.PublicAddresses) > 0 {
			if !a.opts.JSON {
				fmt.Fprintf(a.stdout, "\n%s %s has the public address %s: this server is probably reachable from the\n"+
					"internet. Only private addresses can use the proxy, but if the server is not on a LAN you use,\n"+
					"leave this off.\n", a.style.warn("Warning:"), pre.Interface, strings.Join(pre.PublicAddresses, ", "))
			}
			if ok, err := a.ask(ctx, "This server has a public address. Open the proxy port to the LAN anyway?", "", declined); !ok {
				return err
			}
		}
	}
	question := "Let devices on the LAN use the proxy port?"
	if !on {
		question = "Stop LAN access to the proxy port?"
	}
	if ok, err := a.ask(ctx, question, "", declined); !ok {
		return err
	}
	err = a.spin("Publishing the configuration", func() (err error) {
		v.Result, err = inst.SetProxyLAN(ctx, on, plan)
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
		fmt.Fprintf(a.stdout, "\n%s\n", a.style.good(fmt.Sprintf("LAN access to the proxy port is %s.", state)))
		if on {
			a.printProxyAddresses(pre)
			fmt.Fprintln(a.stdout, "If a device cannot connect, check other firewalls on this server (they can still block the port).")
		}
		return nil
	})
}
