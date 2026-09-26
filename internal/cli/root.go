// Package cli implements the shpyrd command tree.
package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"shpyrd/pkg/ext/all"
	"shpyrd/pkg/version"
)

// Version of the CLI (see pkg/version).
var Version = version.Version

type globalFlags struct {
	kubeconfig string
	kubeCtx    string
	verbose    bool
}

// New builds the root command.
func New() *cobra.Command {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:           "shpyrd",
		Short:         "Opensource Cloud PaaS",
		Long:          "shpyrd manages applications and agents from one place, from deploy to monitoring: cluster bootstrap, buildpack builds, releases, config vars, logs and metrics on Kubernetes.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			level := slog.LevelInfo
			if g.verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		},
	}
	root.PersistentFlags().StringVar(&g.kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "path to the kubeconfig file")
	root.PersistentFlags().StringVar(&g.kubeCtx, "context", "", "kubeconfig context to use")
	root.PersistentFlags().BoolVarP(&g.verbose, "verbose", "v", false, "verbose output")
	root.CompletionOptions.HiddenDefaultCmd = true

	root.AddCommand(newClusterCmd(g))
	root.AddCommand(newAppsCmd(g))
	root.AddCommand(newDeployCmd(g))
	root.AddCommand(newSecretsCmd(g))
	root.AddCommand(newGlobalsCmd(g))
	root.AddCommand(newDrainsCmd(g))
	root.AddCommand(newScaleCmd(g))
	root.AddCommand(newResizeCmd(g))
	root.AddCommand(newSizesCmd(g))
	root.AddCommand(newVolumesCmd(g))
	root.AddCommand(newAttachCmd(g))
	root.AddCommand(newDetachCmd(g))
	root.AddCommand(newExtensionsCmd(g))
	root.AddCommand(newTeamsCmd(g))
	root.AddCommand(newMembersCmd(g))
	// Commands contributed by extensions (they explain themselves when the
	// extension is not enabled on the cluster).
	for _, x := range all.All() {
		for _, c := range x.CLI(g) {
			// Extensions may share a top-level command (`shpyrd auth`): the
			// later ones add their subcommands to the first.
			if existing := findCommand(root, c.Name()); existing != nil {
				existing.AddCommand(c.Commands()...)
				continue
			}
			root.AddCommand(c)
		}
	}
	root.AddCommand(newLogsCmd(g))
	root.AddCommand(newShellCmd(g))
	root.AddCommand(newRunCmd(g))
	root.AddCommand(newReleasesCmd(g))
	root.AddCommand(newRollbackCmd(g))
	root.AddCommand(newExposureCmd(g))
	root.AddCommand(newAccessCmd(g))
	root.AddCommand(newAllowCmd(g))
	root.AddCommand(newDomainsCmd(g))
	root.AddCommand(newLoginCmd(g))
	root.AddCommand(newLogoutCmd(g))
	root.AddCommand(newWhoAmICmd(g))
	root.AddCommand(newRedeployCmd(g))
	root.AddCommand(newOpenCmd(g))
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), Version)
		},
	})
	return root
}

func findCommand(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}
