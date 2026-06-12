//go:build firecracker && linux

package firecracker

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Hibernate/resume (Phase 4 decision #4/#9), mirroring spike 05/08/09:
//
//	hibernate: PATCH /vm {Paused} → PUT /snapshot/create {Full → /state/snap/*}
//	resume:    PUT /snapshot/load {mem_backend:File, resume_vm:true}
//	           → guest PUT /resume {clock + entropy} → consume (rm) /state/snap/*
//
// Two hard spike findings gate this: restore needs the SAME Firecracker version
// (we guard on it and cold-boot otherwise) and the SAME tap name (persisted in
// the record), and the stale vsock uds must be removed before LoadSnapshot.

// pauseAndSnapshot pauses the running VM and writes a Full snapshot onto the
// encrypted volume. Returns the memory-file size in bytes (for snapshot
// metadata). The caller has the volume mounted at the jail's /state.
//
// uid is the jail uid the VMM was dropped to: the snapshot dir is created here
// (after the boot-time chown of /state), so Firecracker — running as that uid —
// can only write the mem/vmstate files if the dir is owned by it. Without this
// chown the create fails with "Permission denied (os error 13)".
func pauseAndSnapshot(ctx context.Context, api *fcAPI, layout jailLayout, uid int) (memBytes int64, err error) {
	if err := os.MkdirAll(layout.statePath(snapDir), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir snap dir: %w", err)
	}
	if err := os.Chown(layout.statePath(snapDir), uid, uid); err != nil {
		return 0, fmt.Errorf("chown snap dir to jail uid %d: %w", uid, err)
	}
	if err := api.do(ctx, http.MethodPatch, "/vm", vmStateBody{State: "Paused"}); err != nil {
		return 0, fmt.Errorf("pause vm: %w", err)
	}
	if err := api.put(ctx, "/snapshot/create", snapshotCreateBody{
		SnapshotType: "Full",
		SnapshotPath: inJailState(snapVMState),
		MemFilePath:  inJailState(snapMem),
	}); err != nil {
		return 0, fmt.Errorf("create snapshot: %w", err)
	}
	fi, err := os.Stat(layout.statePath(snapMem))
	if err != nil {
		return 0, fmt.Errorf("stat snapshot mem file: %w", err)
	}
	return fi.Size(), nil
}

// loadSnapshot restores the VM from the snapshot on /state and resumes it. The
// VMM must be freshly launched (no instance started) and the volume mounted.
func loadSnapshot(ctx context.Context, api *fcAPI) error {
	return api.put(ctx, "/snapshot/load", snapshotLoadBody{
		SnapshotPath: inJailState(snapVMState),
		MemBackend:   memBackend{BackendType: "File", BackendPath: inJailState(snapMem)},
		ResumeVM:     true,
	})
}

// consumeSnapshot deletes the snapshot files after a successful resume — stale
// guest RAM must never be restored twice (decision #4).
func consumeSnapshot(layout jailLayout) {
	_ = os.RemoveAll(layout.statePath(snapDir))
}

// snapshotExists reports whether a usable snapshot is present on the mounted
// volume (both the vm-state and the memory file).
func snapshotPresent(layout jailLayout) bool {
	return fileExists(layout.statePath(snapVMState)) && fileExists(layout.statePath(snapMem))
}

// installedFCVersion returns the Firecracker binary's reported version (e.g.
// "v1.16.0"), used for the restore version guard.
func (d *Driver) installedFCVersion() string {
	out, err := runOut(d.cfg.FirecrackerBin, "--version")
	if err != nil {
		return ""
	}
	// First line: "Firecracker v1.16.0". Return the v-prefixed token if present.
	for _, tok := range strings.Fields(firstLine(out)) {
		if strings.HasPrefix(tok, "v") {
			return tok
		}
	}
	return firstLine(out)
}

// callGuestResume drives the guest agent's resume hook over the vsock tunnel
// (decision #9): it sets the guest wall clock to the host's and reseeds the
// CRNG. Best-effort — a resume whose RAM restored successfully must not be
// aborted because the (possibly old) guest rootfs lacks the /resume route; we
// log loudly instead. Returns the corrected skew (host − guest, ms) the guest
// reported.
func (d *Driver) callGuestResume(ctx context.Context, machineID string) (int64, error) {
	conn, err := d.DialGuest(ctx, machineID)
	if err != nil {
		return 0, fmt.Errorf("dial guest for resume: %w", err)
	}
	defer conn.Close()

	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return 0, err
	}
	body, _ := json.Marshal(resumeBody{
		UnixNanos:  time.Now().UnixNano(),
		EntropyB64: base64.StdEncoding.EncodeToString(entropy),
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://guest/resume", strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Close = true // single-shot connection
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := req.Write(conn); err != nil {
		return 0, fmt.Errorf("write resume request: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return 0, fmt.Errorf("read resume response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("guest resume: HTTP %d", resp.StatusCode)
	}
	// A 200 already proves the hook ran (clock set + entropy reseeded); the skew
	// is telemetry, so a decode hiccup must not turn a successful resume into a
	// failure.
	var rr struct {
		SkewCorrectedMS int64 `json:"skew_corrected_ms"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rr)
	return rr.SkewCorrectedMS, nil
}

// --- request/response bodies -------------------------------------------------

type vmStateBody struct {
	State string `json:"state"` // "Paused" | "Resumed"
}

type snapshotCreateBody struct {
	SnapshotType string `json:"snapshot_type"` // "Full"
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type memBackend struct {
	BackendType string `json:"backend_type"` // "File"
	BackendPath string `json:"backend_path"`
}

type snapshotLoadBody struct {
	SnapshotPath string     `json:"snapshot_path"`
	MemBackend   memBackend `json:"mem_backend"`
	ResumeVM     bool       `json:"resume_vm"`
}

// resumeBody is the guest-agent /resume request (kept inline to avoid a
// cross-module import of guestwire).
type resumeBody struct {
	UnixNanos  int64  `json:"unix_nanos"`
	EntropyB64 string `json:"entropy_b64"`
}
