// Command shpyrd is the shpyrd command line interface.
package main

import (
	"fmt"
	"os"

	"shpyrd/internal/cli"
	"shpyrd/pkg/kexec"
)

func main() {
	if err := cli.New().Execute(); err != nil {
		if code := kexec.ExitCode(err); code != 1 {
			os.Exit(code) // remote command's exit code (shpyrd run)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
