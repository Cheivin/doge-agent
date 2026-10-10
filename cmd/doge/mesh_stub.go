//go:build !mesh

package main

import (
	"fmt"
	"os"
)

// runMeshCommand handles `doge mesh ...` in the non-mesh build. Token
// generation works (pure config, no libp2p); everything else needs the mesh
// build.
func runMeshCommand(args []string) int {
	if len(args) == 0 {
		meshUsage()
		return 2
	}
	switch args[0] {
	case "new":
		return meshNewCmd(args[1:])
	case "join":
		return meshJoinCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "doge: mesh subcommand %q requires a build with -tags mesh\n", args[0])
		return 1
	}
}
