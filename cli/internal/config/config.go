// Package config loads and persists the proteos CLI's stored credentials and
// resolves the effective base URL + token for a command, applying the precedence
// the CLI documents: explicit flag > environment variable > stored file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Environment overrides. These let an agent / CI run with no prior `auth login`.
const (
	EnvURL   = "PROTEOS_URL"
	EnvToken = "PROTEOS_TOKEN"
)

// Credentials is the on-disk credential file (mode 0600). Token is a personal
// access token minted from the browser Settings → CLI tokens page.
type Credentials struct {
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
	Login   string `json:"login,omitempty"`
}

// Resolved is the effective configuration for a command after applying
// precedence, with TokenSource/URLSource recording where each value came from
// (for `auth status` and diagnostics). Token is never echoed in full.
type Resolved struct {
	BaseURL   string
	Token     string
	Login     string
	URLSource string
	TokSource string
}

// Path returns the credential file path: $XDG_CONFIG_HOME/proteos/credentials.json,
// falling back to ~/.config/proteos/credentials.json.
func Path() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "proteos", "credentials.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, ".config", "proteos", "credentials.json"), nil
}

// Load reads the stored credentials. A missing file is not an error — it returns
// a zero Credentials so first-run / env-only callers work.
func Load() (Credentials, error) {
	p, err := Path()
	if err != nil {
		return Credentials{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, nil
		}
		return Credentials{}, fmt.Errorf("read credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return Credentials{}, fmt.Errorf("parse credentials: %w", err)
	}
	return c, nil
}

// Save writes credentials to disk with 0600 perms, creating the parent dir 0700.
func Save(c Credentials) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}

// Delete removes the stored credential file (a no-op if it is absent).
func Delete() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove credentials: %w", err)
	}
	return nil
}

// Resolve computes the effective base URL + token. flagURL (from --url) wins over
// the environment, which wins over the stored file. The token has no flag in the
// general case — PROTEOS_TOKEN wins over the stored token.
func Resolve(flagURL string) (Resolved, error) {
	creds, err := Load()
	if err != nil {
		return Resolved{}, err
	}
	r := Resolved{Login: creds.Login}

	switch {
	case flagURL != "":
		r.BaseURL, r.URLSource = flagURL, "flag"
	case os.Getenv(EnvURL) != "":
		r.BaseURL, r.URLSource = os.Getenv(EnvURL), "env"
	default:
		r.BaseURL, r.URLSource = creds.BaseURL, "file"
	}

	if t := os.Getenv(EnvToken); t != "" {
		r.Token, r.TokSource = t, "env"
	} else {
		r.Token, r.TokSource = creds.Token, "file"
	}
	return r, nil
}
