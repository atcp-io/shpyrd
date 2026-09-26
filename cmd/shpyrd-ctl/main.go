// shpyrd-ctl is the operator CLI (RFC-0052): the cluster lifecycle commands
// that need a kubeconfig (cluster create|init|status|destroy|backup|restore,
// extensions, registry, sizes, the admin token, the break-glass tools).
// Developers who build and deploy use `shpyrd` instead.
package main

import (
	"fmt"
	"os"

	"shpyrd/internal/cli"
	"shpyrd/pkg/version"
)

func main() {
	cli.Version = version.Version
	cmd := cli.NewCtl()
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
