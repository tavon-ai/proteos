// Package persist owns the guest's persistent storage: the per-machine disk
// presented as /dev/vdb (Phase 4 decision #7) and the machine SQLite that lives
// on it (decision #8). It runs first at startup, before the session manager, so
// that $HOME and the workspace are on the disk before any shell spawns.
//
// Two modes:
//
//   - disk  — mount the block device at the persist mount point, fsck it first,
//     ensure home/ + workspace/, and bind-mount them over /root and /workspace
//     so the root shell's $HOME is on disk. Linux only.
//   - dir   — a plain directory (dev override, PROTEOS_GUEST_PERSIST): no mount,
//     no binds; $HOME is pointed at <dir>/home via the shell env instead.
//
// A missing device degrades to "none" (ephemeral) and serves terminals anyway,
// rather than refusing them — losing files is better than losing the machine.
package persist

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; keeps the agent CGO_ENABLED=0 static

	guestwire "github.com/tavon/proteos/guestagent/api"
)

// DefaultMountPoint is where the persistent disk is mounted in disk mode.
const DefaultMountPoint = "/persist"

// deviceWaitTimeout bounds how long disk mode waits for the block device to
// appear before degrading to ephemeral (decision #7: ≤10s).
const deviceWaitTimeout = 10 * time.Second

// Config configures Setup.
type Config struct {
	// Dir, when non-empty, selects dir mode and is used as the persist root.
	Dir string
	// Device is the block device mounted in disk mode (Dir empty). E.g. /dev/vdb.
	Device string
	// MountPoint is where Device is mounted in disk mode. Empty ⇒ DefaultMountPoint.
	MountPoint string
	// Version is the guest-agent version reported by Info.
	Version string
	// WaitTimeout bounds how long disk mode waits for the device. Zero ⇒
	// deviceWaitTimeout. Tests set a short value to avoid blocking.
	WaitTimeout time.Duration

	// --- Phase 8: unprivileged sessions -------------------------------------
	// When the guest runs PTY sessions as an unprivileged user, these locate and
	// own that user's $HOME. RunAsHome is the disk-mode bind target for the
	// persisted home (empty ⇒ /root, the legacy root-shell location). RunAsUID/
	// RunAsGID, when non-zero, own the persisted home + workspace so the user can
	// write them. RunAsUser names the user for the shell env (USER/LOGNAME). All
	// zero/empty ⇒ legacy root behavior, unchanged.
	RunAsHome string
	RunAsUID  int
	RunAsGID  int
	RunAsUser string
}

// Persist is the live persistence handle: a mode, a root directory, the home /
// workspace locations, and the machine SQLite. A nil-safe degraded handle
// (ModeNone) is returned when no storage is available.
type Persist struct {
	mode    string // guestwire.Persist*
	root    string // persist root (mount point or dir)
	home    string // resolved $HOME (on disk)
	work    string // workspace dir (on disk)
	user    string // unprivileged session user (USER/LOGNAME); empty ⇒ root
	version string

	mu sync.Mutex
	db *sql.DB
}

// Setup brings up persistence per cfg and records the cold-boot row. It never
// returns an error for "no disk" — it logs loudly and returns a degraded handle
// so terminals still work. It returns an error only for genuinely unexpected
// failures the caller may want to surface.
func Setup(cfg Config) (*Persist, error) {
	mount := cfg.MountPoint
	if mount == "" {
		mount = DefaultMountPoint
	}
	wait := cfg.WaitTimeout
	if wait == 0 {
		wait = deviceWaitTimeout
	}

	p := &Persist{version: cfg.Version}

	switch {
	case cfg.Dir != "":
		// Dir mode: plain directory, no mount, $HOME under it.
		p.mode = guestwire.PersistDir
		p.root = cfg.Dir
		p.home = filepath.Join(cfg.Dir, "home")
		p.work = filepath.Join(cfg.Dir, "workspace")
		p.user = cfg.RunAsUser
		if err := ensureDirs(p.home, p.work); err != nil {
			slog.Error("persist: dir mode setup failed; degrading to ephemeral", "err", err)
			return degraded(cfg.Version), nil
		}
	default:
		// Disk mode: mount the device and bind home/workspace over /root + /workspace.
		if err := mountDisk(cfg.Device, mount, wait); err != nil {
			slog.Error("persist: no persistent disk; running EPHEMERAL (files will NOT survive)",
				"device", cfg.Device, "err", err)
			return degraded(cfg.Version), nil
		}
		home := cfg.RunAsHome
		if home == "" {
			home = "/root"
		}
		p.mode = guestwire.PersistDisk
		p.root = mount
		p.home = home
		p.work = "/workspace"
		p.user = cfg.RunAsUser
		if err := setupDiskBinds(mount, home, cfg.RunAsUID, cfg.RunAsGID); err != nil {
			slog.Error("persist: bind setup failed; degrading to ephemeral", "err", err)
			return degraded(cfg.Version), nil
		}
	}

	db, err := openDB(filepath.Join(p.root, "machine.db"))
	if err != nil {
		slog.Error("persist: opening machine.db failed; degrading to ephemeral", "err", err)
		return degraded(cfg.Version), nil
	}
	p.db = db

	if err := p.recordBoot(guestwire.BootCold); err != nil {
		slog.Warn("persist: failed to record cold boot", "err", err)
	}
	slog.Info("persist: ready", "mode", p.mode, "root", p.root, "home", p.home)
	return p, nil
}

// degraded returns an ephemeral handle with no DB and no persistent paths.
func degraded(version string) *Persist {
	return &Persist{mode: guestwire.PersistNone, version: version}
}

// Mode reports the persistence mode (guestwire.Persist*).
func (p *Persist) Mode() string { return p.mode }

// ShellEnv returns the environment entries that put the shell's $HOME (and a
// WORKSPACE pointer) on the persistent disk. Empty in degraded mode.
func (p *Persist) ShellEnv() []string {
	if p.home == "" {
		return nil
	}
	env := []string{
		"HOME=" + p.home,
		"PROTEOS_WORKSPACE=" + p.work,
	}
	// When sessions run as an unprivileged user, advertise it so the shell, git,
	// and tools see the right identity (and the prompt isn't "I have no name!").
	if p.user != "" && p.user != "root" {
		env = append(env, "USER="+p.user, "LOGNAME="+p.user)
	}
	return env
}

// Resume applies the host-provided wall clock and entropy after a snapshot
// restore (decision #9), records a resumed-boot row, and returns the corrected
// skew in milliseconds. Safe to call in degraded mode (the boot row is skipped).
func (p *Persist) Resume(unixNanos int64, entropy []byte) (int64, error) {
	skewMS, err := applyResume(unixNanos, entropy)
	if err != nil {
		return 0, err
	}
	if p.db != nil {
		if err := p.recordBoot(guestwire.BootResumed); err != nil {
			slog.Warn("persist: failed to record resumed boot", "err", err)
		}
	}
	return skewMS, nil
}

// Info returns the GET /info payload.
func (p *Persist) Info() guestwire.Info {
	info := guestwire.Info{Version: p.version, Persist: p.mode}
	if b, ok := p.lastBoot(); ok {
		info.LastBoot = &b
	}
	return info
}

// Close closes the machine SQLite (best-effort). Safe on a degraded handle.
func (p *Persist) Close() error {
	if p.db == nil {
		return nil
	}
	return p.db.Close()
}

// --- SQLite ------------------------------------------------------------------

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// modernc's driver is fine with the default pool, but the machine DB is tiny
	// and single-writer; cap connections to avoid SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// migrate creates the Phase 4 schema if absent. Phase 9 extends it.
func migrate(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS boots (
    boot_id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind    TEXT NOT NULL,
    ts      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	// Record the schema version once.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (1)`); err != nil {
			return err
		}
	}
	return nil
}

func (p *Persist) recordBoot(kind string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.Exec(`INSERT INTO boots (kind, ts) VALUES (?, ?)`, kind, time.Now().Unix())
	return err
}

func (p *Persist) lastBoot() (guestwire.Boot, bool) {
	if p.db == nil {
		return guestwire.Boot{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var b guestwire.Boot
	err := p.db.QueryRow(`SELECT kind, ts FROM boots ORDER BY boot_id DESC LIMIT 1`).Scan(&b.Kind, &b.TS)
	if err != nil {
		return guestwire.Boot{}, false
	}
	return b, true
}

// Set writes a key/value pair into the machine kv table. Used by later phases
// (window layout, session index); exposed now so the schema has a live user.
func (p *Persist) Set(key, value string) error {
	if p.db == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.Exec(
		`INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	return err
}

// Get reads a kv value; ok is false if absent or in degraded mode.
func (p *Persist) Get(key string) (value string, ok bool) {
	if p.db == nil {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.db.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&value); err != nil {
		return "", false
	}
	return value, true
}

// ensureDirs creates each dir (0700) if missing.
func ensureDirs(dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
