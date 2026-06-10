package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	api "github.com/tavon/proteos/nodeagent/api"
	"github.com/tavon/proteos/nodeagent/internal/driver/dev"
	"github.com/tavon/proteos/nodeagent/internal/httpapi"
	"github.com/tavon/proteos/nodeagent/internal/state"
)

const testToken = "test-bearer-token"

// fastBoot keeps the dev driver's simulated boot short so lifecycle tests run
// quickly while still exercising the async creating→running path.
const fastBoot = 30 * time.Millisecond

func newServer(t *testing.T) (*httptest.Server, *state.Store) {
	t.Helper()
	store, err := state.NewStore(t.TempDir(), netip.MustParsePrefix("172.30.0.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	drv := dev.New(store, fastBoot, "")
	ts := httptest.NewServer(httpapi.New(testToken, drv).Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

// do issues an authenticated request and returns the response.
func do(t *testing.T, ts *httptest.Server, method, path string, body any) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rdr)
	req.Header.Set(api.AuthHeader, api.BearerPrefix+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeStatus(t *testing.T, resp *http.Response) api.MachineStatus {
	t.Helper()
	defer resp.Body.Close()
	var st api.MachineStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st
}

// waitState polls GET /v1/machines/{id} until it reaches want or times out.
func waitState(t *testing.T, ts *httptest.Server, id, want string) api.MachineStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last api.MachineStatus
	for time.Now().Before(deadline) {
		resp := do(t, ts, http.MethodGet, "/v1/machines/"+id, nil)
		last = decodeStatus(t, resp)
		if last.State == want {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("machine %s never reached %q (last=%q reason=%q)", id, want, last.State, last.Reason)
	return last
}

func TestHealthzIsPublic(t *testing.T) {
	ts, _ := newServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: want 200, got %d", resp.StatusCode)
	}
}

func TestAuthRequired(t *testing.T) {
	ts, _ := newServer(t)
	id := "aaaaaaaa-0000-0000-0000-000000000001"

	// No bearer at all.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/machines/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", resp.StatusCode)
	}

	// Wrong bearer.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/v1/machines/"+id, nil)
	req.Header.Set(api.AuthHeader, api.BearerPrefix+"wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: want 401, got %d", resp.StatusCode)
	}
}

func TestLifecycle(t *testing.T) {
	ts, _ := newServer(t)
	id := "aaaaaaaa-0000-0000-0000-000000000001"

	// Ensure → 202 with handle.
	resp := do(t, ts, http.MethodPut, "/v1/machines/"+id, api.EnsureRequest{
		Vcpus: 2, MemMiB: 2048, KernelRef: "vmlinux-x", RootfsRef: "rootfs-x",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ensure: want 202, got %d", resp.StatusCode)
	}
	var ens api.EnsureResponse
	json.NewDecoder(resp.Body).Decode(&ens)
	resp.Body.Close()
	if ens.Handle != "fc-aaaaaaaa" {
		t.Fatalf("handle=%q, want fc-aaaaaaaa", ens.Handle)
	}

	// creating → running, with an allocated guest IP.
	st := waitState(t, ts, id, api.StateRunning)
	if st.GuestIP != "172.30.0.2" {
		t.Fatalf("guest_ip=%q, want 172.30.0.2", st.GuestIP)
	}
	if st.Handle != "fc-aaaaaaaa" {
		t.Fatalf("status handle=%q", st.Handle)
	}

	// Idempotent re-ensure returns the same handle, still running.
	resp = do(t, ts, http.MethodPut, "/v1/machines/"+id, api.EnsureRequest{Vcpus: 2, MemMiB: 2048, KernelRef: "vmlinux-x", RootfsRef: "rootfs-x"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("re-ensure: want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if got := waitState(t, ts, id, api.StateRunning); got.State != api.StateRunning {
		t.Fatalf("re-ensure changed state to %q", got.State)
	}

	// Stop → 202 → stopped.
	resp = do(t, ts, http.MethodPost, "/v1/machines/"+id+"/stop", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop: want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	waitState(t, ts, id, api.StateStopped)

	// List shows the one (stopped) machine.
	resp = do(t, ts, http.MethodGet, "/v1/machines", nil)
	defer resp.Body.Close()
	var list api.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Machines) != 1 || list.Machines[0].MachineID != id {
		t.Fatalf("list=%+v", list.Machines)
	}

	// Start again from stopped (cold boot via ensure) → running.
	resp2 := do(t, ts, http.MethodPut, "/v1/machines/"+id, api.EnsureRequest{Vcpus: 2, MemMiB: 2048, KernelRef: "vmlinux-x", RootfsRef: "rootfs-x"})
	resp2.Body.Close()
	waitState(t, ts, id, api.StateRunning)

	// Destroy → 204, then GET → 404.
	resp3 := do(t, ts, http.MethodDelete, "/v1/machines/"+id, nil)
	if resp3.StatusCode != http.StatusNoContent {
		t.Fatalf("destroy: want 204, got %d", resp3.StatusCode)
	}
	resp3.Body.Close()
	resp4 := do(t, ts, http.MethodGet, "/v1/machines/"+id, nil)
	if resp4.StatusCode != http.StatusNotFound {
		t.Fatalf("get after destroy: want 404, got %d", resp4.StatusCode)
	}
	resp4.Body.Close()
}

func TestFailBoot(t *testing.T) {
	ts, _ := newServer(t)
	id := "bbbbbbbb-0000-0000-0000-000000000002"

	resp := do(t, ts, http.MethodPut, "/v1/machines/"+id, api.EnsureRequest{
		Vcpus: 2, MemMiB: 2048, KernelRef: dev.FailBootRef, RootfsRef: "rootfs-x",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ensure: want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	st := waitState(t, ts, id, api.StateError)
	if st.Reason == "" {
		t.Fatalf("error state should carry a reason")
	}
}

func TestStopUnknownMachine(t *testing.T) {
	ts, _ := newServer(t)
	resp := do(t, ts, http.MethodPost, "/v1/machines/does-not-exist/stop", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stop unknown: want 404, got %d", resp.StatusCode)
	}
}

// TestReattachAfterRestart proves the agent re-adopts a live VM after a restart:
// boot via one driver, then build a *fresh* driver over the same on-disk store
// (as a restarted agent would) and confirm the machine is still running.
func TestReattachAfterRestart(t *testing.T) {
	dataDir := t.TempDir()
	subnet := netip.MustParsePrefix("172.30.0.0/24")
	store, err := state.NewStore(dataDir, subnet)
	if err != nil {
		t.Fatal(err)
	}
	id := "cccccccc-0000-0000-0000-000000000003"

	drv1 := dev.New(store, fastBoot, "")
	srv1 := httptest.NewServer(httpapi.New(testToken, drv1).Handler())
	resp := do(t, srv1, http.MethodPut, "/v1/machines/"+id, api.EnsureRequest{Vcpus: 1, MemMiB: 512, KernelRef: "k", RootfsRef: "r"})
	resp.Body.Close()
	waitState(t, srv1, id, api.StateRunning)
	srv1.Close() // agent goes away; the stub child keeps running

	// Restart: fresh store + driver over the same data dir, then Reattach.
	store2, err := state.NewStore(dataDir, subnet)
	if err != nil {
		t.Fatal(err)
	}
	drv2 := dev.New(store2, fastBoot, "")
	if err := drv2.Reattach(context.Background()); err != nil {
		t.Fatalf("reattach: %v", err)
	}
	srv2 := httptest.NewServer(httpapi.New(testToken, drv2).Handler())
	t.Cleanup(srv2.Close)

	st := decodeStatus(t, do(t, srv2, http.MethodGet, "/v1/machines/"+id, nil))
	if st.State != api.StateRunning {
		t.Fatalf("after reattach: state=%q, want running", st.State)
	}

	// The re-adopted machine is still controllable: stop works.
	do(t, srv2, http.MethodPost, "/v1/machines/"+id+"/stop", nil).Body.Close()
	waitState(t, srv2, id, api.StateStopped)
	_ = drv2.Destroy(context.Background(), id)
}
