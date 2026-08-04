// Command proteos is the ProteOS CLI: it drives the headless Agent Task lane
// (and machine/auth basics) over the control-plane HTTP API, authenticating with
// a personal access token.
package main

import (
	"os"
	"runtime/debug"

	"github.com/tavon-ai/proteos/cli/internal/app"
	"github.com/tavon-ai/proteos/cli/internal/client"
)

// Stamped at build time via -ldflags, e.g.
// "-X main.version=v1.2.3 -X main.commit=abc1234 -X main.date=2026-06-29T12:00:00Z".
// The defaults are what an un-stamped `go build`/`go run` reports, and what
// buildIdentity tries to improve on.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// identity is the build identity `proteos version` prints.
type identity struct {
	version string
	commit  string
	date    string
}

// withBuildInfo backfills any field still at its -ldflags default from the Go
// module build info the toolchain embeds in every binary. Stamped values always
// win; this only fills gaps.
//
// It exists for `go install github.com/tavon-ai/proteos/cli/cmd/proteos@v1.2.3`,
// which is the documented install path and cannot pass linker flags — so those
// binaries reported themselves as "dev / none / unknown" regardless of the tag
// they were built from. Module installs do record the resolved version, which is
// the field that actually matters for bug reports.
//
// The two sources are mutually exclusive in practice: the toolchain stamps
// vcs.* only when building from a local checkout (where Main.Version is
// "(devel)"), and records a real Main.Version only for module downloads (where
// there is no checkout to read a revision from). So a `go install`ed binary
// still has no commit or date, and a `go build` from a clone gets both of those
// but no version.
func (id identity) withBuildInfo(bi *debug.BuildInfo, ok bool) identity {
	if !ok {
		return id
	}
	// "(devel)" is what a build from a source tree reports — no more useful
	// than the "dev" default it would replace.
	if id.version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		id.version = bi.Main.Version
	}

	var revision, modified, buildTime string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		case "vcs.time":
			buildTime = s.Value
		}
	}
	if id.commit == "none" && revision != "" {
		// Match the short sha the release workflow stamps, so the two build
		// paths print commits of the same shape.
		if len(revision) > 7 {
			revision = revision[:7]
		}
		if modified == "true" {
			revision += "-dirty"
		}
		id.commit = revision
	}
	if id.date == "unknown" && buildTime != "" {
		id.date = buildTime
	}
	return id
}

func main() {
	id := identity{version: version, commit: commit, date: date}.
		withBuildInfo(debug.ReadBuildInfo())

	client.SetUserAgent("proteos-cli/" + id.version)
	os.Exit(app.Run(app.Env{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: id.version,
		Commit:  id.commit,
		Date:    id.date,
	}, os.Args[1:]))
}
