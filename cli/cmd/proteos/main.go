// Command proteos is the ProteOS CLI: it drives the headless Agent Task lane
// (and machine/auth basics) over the control-plane HTTP API, authenticating with
// a personal access token.
package main

import (
	"os"

	"github.com/tavon-ai/proteos/cli/internal/app"
	"github.com/tavon-ai/proteos/cli/internal/client"
)

// Stamped at build time via -ldflags, e.g.
// "-X main.version=v1.2.3 -X main.commit=abc1234 -X main.date=2026-06-29T12:00:00Z".
// The defaults are what an un-stamped `go build`/`go run` reports.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	client.SetUserAgent("proteos-cli/" + version)
	os.Exit(app.Run(app.Env{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: version,
		Commit:  commit,
		Date:    date,
	}, os.Args[1:]))
}
