package ctlchan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
)

// claude.configure merges the user's Claude Code preference into
// ~/.claude/settings.json. Unlike ~/.gitconfig (which git.configure fully
// overwrites as its single writer), users edit settings.json themselves, so
// this op only ever touches the keys it manages and leaves everything else
// intact:
//
//   - attribution off: force includeCoAuthoredBy=false and blank
//     attribution.commit / attribution.pr (empty string disables the line),
//     and drop a marker under ~/.proteos recording that we did.
//   - attribution on (the Claude Code default): remove exactly the values a
//     previous disable wrote — and only if the marker is present, so a user
//     who blanked attribution by hand is never fought with.
const (
	claudeSettingsRel     = ".claude/settings.json"
	attributionMarkerRel  = ".proteos/claude-attribution-managed"
	claudeIncludeCoAuthor = "includeCoAuthoredBy"
	claudeAttributionKey  = "attribution"
)

// applyClaudeConfig applies a claude.configure payload to the session user's
// Claude settings file.
func (m *Manager) applyClaudeConfig(p guestwire.ClaudeConfigurePayload) error {
	if p.Attribution {
		return m.restoreClaudeAttribution()
	}
	return m.suppressClaudeAttribution()
}

// suppressClaudeAttribution merges the blank-attribution keys into
// ~/.claude/settings.json (creating it if absent) and records the marker.
func (m *Manager) suppressClaudeAttribution() error {
	settings, err := m.readClaudeSettings()
	if err != nil {
		return err
	}
	if settings == nil {
		settings = map[string]any{}
	}
	settings[claudeIncludeCoAuthor] = false
	attr, _ := settings[claudeAttributionKey].(map[string]any)
	if attr == nil {
		attr = map[string]any{}
	}
	attr["commit"] = ""
	attr["pr"] = ""
	settings[claudeAttributionKey] = attr

	if err := m.writeClaudeSettings(settings); err != nil {
		return err
	}
	return m.writeAttributionMarker()
}

// restoreClaudeAttribution undoes a previous suppress: it removes the managed
// keys, but only when the marker says we wrote them and only if they still
// hold the values we set (the user may have replaced them with custom text —
// theirs wins). Without the marker the settings file is not touched at all.
func (m *Manager) restoreClaudeAttribution() error {
	marker := filepath.Join(m.homeDir, filepath.FromSlash(attributionMarkerRel))
	if _, err := os.Stat(marker); err != nil {
		return nil // nothing managed; never touch the user's file
	}
	settings, err := m.readClaudeSettings()
	if err != nil {
		return err
	}
	if settings != nil {
		if v, ok := settings[claudeIncludeCoAuthor].(bool); ok && !v {
			delete(settings, claudeIncludeCoAuthor)
		}
		if attr, ok := settings[claudeAttributionKey].(map[string]any); ok {
			if s, ok := attr["commit"].(string); ok && s == "" {
				delete(attr, "commit")
			}
			if s, ok := attr["pr"].(string); ok && s == "" {
				delete(attr, "pr")
			}
			if len(attr) == 0 {
				delete(settings, claudeAttributionKey)
			}
		}
		if err := m.writeClaudeSettings(settings); err != nil {
			return err
		}
	}
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove attribution marker: %w", err)
	}
	return nil
}

// readClaudeSettings loads ~/.claude/settings.json as a generic object. A
// missing file yields (nil, nil). Invalid JSON is an error — the file is
// user-edited, and a merge must never destroy content it cannot parse.
func (m *Manager) readClaudeSettings() (map[string]any, error) {
	path := filepath.Join(m.homeDir, filepath.FromSlash(claudeSettingsRel))
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read claude settings: %w", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(b, &settings); err != nil {
		return nil, fmt.Errorf("claude settings is not valid JSON, refusing to modify: %w", err)
	}
	return settings, nil
}

// writeClaudeSettings writes the settings object back, indented (the file is
// user-edited), owned by the session user like every $HOME file the agent
// (running as root) creates.
func (m *Manager) writeClaudeSettings(settings map[string]any) error {
	path := filepath.Join(m.homeDir, filepath.FromSlash(claudeSettingsRel))
	if err := m.mkdirOwned(filepath.Dir(path)); err != nil {
		return err
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claude settings: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write claude settings: %w", err)
	}
	return m.owner.Chown(path)
}

// writeAttributionMarker records that the managed attribution keys are ours to
// remove on a later re-enable.
func (m *Manager) writeAttributionMarker() error {
	path := filepath.Join(m.homeDir, filepath.FromSlash(attributionMarkerRel))
	if err := m.mkdirOwned(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte("managed\n"), 0o600); err != nil {
		return fmt.Errorf("write attribution marker: %w", err)
	}
	return m.owner.Chown(path)
}

// mkdirOwned creates a directory under $HOME (if needed) owned by the session
// user, so the user can read and edit what the root agent placed there.
func (m *Manager) mkdirOwned(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return m.owner.Chown(dir)
}
