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
