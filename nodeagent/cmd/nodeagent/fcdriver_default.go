//go:build !firecracker

package main

import (
	"errors"

	"github.com/tavon-ai/proteos/nodeagent/internal/config"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
	"github.com/tavon-ai/proteos/nodeagent/internal/state"
)

// newFirecrackerDriver is unavailable in default builds: the firecracker driver
// is linux-only and carries netlink/Firecracker dependencies, so it is compiled
// in only with `-tags=firecracker`. Asking for it otherwise is a clear startup
// error rather than a silent fallback to the dev driver.
func newFirecrackerDriver(_ *config.Config, _ *state.Store) (driver.Driver, error) {
	return nil, errors.New("firecracker driver not built in (rebuild on linux with -tags=firecracker)")
}
