package secrets

import (
	"encoding/json"
	"fmt"
	"os"
)

// BackendConfig is the subset of control-plane config needed to construct a
// Store. It mirrors the PROTEOS_SECRETS_* / PROTEOS_OPENBAO_* env knobs so
// main.go can hand the resolved values straight through.
type BackendConfig struct {
	Backend       string // "file" | "openbao"
	File          string // file backend: path to the JSON store
	OpenBaoAddr   string
	OpenBaoMount  string
	OpenBaoPrefix string // optional path namespace inside the mount (e.g. "proteos")
	RoleID        string
	SecretIDFile  string
}

// Open constructs the Store selected by cfg.Backend. "file" (default) returns
// the dev FileStore; "openbao" returns a BaoStore authenticated via AppRole.
func Open(cfg BackendConfig) (Store, error) {
	switch cfg.Backend {
	case "", "file":
		return NewFileStore(cfg.File)
	case "openbao":
		return NewBaoStore(BaoConfig{
			Address:      cfg.OpenBaoAddr,
			Mount:        cfg.OpenBaoMount,
			Prefix:       cfg.OpenBaoPrefix,
			RoleID:       cfg.RoleID,
			SecretIDFile: cfg.SecretIDFile,
		})
	default:
		return nil, fmt.Errorf("secrets: unknown backend %q (want file|openbao)", cfg.Backend)
	}
}

// MigrateFromFile copies every secret in a dev FileStore JSON dump into dst
// (one-shot `controlplane -migrate-secrets`). It is a straight Put per path, so
// user paths flow through dst's per-user policy machinery exactly as a live
// write would. Returns the number of paths copied.
func MigrateFromFile(filePath string, dst Store) (int, error) {
	b, err := os.ReadFile(filePath)
	if err != nil {
		return 0, fmt.Errorf("read filestore dump: %w", err)
	}
	var all map[string]map[string]string
	if err := json.Unmarshal(b, &all); err != nil {
		return 0, fmt.Errorf("parse filestore dump: %w", err)
	}
	n := 0
	for path, data := range all {
		if err := dst.Put(path, data); err != nil {
			return n, fmt.Errorf("migrate %q: %w", path, err)
		}
		n++
	}
	return n, nil
}
