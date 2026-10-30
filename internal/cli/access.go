package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/setup"
	"github.com/tunlr-app/cli/pkg/core/state"
)

// lanChoice 是命令行给出的放行入口（--interface、--containers），没有给出的为 nil，见 setup.ResolveLANAccess。
// also 是在选过的或默认的网卡之外另外放行的网卡（init 询问用途时确认过的、有公网地址的网卡），interfaces
// 为 nil 时才用。
type lanChoice struct {
	interfaces []string
	containers *bool
	also       []string
}

// lanAccessFlags 是 proxy lan on、panel lan on 的 --interface、--containers。
type lanAccessFlags struct {
	cmd        *cobra.Command
	interfaces []string
	containers bool
}

func addLANAccessFlags(cmd *cobra.Command) *lanAccessFlags {
	f := &lanAccessFlags{cmd: cmd}
	cmd.Flags().StringSliceVar(&f.interfaces, "interface", nil,
		"let in connections through these interfaces only, comma-separated; shared by the proxy port and\n"+
			"the panel (default: as chosen before, else the interfaces without a public address)")
	cmd.Flags().BoolVar(&f.containers, "containers", false, "also let in containers on this server (docker0, br-*; other bridges such as podman0 go in\n"+
		"--interface); --containers=false stops")
	return f
}

func (f *lanAccessFlags) choice() lanChoice {
	var c lanChoice
	if f.cmd.Flags().Changed("interface") {
		c.interfaces = f.interfaces
		if c.interfaces == nil {
			c.interfaces = []string{}
		}
	}
	if f.cmd.Flags().Changed("containers") {
		c.containers = &f.containers
	}
	return c
}

// chooseLANAccess 得出开启代理端口或面板的局域网访问时放行的入口，cands、cur 取自前置检查：参数给出的
// 优先；可以提问时让用户勾选网卡和容器，预先勾上现在放行的（没有选过时是默认值）；否则沿用选过的或默认值。
// 用户取消时返回 nil。
func (a *app) chooseLANAccess(ctx context.Context, cands []setup.LANInterface, cur setup.LANAccess, c lanChoice) (*state.LANAccess, error) {
	inst := a.installer()
	if c.interfaces == nil && c.also != nil {
		c.interfaces = append(slices.Clone(cur.Interfaces), c.also...)
	}
	if c.interfaces != nil || c.containers != nil || a.opts.Yes || a.opts.DryRun || !a.interactive() {
		return inst.ResolveLANAccess(cands, c.interfaces, c.containers)
	}
	// 网关的局域网网卡总是放行，不列；选过它时照旧保留。
	var rows [][]string
	var names []string
	for _, cand := range cands {
		if cand.Name != cur.Gateway {
			rows = append(rows, []string{cand.Name, strings.Join(cand.Addresses, ", "), interfaceNotes(cand)})
			names = append(names, cand.Name)
		}
	}
	for _, name := range cur.Interfaces {
		if name != cur.Gateway && !slices.Contains(names, name) {
			rows = append(rows, []string{name, "-", "down or gone"})
			names = append(names, name)
		}
	}
	var options []string
	if len(rows) > 0 {
		options = columns(rows)
	}
	options = append(options, "Containers on this server (docker0, br-*)")
	var selected []int
	for k, name := range names {
		if slices.Contains(cur.Interfaces, name) {
			selected = append(selected, k)
		}
	}
	if cur.Containers {
		selected = append(selected, len(names))
	}
	title := "Let in connections through which interfaces? (private addresses only)"
	if cur.Gateway != "" {
		title = fmt.Sprintf("Let in connections through which interfaces besides %s, the gateway's LAN? (private addresses only)", cur.Gateway)
	}
	for {
		picked, err := a.prompt.ChooseMany(ctx, title, options, selected)
		if cancelled, err := a.cancelled(err); cancelled {
			return nil, err
		}
		res := state.LANAccess{Interfaces: []string{}}
		if slices.Contains(cur.Interfaces, cur.Gateway) {
			res.Interfaces = append(res.Interfaces, cur.Gateway)
		}
		for _, k := range picked {
			if k < len(names) {
				res.Interfaces = append(res.Interfaces, names[k])
			} else {
				res.Containers = true
			}
		}
		if len(res.Interfaces) == 0 && !res.Containers && cur.Gateway == "" {
			fmt.Fprintln(a.stdout, "Choose at least one (space toggles); to let only this server in, turn LAN access off.")
			continue
		}
		return inst.ResolveLANAccess(cands, res.Interfaces, &res.Containers)
	}
}

// interfaceNotes 是选择列表中网卡的说明：默认路由所在、当前 SSH 会话进来的、VPN、有公网地址。
func interfaceNotes(c setup.LANInterface) string {
	var notes []string
	if c.Default {
		notes = append(notes, "default route")
	}
	if c.SSH {
		notes = append(notes, "this SSH session")
	}
	if c.VPN {
		notes = append(notes, "VPN")
	}
	if len(c.PublicAddresses) > 0 {
		notes = append(notes, "public address")
	}
	return strings.Join(notes, ", ")
}

// newlyPublic 返回 access 放行、有公网地址的网卡中，开关关着（on 为 false）或之前没有放行（cur）的那些：
// 放行它们之前要再确认一次。
func newlyPublic(cands []setup.LANInterface, cur setup.LANAccess, on bool, access *state.LANAccess) []setup.LANInterface {
	var res []setup.LANInterface
	for _, c := range cands {
		if len(c.PublicAddresses) > 0 && slices.Contains(access.Interfaces, c.Name) && (!on || !slices.Contains(cur.Interfaces, c.Name)) {
			res = append(res, c)
		}
	}
	return res
}

// confirmPublic 在放行有公网地址的网卡（list）之前再确认一次，what 是开放的东西，如 "the proxy port"。
// 同意或 list 为空时返回 true。
func (a *app) confirmPublic(ctx context.Context, list []setup.LANInterface, what, declined string) (bool, error) {
	public := publicNote(list)
	if public == "" {
		return true, nil
	}
	if !a.opts.JSON {
		fmt.Fprintf(a.stdout, "\n%s %s: this server is probably reachable from the internet\n"+
			"through it. Only private addresses are let in, but if it is not on a LAN you use, leave it out.\n",
			a.style.warn("Warning:"), public)
	}
	return a.ask(ctx, "Let in connections to "+what+" through an interface with a public address anyway?", "", declined)
}

// updateLANAccess 在开关已经开启时保存新选的入口 access（cands、cur 取自前置检查）并重新加载表；入口没有
// 变化时也重新加载，补上不见了的表。返回入口是否有变化。用户拒绝放行有公网地址的网卡时什么都不做，
// a.aborted 为 true。
func (a *app) updateLANAccess(ctx context.Context, cands []setup.LANInterface, cur setup.LANAccess, access *state.LANAccess, what, declined string) (bool, error) {
	changed := !cur.Same(access)
	if changed {
		if ok, err := a.confirmPublic(ctx, newlyPublic(cands, cur, true, access), what, declined); !ok {
			return false, err
		}
	}
	if a.opts.DryRun {
		return changed, nil
	}
	return changed, a.installer().SetLANAccess(ctx, access)
}

// printAccessUpdate 输出 updateLANAccess 之后放行的入口；cur 是之后的前置检查结果（--dry-run 时是之前的）。
func (a *app) printAccessUpdate(changed bool, cur setup.LANAccess, access *state.LANAccess, command string) {
	switch {
	case !changed:
		a.printAccess(cur, command)
	case a.opts.DryRun:
		fmt.Fprintf(a.stdout, "Dry run: would let in through %s instead of %s.\n", a.describeAccess(cur.With(access)), a.describeAccess(cur))
	default:
		fmt.Fprintln(a.stdout, a.style.good("Interfaces updated."))
		a.printAccess(cur, command)
	}
}

// describeAccess 是放行的入口，如 “ens3, wg0, ens4 (gateway LAN), containers on this server”。
func (a *app) describeAccess(acc setup.LANAccess) string {
	parts := slices.Clone(acc.Interfaces)
	if acc.Gateway != "" && !slices.Contains(parts, acc.Gateway) {
		parts = append(parts, acc.Gateway+" (gateway LAN)")
	}
	if acc.Containers {
		parts = append(parts, "containers on this server")
	}
	if len(parts) == 0 {
		return a.style.warn("nothing (only this server)")
	}
	return strings.Join(parts, ", ")
}

// printAccess 输出放行的入口和修改的方法，command 是 "proxy" 或 "panel"。
func (a *app) printAccess(acc setup.LANAccess, command string) {
	fmt.Fprintf(a.stdout, "Let in through: %s %s\n", a.describeAccess(acc),
		a.style.dim("(shared by the proxy port and the panel; change: tunlr "+command+" lan on --interface ...)"))
}
