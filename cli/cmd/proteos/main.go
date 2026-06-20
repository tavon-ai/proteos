// Command proteos is the ProteOS CLI: it drives the headless Agent Task lane
// (and machine/auth basics) over the control-plane HTTP API, authenticating with
// a personal access token.
package main

import (
	"os"

	"github.com/tavon/proteos/cli/internal/app"
	"github.com/tavon/proteos/cli/internal/client"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	client.SetUserAgent("proteos-cli/" + version)
	os.Exit(app.Run(app.Env{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: version,
	}, os.Args[1:]))
}
