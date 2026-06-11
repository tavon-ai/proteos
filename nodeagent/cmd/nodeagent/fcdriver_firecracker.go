//go:build firecracker && linux

package main

import (
	"github.com/tavon/proteos/nodeagent/internal/config"
	"github.com/tavon/proteos/nodeagent/internal/driver"
	"github.com/tavon/proteos/nodeagent/internal/driver/firecracker"
	"github.com/tavon/proteos/nodeagent/internal/state"
)

// newFirecrackerDriver wires the real linux-only Firecracker driver. Built only
// with `-tags=firecracker`; the default build uses fcdriver_default.go which
// returns an error for this driver.
func newFirecrackerDriver(cfg *config.Config, store *state.Store) (driver.Driver, error) {
	return firecracker.New(firecracker.Config{
		FirecrackerBin: cfg.FirecrackerBin,
		JailerBin:      cfg.JailerBin,
		ChrootBaseDir:  cfg.ChrootBaseDir,
		ImagesDir:      cfg.ImagesDir,
		JailUIDStart:   cfg.JailUIDStart,
		JailUIDCount:   cfg.JailUIDCount,
		GuestVsockPort: cfg.GuestVsockPort,
		VolumesDir:     cfg.VolumesDir,
		CryptsetupBin:  cfg.CryptsetupBin,
	}, store), nil
}
