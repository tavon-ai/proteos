//go:build firecracker && linux

package main

import (
	"fmt"
	"net"

	"github.com/tavon-ai/proteos/nodeagent/internal/config"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver/firecracker"
	"github.com/tavon-ai/proteos/nodeagent/internal/state"
)

// newFirecrackerDriver wires the real linux-only Firecracker driver. Built only
// with `-tags=firecracker`; the default build uses fcdriver_default.go which
// returns an error for this driver.
func newFirecrackerDriver(cfg *config.Config, store *state.Store) (driver.Driver, error) {
	_, agentPort, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("parsing PROTEOS_AGENT_ADDR %q: %w", cfg.Addr, err)
	}
	return firecracker.New(firecracker.Config{
		FirecrackerBin:  cfg.FirecrackerBin,
		JailerBin:       cfg.JailerBin,
		ChrootBaseDir:   cfg.ChrootBaseDir,
		ImagesDir:       cfg.ImagesDir,
		JailUIDStart:    cfg.JailUIDStart,
		JailUIDCount:    cfg.JailUIDCount,
		GuestVsockPort:  cfg.GuestVsockPort,
		VolumesDir:      cfg.VolumesDir,
		CryptsetupBin:   cfg.CryptsetupBin,
		AgentPort:       agentPort,
		MgmtIfaces:      cfg.MgmtIfaces,
		CapacityVcpus:   cfg.CapacityVcpus,
		CapacityMemMiB:  cfg.CapacityMemMiB,
		CapacityDiskMiB: cfg.CapacityDiskMiB,
	}, store), nil
}
