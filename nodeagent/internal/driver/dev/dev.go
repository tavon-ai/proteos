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

	// guestWebBackend, when set (PROTEOS_DEV_GUEST_WEB_BACKEND), enables the guest
	// agent's Phase 8 web forward (code-server stand-in): the guest agent listens
	// on guest-web.sock and raw-forwards to this address. In dev/e2e this points
	// at a stub HTTP+WS server, so DialGuest(GuestWebPort) round-trips without a
	// real code-server. Empty ⇒ no web listener (terminal-only dev).
	guestWebBackend string

	mu    sync.Mutex
	procs map[string]*exec.Cmd // machineID -> running stub child
}

// New builds a DevDriver. stubPath empty ⇒ resolve `sleep` from PATH. The stub
// just needs to be a process that stays alive until signalled. guestAgentBin,
// when non-empty, replaces the stub with the real guest agent (see field doc).
// guestWebBackend, when non-empty, enables the guest agent's web forward toward
// that address (Phase 8 dev/e2e stand-in for code-server).
func New(store *state.Store, bootDelay time.Duration, stubPath, guestAgentBin, guestWebBackend string) *DevDriver {
	args := []string{"2147483647"} // ~68 years; `sleep` accepts a plain number on darwin+linux
	if stubPath == "" {
		if p, err := exec.LookPath("sleep"); err == nil {
			stubPath = p
		}
	}
	return &DevDriver{
		store:           store,
		bootDelay:       bootDelay,
		stubPath:        stubPath,
		stubArgs:        args,
		guestAgentBin:   guestAgentBin,
		guestWebBackend: guestWebBackend,
		procs:           make(map[string]*exec.Cmd),
	}
}

// guestSockPath is the per-machine unix socket the guest agent's terminal
// listener uses (and DialGuest connects to for the terminal port).
func (d *DevDriver) guestSockPath(machineID string) string {
	return filepath.Join(d.store.MachineDir(machineID), "guest.sock")
}

// guestWebSockPath is the per-machine unix socket the guest agent's Phase 8 web
// forward (code-server) listens on, standing in for vsock port 1025 in dev.
func (d *DevDriver) guestWebSockPath(machineID string) string {
	return filepath.Join(d.store.MachineDir(machineID), "guest-web.sock")
}

// guestPreviewSockPath is the per-machine unix socket the guest agent's port-
// preview forwarder (PP1) listens on, standing in for vsock port 1026 in dev.
func (d *DevDriver) guestPreviewSockPath(machineID string) string {
	return filepath.Join(d.store.MachineDir(machineID), "guest-preview.sock")
}

// persistDir is the per-machine directory standing in for the real driver's
// persistent disk (decision #10). It is handed to the guest agent via
// PROTEOS_GUEST_PERSIST and survives hibernate/resume (kept across Stop, only
// removed by Destroy).
func (d *DevDriver) persistDir(machineID string) string {
	return filepath.Join(d.store.MachineDir(machineID), "persist")
}

// envDir is the per-machine directory standing in for the guest's tmpfs secret
// env dir (/run/proteos/env in a real VM). Handed to the guest via
// PROTEOS_GUEST_ENV_DIR; the guest prunes it on each boot so it behaves like
// tmpfs even though it lives under the machine dir in dev.
func (d *DevDriver) envDir(machineID string) string {
	return filepath.Join(d.store.MachineDir(machineID), "env")
}

// EnsureRunning is idempotent: a machine already booting/running is a no-op that
// returns its handle; a stopped/error/dead machine is (re)booted, reusing its
// previously allocated network resources. A stopped machine that carries a
// (fake, dev-only) hibernation snapshot resumes from it (boot=resumed) and the
// snapshot is consumed; otherwise it cold-boots (boot=cold).
func (d *DevDriver) EnsureRunning(ctx context.Context, spec driver.VMSpec) (string, error) {
	handle := state.Handle(spec.MachineID)
	diskID, diskMiB := "", 0
	if len(spec.Disks) > 0 {
		diskID, diskMiB = spec.Disks[0].ID, spec.Disks[0].SizeMiB
	}

	rec, existed, err := d.store.Reserve(spec.MachineID, func(a state.Alloc) state.Record {
		return state.Record{
			MachineID: spec.MachineID,
			Handle:    handle,
			State:     agentapi.StateCreating,
			Vcpus:     spec.Vcpus,
			MemMiB:    spec.MemMiB,
			KernelRef: spec.KernelRef,
			RootfsRef: spec.RootfsRef,
			DiskID:    diskID,
			DiskMiB:   diskMiB,
			Boot:      agentapi.BootCold,
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
		// Otherwise a (re)boot: refresh the desired spec, decide cold-vs-resume
		// from the persisted snapshot, and consume it. The real FC driver
		// restores guest RAM here; the dev driver only relaunches the guest
		// agent against the same persist dir, so PTY sessions do NOT survive —
		// only files on the persist dir do (documented; decision #10).
		boot := agentapi.BootCold
		if rec.Snapshot.Present {
			boot = agentapi.BootResumed
		}
		rec, _, err = d.store.Update(spec.MachineID, func(r *state.Record) {
			r.State = agentapi.StateCreating
			r.Reason = ""
			r.Vcpus = spec.Vcpus
			r.MemMiB = spec.MemMiB
			r.KernelRef = spec.KernelRef
			r.RootfsRef = spec.RootfsRef
			if diskID != "" {
				r.DiskID = diskID
				r.DiskMiB = diskMiB
			}
			r.Boot = boot
			r.Snapshot = state.SnapshotRecord{} // consumed on resume
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
	// The persist dir stands in for the real driver's persistent disk: it is
	// created once and kept across hibernate, so files written under $HOME /
	// workspace survive stop/start (decision #10). The guest agent's dir-mode
	// persist path (decision #7) uses it directly — no mkfs/mount in dev.
	persist := d.persistDir(machineID)
	if err := os.MkdirAll(persist, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir persist dir: %w", err)
	}

	env := append(os.Environ(),
		"PROTEOS_GUEST_LISTEN=unix:"+sock,
		"PROTEOS_GUEST_SHELL=/bin/bash",
		"PROTEOS_GUEST_PERSIST="+persist,
		"PROTEOS_GUEST_ENV_DIR="+d.envDir(machineID),
		// PP1: the generic port-preview forwarder (vsock:1026 in production). It
		// needs no backend config — it dials whatever loopback port the node-agent
		// names in the per-connection preamble — so it is always on in dev.
		"PROTEOS_GUEST_PREVIEW_LISTEN=unix:"+d.guestPreviewSockPath(machineID),
	)
	// Phase 8: when a web backend is configured, run the guest agent's web forward
	// on a per-machine socket (the dev stand-in for vsock port 1025) pointing at
	// that backend (a stub/real code-server). No PROTEOS_CODESERVER_BIN ⇒ the
	// forward assumes the backend is already up rather than supervising it.
	if d.guestWebBackend != "" {
		env = append(env,
			"PROTEOS_GUEST_WEB_LISTEN=unix:"+d.guestWebSockPath(machineID),
			"PROTEOS_GUEST_WEB_BACKEND="+d.guestWebBackend,
		)
	}

	cmd := exec.Command(d.guestAgentBin)
	cmd.Env = env
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

// Stop shuts the machine down. StopModeHibernate moves it through hibernating →
// stopped and records a (fake, dev-only) snapshot so the next ensure resumes;
// StopModePoweroff is a cold shutdown (stopping → stopped) that clears any
// snapshot. The child is signalled and (after a grace period) killed
// asynchronously either way — the persist dir is kept (only Destroy removes it).
func (d *DevDriver) Stop(ctx context.Context, machineID string, mode driver.StopMode) error {
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

	hibernate := mode != driver.StopModePoweroff // default + explicit hibernate
	transition := agentapi.StateStopping
	if hibernate {
		transition = agentapi.StateHibernating
	}
	if _, _, err := d.store.Update(machineID, func(r *state.Record) { r.State = transition }); err != nil {
		return err
	}

	d.mu.Lock()
	cmd := d.procs[machineID]
	d.mu.Unlock()

	go d.finishStop(machineID, cmd, hibernate)
	return nil
}

// finishStop terminates the child (graceful then hard) and records stopped. On
// hibernate it writes fake snapshot metadata (fc_version "dev"); on poweroff it
// clears any prior snapshot. The dev driver cannot capture guest RAM, so this
// metadata is the contract surface only — live sessions are lost on resume.
func (d *DevDriver) finishStop(machineID string, cmd *exec.Cmd, hibernate bool) {
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

	now := time.Now().UTC().Format(time.RFC3339)
	_, _, _ = d.store.Update(machineID, func(r *state.Record) {
		r.State = agentapi.StateStopped
		r.Pid = 0
		if hibernate {
			r.Snapshot = state.SnapshotRecord{
				Present:   true,
				CreatedAt: now,
				FCVersion: "dev",
				MemBytes:  int64(r.MemMiB) * 1024 * 1024,
			}
		} else {
			r.Snapshot = state.SnapshotRecord{}
		}
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

// DialGuest connects to the machine's guest agent over its terminal socket
// (guest.sock) or, for the Phase 8 web port, its web socket (guest-web.sock).
// The machine must be tracked (else ErrUnknownMachine); the HTTP layer is
// responsible for the running-state check and port allowlisting before calling
// this. A zero port means the terminal socket. A preview application port goes
// to the preview forwarder socket, preceded by the target-port preamble (the
// dev stand-in for the production vsock-1026 + preamble path).
func (d *DevDriver) DialGuest(ctx context.Context, machineID string, port uint32) (net.Conn, error) {
	if _, ok, err := d.store.Load(machineID); err != nil {
		return nil, err
	} else if !ok {
		return nil, driver.ErrUnknownMachine
	}
	sock := d.guestSockPath(machineID)
	var preamble string
	switch {
	case port == agentapi.GuestWebPort:
		sock = d.guestWebSockPath(machineID)
	case port != 0 && !agentapi.IsSystemGuestPort(port):
		sock = d.guestPreviewSockPath(machineID)
		preamble = agentapi.PreviewPreamble(port)
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}
	if preamble != "" {
		if _, err := conn.Write([]byte(preamble)); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
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
		case agentapi.StateStopping, agentapi.StateHibernating:
			hibernate := rec.State == agentapi.StateHibernating
			if alive {
				d.adopt(rec.MachineID, rec.Pid)
				go d.finishStop(rec.MachineID, d.procs[rec.MachineID], hibernate)
			} else {
				// Child already gone: finalize directly so we still record the
				// (fake) snapshot a mid-hibernate restart would otherwise lose.
				go d.finishStop(rec.MachineID, nil, hibernate)
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
		Boot:      rec.Boot,
		DiskID:    rec.DiskID,
		Snapshot: driver.Snapshot{
			Present:   rec.Snapshot.Present,
			CreatedAt: rec.Snapshot.CreatedAt,
			FCVersion: rec.Snapshot.FCVersion,
			MemBytes:  rec.Snapshot.MemBytes,
		},
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
