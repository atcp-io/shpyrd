// Command shpyrd is the shpyrd command line interface.
package main

import (
	"fmt"
	"os"

	"shpyrd/internal/cli"
)

func main() {
	if err := cli.New().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
