// Package runas resolves the unprivileged OS user that the guest's interactive
// shells and AI-agent processes run as (the baked-in `dev` user, uid 1000). The
// guest agent itself stays root — it must mount the persistent disk, bind-mount
// home/workspace, and listen on vsock — but every PTY it spawns drops to this
// identity so a rogue agent command is a non-root process and tools (npm, git,
// installers) behave the way they do on a real workstation.
//
// Resolution degrades gracefully: if the named user is absent (an older rootfs
// without the user, or a unit test on a developer's Mac) Resolve returns the
// root identity, which reproduces the pre-user behavior exactly — no credential
// switch, no chowns. This keeps the change safe to ship over existing images.
package runas

import (
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// Identity is the resolved run-as user: who PTY sessions run as and where their
// $HOME lives. The zero value is not valid; obtain one from Resolve.
type Identity struct {
	Name   string // login name, e.g. "dev"
	UID    int
	GID    int
	Home   string // home directory, e.g. "/home/dev"
	IsRoot bool   // true when no unprivileged user applies (degraded/root)
}

// rootIdentity reproduces the historical behavior: everything runs as root with
// $HOME=/root. Returned whenever an unprivileged user cannot be resolved.
func rootIdentity() Identity {
	return Identity{Name: "root", UID: 0, GID: 0, Home: "/root", IsRoot: true}
}

// Root returns the root identity — the legacy/degraded case where sessions run
// as the agent itself (no credential switch, no chowns). Handy in tests and as
// an explicit "no unprivileged user" value.
func Root() Identity { return rootIdentity() }

// Resolve looks up name and returns its Identity. An empty name, "root", a
// lookup miss, or an unparseable uid/gid all degrade to the root identity (with
// a warning for the miss case) so the guest never refuses to boot over identity.
func Resolve(name string) Identity {
	if name == "" || name == "root" {
		return rootIdentity()
	}
	u, err := user.Lookup(name)
	if err != nil {
		slog.Warn("runas: user not found; running sessions as root", "user", name, "err", err)
		return rootIdentity()
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		slog.Warn("runas: unparseable uid; running sessions as root", "user", name, "uid", u.Uid)
		return rootIdentity()
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		slog.Warn("runas: unparseable gid; running sessions as root", "user", name, "gid", u.Gid)
		return rootIdentity()
	}
	home := u.HomeDir
	if home == "" {
		home = filepath.Join("/home", name)
	}
	return Identity{Name: name, UID: uid, GID: gid, Home: home, IsRoot: uid == 0}
}

// Credential returns the syscall.Credential a child process should run under, or
// nil for the root identity (inherit the agent's identity unchanged).
func (i Identity) Credential() *syscall.Credential {
	if i.IsRoot {
		return nil
	}
	return &syscall.Credential{Uid: uint32(i.UID), Gid: uint32(i.GID)}
}

// Chown sets path's owner to this identity. A no-op for the root identity (the
// files are already root-owned). Best-effort callers should log, not fail: a
// chown miss degrades access, it does not corrupt anything.
func (i Identity) Chown(path string) error {
	if i.IsRoot {
		return nil
	}
	return os.Chown(path, i.UID, i.GID)
}

// ChownTree recursively chowns path and everything under it. A no-op for the
// root identity. Used for the persisted home/workspace, which must be writable
// by the unprivileged user (and may carry root-owned files from a prior boot of
// an older image).
func (i Identity) ChownTree(path string) error {
	if i.IsRoot {
		return nil
	}
	return filepath.Walk(path, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(p, i.UID, i.GID)
	})
}
