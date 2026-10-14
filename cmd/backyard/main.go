// Command backyard is the single Backyard binary: the server (serve, gateway,
// dev) and the CLI/MCP server developers and agents use to build and deploy
// sites. See `backyard --help`.
package main

import (
	"os"

	"github.com/thasami/backyard/internal/cli"
)

func main() { os.Exit(cli.Main()) }
