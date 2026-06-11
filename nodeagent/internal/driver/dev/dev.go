// Package dev implements a process-backed Driver used for development on
// machines without KVM (e.g. a Mac). Each "VM" is a real long-lived child
// process, so liveness checks, crash detection, and re-attach-after-restart
// exercise the same code paths the firecracker driver needs — the dev driver is
// honest about process lifetime rather than faking state in memory.
package dev

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/tavon/proteos/nodeagent/api"
	"github.com/tavon/proteos/nodeagent/internal/driver"
	"github.com/tavon/proteos/nodeagent/internal/state"
)

// FailBootRef is the magic kernel_ref that makes a boot fail, for exercising
// the error path end-to-end.
const FailBootRef = "dev:fail-boot"

// stopGrace is how long the dev driver waits after a graceful signal before it
// hard-kills the stub child. Short, since the stub has nothing to flush.
const stopGrace = 500 * time.Millisecond

// DevDriver runs each machine as a stub child process and persists every state
// change through the shared on-disk Store.
type DevDriver struct {
	store     *state.Store
	bootDelay time.Duration
	stubPath  string
	stubArgs  []string

	// guestAgentBin, when set (PROTEOS_DEV_GUESTAGENT_BIN), makes each "VM" the
	// real guest agent listening on a per-machine unix socket, so the whole
	// browser→gateway→node-agent→guest path runs on a Mac with no hypervisor.
	// Empty ⇒ the plain stub (Phase 2 behaviour).
	guestAgentBin string

	mu    sync.Mutex
	procs map[string]*exec.Cmd // machineID -> running stub child
}

// New builds a DevDriver. stubPath empty ⇒ resolve `sleep` from PATH. The stub
// just needs to be a process that stays alive until signalled. guestAgentBin,
// when non-empty, replaces the stub with the real guest agent (see field doc).
func New(store *state.Store, bootDelay time.Duration, stubPath, guestAgentBin string) *DevDriver {
	args := []string{"2147483647"} // ~68 years; `sleep` accepts a plain number on darwin+linux
	if stubPath == "" {
		if p, err := exec.LookPath("sleep"); err == nil {
			stubPath = p
		}
	}
	return &DevDriver{
		store:         store,
		bootDelay:     bootDelay,
		stubPath:      stubPath,
		stubArgs:      args,
		guestAgentBin: guestAgentBin,
		procs:         make(map[string]*exec.Cmd),
	}
}

// guestSockPath is the per-machine unix socket the guest agent listens on (and
// DialGuest connects to).
func (d *DevDriver) guestSockPath(machineID string) string {
	return filepath.Join(d.store.MachineDir(machineID), "guest.sock")
}

// EnsureRunning is idempotent: a machine already booting/running is a no-op that
// returns its handle; a stopped/error/dead machine is (re)booted, reusing its
// previously allocated network resources.
func (d *DevDriver) EnsureRunning(ctx context.Context, spec driver.VMSpec) (string, error) {
	handle := state.Handle(spec.MachineID)

	rec, existed, err := d.store.Reserve(spec.MachineID, func(a state.Alloc) state.Record {
		return state.Record{
			MachineID: spec.MachineID,
			Handle:    handle,
			State:     agentapi.StateCreating,
			Vcpus:     spec.Vcpus,
			MemMiB:    spec.MemMiB,
			KernelRef: spec.KernelRef,
			RootfsRef: spec.RootfsRef,
			TapName:   a.TapName,
			GuestIP:   a.GuestIP.String(),
			GatewayIP: a.GatewayIP.String(),
			MAC:       a.MAC,
		}
	})
	if err != nil {
		return "", err
	}

	if existed {
		// Already booting/running with a live child ⇒ nothing to do.
		if (rec.State == agentapi.StateCreating || rec.State == agentapi.StateRunning) && d.alive(spec.MachineID, rec.Pid) {
			return handle, nil
		}
		// Otherwise a (re)boot: refresh the desired spec and reset to creating.
		rec, _, err = d.store.Update(spec.MachineID, func(r *state.Record) {
			r.State = agentapi.StateCreating
			r.Reason = ""
			r.Vcpus = spec.Vcpus
			r.MemMiB = spec.MemMiB
			r.KernelRef = spec.KernelRef
			r.RootfsRef = spec.RootfsRef
		})
		if err != nil {
			return "", err
		}
	}

	if err := d.boot(spec.MachineID, spec.KernelRef); err != nil {
		_, _, _ = d.store.Update(spec.MachineID, func(r *state.Record) {
			r.State = agentapi.StateError
			r.Reason = "failed to start stub: " + err.Error()
		})
		return "", err
	}
	return handle, nil
}

// boot launches the stub child, records its pid, and schedules the asynchronous
// creating→running (or →error for the fail-boot ref) transition.
func (d *DevDriver) boot(machineID, kernelRef string) error {
	cmd, err := d.buildCmd(machineID)
	if err != nil {
		return err
	}
	// New process group so a restarted agent can still find/signal it and it
	// is not accidentally killed by a signal sent to the agent's group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}

	d.mu.Lock()
	d.procs[machineID] = cmd
	d.mu.Unlock()

	pid := cmd.Process.Pid
	if _, _, err := d.store.Update(machineID, func(r *state.Record) { r.Pid = pid }); err != nil {
		return err
	}

	go d.finishBoot(machineID, kernelRef, cmd)
	return nil
}

// buildCmd constructs the child process for a machine. With guestAgentBin set
// the child is the real guest agent listening on the machine's guest.sock (its
// 0700 dir is created and any stale socket cleared first); otherwise it is the
// plain keep-alive stub.
func (d *DevDriver) buildCmd(machineID string) (*exec.Cmd, error) {
	if d.guestAgentBin == "" {
		return exec.Command(d.stubPath, d.stubArgs...), nil
	}

	dir := d.store.MachineDir(machineID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir machine dir: %w", err)
	}
	sock := d.guestSockPath(machineID)
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale guest.sock: %w", err)
	}

	cmd := exec.Command(d.guestAgentBin)
	cmd.Env = append(os.Environ(),
		"PROTEOS_GUEST_LISTEN=unix:"+sock,
		"PROTEOS_GUEST_SHELL=/bin/bash",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd, nil
}

// finishBoot waits out the simulated boot delay, then flips creating→running —
// or, for the fail-boot ref, kills the child and records the error. It only
// touches state still in "creating", so a Stop/Destroy racing the delay wins.
func (d *DevDriver) finishBoot(machineID, kernelRef string, cmd *exec.Cmd) {
	timer := time.NewTimer(d.bootDelay)
	defer timer.Stop()
	<-timer.C

	if kernelRef == FailBootRef {
		d.kill(machineID, cmd)
		_, _, _ = d.store.Update(machineID, func(r *state.Record) {
			if r.State == agentapi.StateCreating {
				r.State = agentapi.StateError
				r.Reason = "boot failed (dev:fail-boot)"
				r.Pid = 0
			}
		})
		slog.Info("dev: simulated boot failure", "machine", machineID)
		return
	}

	_, _, _ = d.store.Update(machineID, func(r *state.Record) {
		if r.State == agentapi.StateCreating {
			r.State = agentapi.StateRunning
		}
	})
}

// Stop requests a graceful shutdown: state→stopping immediately, then the child
// is signalled and (after a grace period) killed asynchronously.
func (d *DevDriver) Stop(ctx context.Context, machineID string) error {
	rec, ok, err := d.store.Load(machineID)
	if err != nil {
		return err
	}
	if !ok {
		return driver.ErrUnknownMachine
	}
	if rec.State == agentapi.StateStopped {
		return nil // already there; idempotent
	}

	if _, _, err := d.store.Update(machineID, func(r *state.Record) { r.State = agentapi.StateStopping }); err != nil {
		return err
	}

	d.mu.Lock()
	cmd := d.procs[machineID]
	d.mu.Unlock()

	go d.finishStop(machineID, cmd)
	return nil
}

// finishStop terminates the stub child (graceful then hard) and records stopped.
func (d *DevDriver) finishStop(machineID string, cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(stopGrace):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	d.mu.Lock()
	delete(d.procs, machineID)
	d.mu.Unlock()

	_, _, _ = d.store.Update(machineID, func(r *state.Record) {
		r.State = agentapi.StateStopped
		r.Pid = 0
	})
}

// Status returns the driver-level status. Unknown ⇒ ErrUnknownMachine.
func (d *DevDriver) Status(ctx context.Context, machineID string) (driver.Status, error) {
	rec, ok, err := d.store.Load(machineID)
	if err != nil {
		return driver.Status{}, err
	}
	if !ok {
		return driver.Status{}, driver.ErrUnknownMachine
	}
	return statusOf(rec), nil
}

// Destroy stops the child (if any) and removes all on-disk state. Idempotent.
func (d *DevDriver) Destroy(ctx context.Context, machineID string) error {
	d.mu.Lock()
	cmd := d.procs[machineID]
	delete(d.procs, machineID)
	d.mu.Unlock()
	d.kill(machineID, cmd)
	// Remove the per-machine runtime dir (guest.sock, etc.). Best-effort.
	_ = os.RemoveAll(d.store.MachineDir(machineID))
	return d.store.Delete(machineID)
}

// DialGuest connects to the machine's guest agent over its guest.sock. The
// machine must be tracked (else ErrUnknownMachine); the HTTP layer is
// responsible for the running-state check before calling this.
func (d *DevDriver) DialGuest(ctx context.Context, machineID string) (net.Conn, error) {
	if _, ok, err := d.store.Load(machineID); err != nil {
		return nil, err
	} else if !ok {
		return nil, driver.ErrUnknownMachine
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", d.guestSockPath(machineID))
}

var _ driver.GuestDialer = (*DevDriver)(nil)

// List returns the status of every tracked machine.
func (d *DevDriver) List(ctx context.Context) ([]driver.Status, error) {
	recs, err := d.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]driver.Status, 0, len(recs))
	for _, r := range recs {
		out = append(out, statusOf(r))
	}
	return out, nil
}

// Reattach reconciles persisted records with reality after an agent restart:
// live children are re-adopted (and a stale "creating" is treated as booted);
// dead children become error (crash) or stopped (mid-shutdown).
func (d *DevDriver) Reattach(ctx context.Context) error {
	recs, err := d.store.List()
	if err != nil {
		return err
	}
	for _, rec := range recs {
		alive := rec.Pid > 0 && processAlive(rec.Pid)
		switch rec.State {
		case agentapi.StateCreating, agentapi.StateRunning:
			if alive {
				d.adopt(rec.MachineID, rec.Pid)
				if rec.State == agentapi.StateCreating {
					_, _, _ = d.store.Update(rec.MachineID, func(r *state.Record) { r.State = agentapi.StateRunning })
				}
			} else {
				_, _, _ = d.store.Update(rec.MachineID, func(r *state.Record) {
					r.State = agentapi.StateError
					r.Reason = "vm process exited while agent was down"
					r.Pid = 0
				})
			}
		case agentapi.StateStopping:
			if alive {
				d.adopt(rec.MachineID, rec.Pid)
				go d.finishStop(rec.MachineID, d.procs[rec.MachineID])
			} else {
				_, _, _ = d.store.Update(rec.MachineID, func(r *state.Record) {
					r.State = agentapi.StateStopped
					r.Pid = 0
				})
			}
		}
	}
	return nil
}

// --- helpers ----------------------------------------------------------------

// adopt re-wraps a surviving child pid in an exec.Cmd so Stop/Destroy can
// signal it after a restart. We can't recover the original Cmd, but FindProcess
// + the pid is enough to signal on unix.
func (d *DevDriver) adopt(machineID string, pid int) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	d.mu.Lock()
	d.procs[machineID] = &exec.Cmd{Process: proc}
	d.mu.Unlock()
}

// alive reports whether the machine's child is running, by the in-memory cmd if
// present, else by probing the persisted pid.
func (d *DevDriver) alive(machineID string, pid int) bool {
	d.mu.Lock()
	cmd := d.procs[machineID]
	d.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		return processAlive(cmd.Process.Pid)
	}
	return pid > 0 && processAlive(pid)
}

// kill best-effort terminates the child and reaps it.
func (d *DevDriver) kill(machineID string, cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

func statusOf(rec state.Record) driver.Status {
	return driver.Status{
		MachineID: rec.MachineID,
		State:     rec.State,
		Reason:    rec.Reason,
		Handle:    rec.Handle,
		GuestIP:   rec.GuestIP,
	}
}

// processAlive reports whether a pid refers to a live process. signal 0 probes
// existence without delivering a signal; ESRCH means gone, EPERM means it
// exists but is owned by someone else (still "alive" for our purposes).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}
