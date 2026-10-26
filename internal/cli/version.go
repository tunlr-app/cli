package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

type versionData struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func (a *app) newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the tunlr version",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			d := versionData{
				Version: a.info.Version,
				Commit:  a.info.Commit,
				Date:    a.info.Date,
				Go:      runtime.Version(),
				OS:      runtime.GOOS,
				Arch:    runtime.GOARCH,
			}
			return a.render(d, func() error {
				_, err := fmt.Fprintf(a.stdout, "tunlr %s (commit %s, built %s, %s %s/%s)\n",
					d.Version, d.Commit, d.Date, d.Go, d.OS, d.Arch)
				return err
			})
		},
	}
}
