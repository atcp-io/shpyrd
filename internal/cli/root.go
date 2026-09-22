// Package cli implements the shpyrd command tree.
package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

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
		Short:         "Make infrastructure easy again",
		Long:          "shpyrd manages the whole application stack on Kubernetes: cluster bootstrap, builds, deploys and monitoring.",
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
	root.AddCommand(newScaleCmd(g))
	root.AddCommand(newLogsCmd(g))
	root.AddCommand(newReleasesCmd(g))
	root.AddCommand(newRollbackCmd(g))
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
