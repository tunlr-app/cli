package cli

import (
	"cmp"
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/setup"
)

func (a *app) newPanelCommand() *cobra.Command {
	var reset bool
	cmd := &cobra.Command{
		Use:   "panel",
		Short: "Show how to open the panel (SSH tunnel and link); --reset-secret replaces the API secret",
		Long: "The panel (zashboard) talks to mihomo's API, which listens on 127.0.0.1 only. Show the SSH\n" +
			"command that forwards it to your computer, and the link that opens the panel already\n" +
			"connected (it carries the API secret). Run the SSH command on your computer, not here.\n\n" +
			"--reset-secret publishes the configuration with a new random API secret; mihomo restarts,\n" +
			"and panels opened with the old link have to connect again with the new one.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if reset {
				return a.resetSecret(cmd)
			}
			if a.opts.DryRun {
				return usageErrorf("panel changes nothing; --dry-run only applies to --reset-secret")
			}
			p, err := a.panel(cmd.Context())
			if err != nil {
				return err
			}
			return a.render(p, func() error { a.printPanel(p); return nil })
		},
	}
	cmd.Flags().BoolVar(&reset, "reset-secret", false, "publish the configuration with a new API secret (mihomo restarts)")
	return cmd
}

func (a *app) panel(ctx context.Context) (*setup.Panel, error) {
	return a.installer().Panel(ctx, setup.PanelOptions{
		SSHConnection: os.Getenv("SSH_CONNECTION"),
		User:          cmp.Or(os.Getenv("SUDO_USER"), os.Getenv("USER")),
	})
}

func (a *app) printPanel(p *setup.Panel) {
	w := a.stdout
	if p.Running == "" {
		fmt.Fprintf(w, "%s mihomo is not running; start it with: tunlr start\n\n", a.style.warn("Note:"))
	}
	if p.UIDir != "" && !p.UIInstalled {
		fmt.Fprintf(w, "%s the panel is not installed in %s yet; tunlr apply installs it.\n\n", a.style.warn("Note:"), p.UIDir)
	}
	if p.Public {
		fmt.Fprintf(w, "%s the API listens on %s, reachable from the network (a mixin set it);\n"+
			"anyone who can reach it and knows the secret controls mihomo.\n\n", a.style.warn("Warning:"), p.Controller)
		fmt.Fprintln(w, "Open in your browser:")
		fmt.Fprintf(w, "  %s\n", p.URL)
		return
	}

	fmt.Fprintf(w, "The API listens on %s, only reachable from this server.\n\n", p.Controller)
	fmt.Fprintln(w, "1. On your computer, keep this SSH tunnel open:")
	fmt.Fprintf(w, "     %s\n", p.SSHCommand)
	if p.SSHHostGuessed {
		fmt.Fprintln(w, "   (the address is the one this SSH session came in on; use the host name or alias you")
		fmt.Fprintln(w, "   normally log in with if that differs)")
	} else {
		fmt.Fprintln(w, "   (replace <server> with how you log in to this server)")
	}
	fmt.Fprintln(w, "   Or add it to the host's entry in ~/.ssh/config, so every login opens the tunnel:")
	fmt.Fprintf(w, "     %s\n\n", p.LocalForward)
	fmt.Fprintln(w, "2. Then open in your browser:")
	fmt.Fprintf(w, "     %s\n", p.URL)
	if p.Secret != "" {
		fmt.Fprintln(w, "\nThe link carries the API secret; keep it private (tunlr panel --reset-secret replaces it).")
	}
}

// resetSecret 经流水线换一个新的 API secret 并发布，成功后输出新的面板链接。
func (a *app) resetSecret(cmd *cobra.Command) error {
	if err := a.requireConfirmation("panel --reset-secret"); err != nil {
		return err
	}
	plan, err := a.plan(func() (*pipeline.Plan, error) { return a.pipeline().PlanResetSecret(cmd.Context()) })
	if err != nil {
		return err
	}
	if err := a.publish(cmd, plan, "Replace the API secret?"); err != nil {
		return err
	}
	// --json 只输出发布结果，避免两份 JSON；新的链接用 tunlr panel --json 取。
	if a.opts.JSON || a.opts.DryRun || a.aborted {
		return nil
	}
	p, err := a.panel(cmd.Context())
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "\nPanels opened with the old link have to connect again; the new link:")
	fmt.Fprintf(a.stdout, "  %s\n", p.URL)
	return nil
}
