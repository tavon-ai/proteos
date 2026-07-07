//go:build firecracker && linux

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// fcAPI talks to one Firecracker VMM over its unix-domain API socket. It mirrors
// the spike's lib.sh fc_api helper: every configuration PUT happens before
// InstanceStart (Firecracker cannot hot-add devices), and HTTP faults surface
// the VMM's JSON fault_message rather than a bare status code.
type fcAPI struct {
	sock string
	http *http.Client
}

func newFCAPI(socketPath string) *fcAPI {
	return &fcAPI{
		sock: socketPath,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// put issues a PUT with a JSON body; Firecracker returns 204 on success.
func (a *fcAPI) put(ctx context.Context, path string, body any) error {
	return a.do(ctx, http.MethodPut, path, body)
}

// do performs one request and turns any non-2xx into an error carrying the
// response body (Firecracker's fault_message).
func (a *fcAPI) do(ctx context.Context, method, path string, body any) error {
	var buf bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s: %w", path, err)
		}
		buf = *bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

// instanceState returns the VMM's reported instance state ("Running",
// "Not started", etc.) from GET /.
func (a *fcAPI) instanceState(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
	if err != nil {
		return "", err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.State, nil
}

// --- request bodies (subset of the Firecracker API we use) -------------------

type machineConfig struct {
	VcpuCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type networkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

type action struct {
	ActionType string `json:"action_type"`
}

// vsockDevice configures the VM's single virtio-vsock device (pre-boot, like
// NICs — no hot-add). UDSPath is relative to the jail chroot; Firecracker
// creates the socket there and the host reaches the guest via the hybrid
// CONNECT/OK handshake on it.
type vsockDevice struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// loggerConfig enables Firecracker's structured JSON logger, directing output
// to a named pipe at LogPath (relative to the jail chroot).
type loggerConfig struct {
	LogPath   string `json:"log_path"`
	Level     string `json:"level"`
	ShowLevel bool   `json:"show_level"`
}
