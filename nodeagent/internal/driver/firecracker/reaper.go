//go:build firecracker && linux

package firecracker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	api "github.com/tavon-ai/proteos/nodeagent/api"
)

const reaperInterval = 30 * time.Second

// StartOrphanReaper starts a background goroutine that periodically scans for
// leaked Firecracker VMM processes and dangling LUKS mapper devices. It wakes
// every reaperInterval, cross-checks /proc against persisted records, SIGKILLs
// any untracked VMM process, and closes any proteos-* mapper not owned by an
// active machine. The goroutine stops when ctx is cancelled.
func (d *Driver) StartOrphanReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(reaperInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.reapOrphans()
			}
		}
	}()
}

// reapOrphans is the single reap cycle: kill leaked Firecracker processes and
// close dangling mapper devices.
func (d *Driver) reapOrphans() {
	recs, err := d.store.List()
	if err != nil {
		slog.Warn("orphan reaper: list state", "err", err)
		return
	}

	// Build the set of VMM PIDs that are legitimately alive. Include all
	// non-terminal states (Creating, Running, Stopping, Hibernating) because
	// finishStop holds the PID live during teardown.
	knownPIDs := map[int]string{} // pid → machineID
	for _, rec := range recs {
		switch rec.State {
		case api.StateCreating, api.StateRunning, api.StateStopping, api.StateHibernating:
			if rec.Pid > 0 {
				knownPIDs[rec.Pid] = rec.MachineID
			}
		}
	}

	for _, pid := range findFirecrackerPIDs() {
		if _, ok := knownPIDs[pid]; !ok {
			slog.Warn("orphan reaper: killing leaked Firecracker VMM", "pid", pid)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}

	// Build the set of mapper names that must stay open. Stopping and Hibernating
	// machines have finishStop actively closing their mapper, so they stay in the
	// expected set to avoid racing with that goroutine.
	expectedMappers := map[string]struct{}{}
	for _, rec := range recs {
		switch rec.State {
		case api.StateCreating, api.StateRunning, api.StateStopping, api.StateHibernating:
			expectedMappers[mapperName(rec.MachineID)] = struct{}{}
		}
	}

	entries, err := os.ReadDir("/dev/mapper")
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "proteos-") {
			continue
		}
		if _, ok := expectedMappers[name]; ok {
			continue
		}
		slog.Warn("orphan reaper: closing dangling LUKS mapper", "mapper", name)
		if err := d.cryptsetup(nil, "close", name); err != nil {
			slog.Warn("orphan reaper: close mapper failed", "mapper", name, "err", err)
		}
	}
}

// findFirecrackerPIDs returns the PIDs of all processes named "firecracker" by
// scanning /proc/<pid>/comm. The comm file truncates to 15 characters; "firecracker"
// (11 chars) fits in full.
func findFirecrackerPIDs() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil {
			continue // process may have exited between ReadDir and ReadFile
		}
		if strings.TrimSpace(string(comm)) == "firecracker" {
			pids = append(pids, pid)
		}
	}
	return pids
}
