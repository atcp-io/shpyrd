package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"shpyrd/pkg/ext/all"
	"shpyrd/pkg/version"
)

// NewCtl builds the shpyrd-ctl command tree: cluster operations that need a
// kubeconfig (RFC-0052). Developers use `shpyrd` instead.
func NewCtl() *cobra.Command {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:           "shpyrd-ctl",
		Short:         "Operate a shpyrd cluster (needs a kubeconfig)",
		Long:          "shpyrd-ctl manages the cluster lifecycle: bootstrap, upgrade, backup, restore, extensions, the registry and the admin token. Developers who only build and deploy apps use `shpyrd` instead.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&g.kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "path to the kubeconfig file")
	root.PersistentFlags().StringVar(&g.kubeCtx, "context", "", "kubeconfig context to use")
	root.PersistentFlags().BoolVarP(&g.verbose, "verbose", "v", false, "verbose output")
	root.CompletionOptions.HiddenDefaultCmd = true

	root.AddCommand(newClusterCmd(g))
	root.AddCommand(newExtensionsCmd(g))
	root.AddCommand(newSizesCmd(g))
	root.AddCommand(newGlobalsCmd(g))
	// Extension commands (auth, pg, redis, object-storage …) for operators.
	for _, x := range all.All() {
		for _, c := range x.CLI(g) {
			if existing := findCommand(root, c.Name()); existing != nil {
				existing.AddCommand(c.Commands()...)
				continue
			}
			root.AddCommand(c)
		}
	}
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version.Version)
		},
	})
	return root
}
