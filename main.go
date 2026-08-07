// Command gbsnap replicates btrfs snapshots between pools.
package main

import (
	"os"

	"github.com/greendrake/gbsnap/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
