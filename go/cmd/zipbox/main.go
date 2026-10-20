// Command zipbox drives Zipbox boxes from a terminal or CI: create and manage
// boxes, run commands, move files, open a shell, publish ports, read docs, and
// serve the Zipbox MCP tools over stdio.
//
// Install: https://zipbox.ai/docs/cli
package main

import (
	"os"

	"github.com/tribes-protocol/zipbox-go/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
