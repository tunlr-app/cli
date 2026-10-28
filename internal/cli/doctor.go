package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/doctor"
	"github.com/tunlr-app/cli/pkg/core/setup"
)

// groupTitles 是 doctor 输出中各组的标题。
var groupTitles = map[string]string{
	doctor.GroupSystem:  "System",
	doctor.GroupTun:     "Transparent proxy (TUN) support",
	doctor.GroupInstall: "Installation",
	doctor.GroupNetwork: "Network",
}

func (a *app) newDoctorCommand() *cobra.Command {
	var offline bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose this server and the installation",
		Long: "Check whether this server can run tunlr (Linux, root, systemd, architecture, not WSL or\n" +
			"a container), whether the transparent proxy can work (TUN, nft, ip, conflicting rules),\n" +
			"whether the installation is healthy (units, service, kernel, configuration, timers, ports,\n" +
			"subscriptions) and whether the network works (DNS, GitHub, mirror, clock, the proxy).\n" +
			"It changes nothing; problems come with a hint on how to fix them.\n\n" +
			"Exit code 3 when any check fails; warnings alone exit 0. --offline skips the network checks.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			var r *doctor.Report
			title := "Running diagnostics"
			if !offline {
				title += " (network checks take a few seconds)"
			}
			err := a.spin(title, func() (err error) {
				r, err = a.installer().Doctor(cmd.Context(), setup.DoctorOptions{Offline: offline})
				return err
			})
			if err != nil {
				return err
			}
			rerr := a.render(r, func() error { a.printReport(r); return nil })
			if err := r.Err(); err != nil {
				return renderedError{err}
			}
			return rerr
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "skip the network checks")
	return cmd
}

// printReport 按组输出诊断结果，warn、fail 的下一行是修复建议，最后一行是汇总。
func (a *app) printReport(r *doctor.Report) {
	w := a.stdout
	width := 0
	for _, c := range r.Checks {
		width = max(width, len(c.Name))
	}
	// 修复建议与说明对齐：有样式时前面是图标和空格，否则是 8 列的状态和空格。
	indent := strings.Repeat(" ", width+4)
	if !a.style.on {
		indent = strings.Repeat(" ", width+10)
	}
	group := ""
	for _, c := range r.Checks {
		if c.Group != group {
			if group != "" {
				fmt.Fprintln(w)
			}
			group = c.Group
			fmt.Fprintln(w, a.style.title(groupTitles[group]))
		}
		a.printCheck(c.Status, c.Name, width, c.Message)
		if c.Hint != "" {
			fmt.Fprintf(w, "%s%s\n", indent, a.style.dim("→ "+c.Hint))
		}
	}
	fmt.Fprintln(w)
	fails, warns := r.Count(doctor.Fail), r.Count(doctor.Warn)
	switch {
	case fails > 0:
		fmt.Fprintln(w, a.style.bad(fmt.Sprintf("%s, %s.", plural(fails, "check failed", "checks failed"), plural(warns, "warning", "warnings"))))
	case warns > 0:
		fmt.Fprintln(w, a.style.warn(fmt.Sprintf("No failures, %s.", plural(warns, "warning", "warnings"))))
	default:
		fmt.Fprintln(w, a.style.good("All checks passed."))
	}
}

// plural 返回“n 个东西”，n 为 1 时用单数。
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// printProblems 在 status 的最后提示快速诊断发现的失败项，没有时什么都不输出。
func (a *app) printProblems(r *doctor.Report) {
	if r == nil {
		return
	}
	if n := r.Count(doctor.Fail); n > 0 {
		fmt.Fprintf(a.stdout, "\n%s; run tunlr doctor for details.\n",
			a.style.bad(plural(n, "problem found", "problems found")))
	}
}
