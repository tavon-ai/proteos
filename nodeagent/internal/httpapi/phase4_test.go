package httpapi_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	api "github.com/tavon/proteos/nodeagent/api"
)

// syncBuffer is a concurrency-safe buffer: the global slog default is shared
// with background boot/stop goroutines, so the log sink must tolerate concurrent
// writes (and a concurrent read) without a data race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestStopHibernateRoundTrip drives the Phase 4 HTTP surface: ensure with a disk
// + volume key, hibernate via the stop body, then re-ensure (resume). It asserts
// the snapshot metadata and boot kind round-trip through the JSON state store
// and the GET status response.
func TestStopHibernateRoundTrip(t *testing.T) {
	ts, _ := newServer(t)
	id := "dddddddd-0000-0000-0000-000000000004"
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

	resp := do(t, ts, http.MethodPut, "/v1/machines/"+id, api.EnsureRequest{
		Vcpus: 2, MemMiB: 2048, KernelRef: "k", RootfsRef: "r",
		DiskID: "disk-1", DiskMiB: 10240, VolumeKeyB64: key,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ensure: want 202, got %d", resp.StatusCode)
	}

	st := waitState(t, ts, id, api.StateRunning)
	if st.Boot != api.BootCold {
		t.Fatalf("first boot: got %q, want cold", st.Boot)
	}
	if st.DiskID != "disk-1" {
		t.Fatalf("disk_id: got %q", st.DiskID)
	}

	// Hibernate with an explicit body.
	resp = do(t, ts, http.MethodPost, "/v1/machines/"+id+"/stop", api.StopRequest{Mode: api.StopModeHibernate})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop: want 202, got %d", resp.StatusCode)
	}
	st = waitState(t, ts, id, api.StateStopped)
	if !st.Snapshot.Present || st.Snapshot.FCVersion != "dev" {
		t.Fatalf("snapshot after hibernate: %+v", st.Snapshot)
	}

	// Resume.
	resp = do(t, ts, http.MethodPut, "/v1/machines/"+id, api.EnsureRequest{
		Vcpus: 2, MemMiB: 2048, KernelRef: "k", RootfsRef: "r", DiskID: "disk-1", DiskMiB: 10240, VolumeKeyB64: key,
	})
	resp.Body.Close()
	st = waitState(t, ts, id, api.StateRunning)
	if st.Boot != api.BootResumed {
		t.Fatalf("after resume: boot=%q, want resumed", st.Boot)
	}
	if st.Snapshot.Present {
		t.Fatalf("snapshot should be consumed after resume")
	}
}

// TestStopDefaultsToHibernate proves an empty stop body hibernates (decision #4).
func TestStopDefaultsToHibernate(t *testing.T) {
	ts, _ := newServer(t)
	id := "eeeeeeee-0000-0000-0000-000000000005"
	do(t, ts, http.MethodPut, "/v1/machines/"+id, api.EnsureRequest{Vcpus: 1, MemMiB: 512, KernelRef: "k", RootfsRef: "r"}).Body.Close()
	waitState(t, ts, id, api.StateRunning)

	do(t, ts, http.MethodPost, "/v1/machines/"+id+"/stop", nil).Body.Close()
	st := waitState(t, ts, id, api.StateStopped)
	if !st.Snapshot.Present {
		t.Fatalf("empty stop body should default to hibernate; snapshot=%+v", st.Snapshot)
	}
}

// TestStopRejectsBadMode proves an unknown stop mode is a 400.
func TestStopRejectsBadMode(t *testing.T) {
	ts, _ := newServer(t)
	id := "ffffffff-0000-0000-0000-000000000006"
	do(t, ts, http.MethodPut, "/v1/machines/"+id, api.EnsureRequest{Vcpus: 1, MemMiB: 512, KernelRef: "k", RootfsRef: "r"}).Body.Close()
	waitState(t, ts, id, api.StateRunning)

	resp := do(t, ts, http.MethodPost, "/v1/machines/"+id+"/stop", api.StopRequest{Mode: "bogus"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad mode: want 400, got %d", resp.StatusCode)
	}
}

// TestVolumeKeyNeverLogged captures all slog output during an ensure carrying a
// volume key and asserts the key never appears in any log line (decision #2).
func TestVolumeKeyNeverLogged(t *testing.T) {
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ts, _ := newServer(t)
	id := "aaaaaaaa-0000-0000-0000-0000000000aa"
	secret := "this-is-the-volume-key-do-not-log"
	keyB64 := base64.StdEncoding.EncodeToString([]byte(secret))

	resp := do(t, ts, http.MethodPut, "/v1/machines/"+id, api.EnsureRequest{
		Vcpus: 1, MemMiB: 512, KernelRef: "k", RootfsRef: "r", VolumeKeyB64: keyB64,
	})
	resp.Body.Close()
	waitState(t, ts, id, api.StateRunning)

	logs := buf.String()
	if strings.Contains(logs, secret) || strings.Contains(logs, keyB64) {
		t.Fatalf("volume key leaked into logs:\n%s", logs)
	}
}

// TestStatusJSONShapesSnapshot guards the wire shape: snapshot is always present
// in the status JSON (present:false when not hibernated).
func TestStatusJSONShapesSnapshot(t *testing.T) {
	ts, _ := newServer(t)
	id := "bbbbbbbb-0000-0000-0000-0000000000bb"
	do(t, ts, http.MethodPut, "/v1/machines/"+id, api.EnsureRequest{Vcpus: 1, MemMiB: 512, KernelRef: "k", RootfsRef: "r"}).Body.Close()
	waitState(t, ts, id, api.StateRunning)

	resp := do(t, ts, http.MethodGet, "/v1/machines/"+id, nil)
	defer resp.Body.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["snapshot"]; !ok {
		t.Fatalf("status JSON missing snapshot field: %v", raw)
	}
}
