package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"time"

	guestwire "github.com/tavon/proteos/guestagent/api"
	agentapi "github.com/tavon/proteos/nodeagent/api"

	"github.com/tavon/proteos/controlplane/internal/machine"
)

// downloadDialTimeout bounds the tunnel dial + guest handshake for a download.
// It does NOT bound the zip stream itself: once the guest responds with headers,
// the body copy runs under the request context and lives as long as the browser
// keeps reading (a large repo can take a while to compress and transfer).
const downloadDialTimeout = 30 * time.Second

// handleProjectDownload streams one of the machine's projects to the browser as
// a zip. The requested project path is authorized exactly like a session cwd —
// it must match one of the machine's listable projects (resolveSessionCwd) — so
// the browser can never name an arbitrary path on the guest disk. The handler
// then dials the guest tunnel, issues GET /download, and copies the zip stream
// straight through. It is a GET (read-only): the attachment response downloads
// without leaving the desktop, and no CSRF header is required.
func (s *Server) handleProjectDownload(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	m, err := s.resolveTerminalMachine(r.Context(), user, r.URL.Query().Get("machine"))
	if err != nil {
		// A foreign or absent machine both surface as 404 (no existence leak).
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	if machine.State(m.State) != machine.StateRunning {
		writeError(w, http.StatusConflict, "machine_not_running")
		return
	}
	machineID := machine.UUIDString(m.ID)

	// Authorization gate: the requested path must equal one of this machine's
	// listable project roots (the same check the terminal cwd uses).
	projectPath, errCode := s.resolveSessionCwd(r.Context(), machineID, r.URL.Query().Get("path"))
	if errCode != "" {
		writeError(w, cwdErrorStatus(errCode), errCode)
		return
	}
	if projectPath == "" {
		writeError(w, http.StatusBadRequest, "bad_path")
		return
	}

	// Dial the guest tunnel and GET /download over a one-shot HTTP client whose
	// transport returns the tunnel for its single connection (the same trick the
	// injector and gateway use for the guest WebSocket).
	dialCtx, cancel := context.WithTimeout(r.Context(), downloadDialTimeout)
	defer cancel()
	tunnel, err := s.Guests.DialGuest(dialCtx, machineID, agentapi.GuestTerminalPort)
	if err != nil {
		writeError(w, http.StatusBadGateway, "guest_unreachable")
		return
	}
	defer tunnel.Close()

	used := false
	client := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				if used {
					return nil, errors.New("download: guest tunnel already consumed")
				}
				used = true
				return tunnel, nil
			},
		},
	}

	q := url.Values{}
	q.Set(guestwire.QueryParamCwd, projectPath)
	guestURL := "http://guest" + guestwire.RouteDownloadPath + "?" + q.Encode()
	// The request body copy uses the browser's request context, so a client
	// disconnect tears the guest stream down with it.
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, guestURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "error")
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "guest_unreachable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, "guest_unreachable")
		return
	}

	name := path.Base(projectPath) + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering of the stream
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
}
