// Package config parses the guest agent's configuration from the environment.
// The guest agent runs inside the microVM (or, in dev, as a child of the
// node-agent); its only knobs are where to listen, what shell to spawn, and how
// much scrollback to keep.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config is the fully-resolved guest-agent configuration.
type Config struct {
	// Listen is the listener spec: "vsock:<port>", "unix:<path>", or
	// "tcp:<host:port>". In production this is vsock:1024; the dev driver sets
	// unix:<datadir>/machines/<id>/guest.sock; tests use tcp:127.0.0.1:0.
	Listen string

	// Shell is the program run as `<shell> -l` for each session.
	Shell string

	// ScrollbackKiB is the per-session scrollback ring size.
	ScrollbackKiB int

	// --- Phase 4: persistent disk -------------------------------------------

	// PersistDir (PROTEOS_GUEST_PERSIST), when set, is the dev override: the
	// guest agent uses this plain directory as the persist root and skips
	// mounting (decision #7). Empty ⇒ disk mode (mount PersistDevice).
	PersistDir string

	// PersistDevice (PROTEOS_GUEST_PERSIST_DEV) is the block device to mount at
	// the persist mount point in disk mode. Default /dev/vdb.
	PersistDevice string

	// --- Phase 5: secret injection ------------------------------------------

	// EnvDir (PROTEOS_GUEST_ENV_DIR) is the directory holding injected provider
	// env files. It must be tmpfs (never the rootfs image or persistent disk);
	// default /run/proteos/env.
	EnvDir string

	// --- Phase 8: unprivileged sessions -------------------------------------

	// RunAsUser (PROTEOS_GUEST_RUN_AS_USER) is the unprivileged OS user that PTY
	// sessions (shells + agent CLIs) run as; default "dev". The guest agent
	// itself stays root. Set to "root" (or "") to keep the legacy all-root
	// behavior. If the named user does not exist, sessions fall back to root.
	RunAsUser string

	// --- Phase 8: code-server web forward -----------------------------------

	// WebListen (PROTEOS_GUEST_WEB_LISTEN) is the listener spec for the code-server
	// web forward (decision #4): "vsock:1025" in production, "unix:<path>" in dev.
	// Empty ⇒ the web forward is disabled (terminal-only). The forward raw-copies
	// bytes between this listener and WebBackend; the node-agent tunnel reaches it
	// on agentapi.GuestWebPort.
	WebListen string

	// WebBackend (PROTEOS_GUEST_WEB_BACKEND) is the loopback address code-server
	// binds and the forward dials. Default 127.0.0.1:13337 (decision #5).
	WebBackend string

	// CodeServerBin (PROTEOS_CODESERVER_BIN), when set, makes the web forward
	// lazily start and supervise code-server at WebBackend (decision #5): first
	// web connection starts it, the forward health-gates it, and a crash restarts
	// it with backoff. Empty ⇒ the forward assumes WebBackend is already up (dev/
	// e2e: a stub server), so the supervisor is bypassed.
	CodeServerBin string

	// CodeServerArgs (PROTEOS_CODESERVER_ARGS) overrides the baked code-server
	// flags (space-split). Empty ⇒ the decision #5 defaults built from WebBackend
	// and the persist-backed user-data/extensions dirs.
	CodeServerArgs string

	// --- PP1: port-preview forward ------------------------------------------

	// PreviewListen (PROTEOS_GUEST_PREVIEW_LISTEN) is the listener spec for the
	// generic port-preview forward: "vsock:1026" in production, "unix:<path>" in
	// dev. Empty ⇒ port preview is disabled. The forward reads a one-line target-
	// port preamble from each connection (written by the node-agent) and bridges
	// to 127.0.0.1:<port> inside the VM; the node-agent tunnel reaches it on
	// agentapi.GuestPreviewPort. There is no backend config and no supervisor —
	// the user's own process is the backend.
	PreviewListen string
}

// Load reads and validates configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		Listen:        getenv("PROTEOS_GUEST_LISTEN", "vsock:1024"),
		Shell:         getenv("PROTEOS_GUEST_SHELL", "/bin/bash"),
		ScrollbackKiB: getenvInt("PROTEOS_GUEST_SCROLLBACK_KIB", 256),
		PersistDir:    os.Getenv("PROTEOS_GUEST_PERSIST"),
		PersistDevice: getenv("PROTEOS_GUEST_PERSIST_DEV", "/dev/vdb"),
		EnvDir:        getenv("PROTEOS_GUEST_ENV_DIR", "/run/proteos/env"),
		RunAsUser:     getenv("PROTEOS_GUEST_RUN_AS_USER", "dev"),

		WebListen:      os.Getenv("PROTEOS_GUEST_WEB_LISTEN"),
		WebBackend:     getenv("PROTEOS_GUEST_WEB_BACKEND", "127.0.0.1:13337"),
		CodeServerBin:  os.Getenv("PROTEOS_CODESERVER_BIN"),
		CodeServerArgs: os.Getenv("PROTEOS_CODESERVER_ARGS"),

		PreviewListen: os.Getenv("PROTEOS_GUEST_PREVIEW_LISTEN"),
	}
	if c.ScrollbackKiB < 1 {
		return nil, fmt.Errorf("PROTEOS_GUEST_SCROLLBACK_KIB must be ≥ 1, got %d", c.ScrollbackKiB)
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
