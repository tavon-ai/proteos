package ctlchan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	"github.com/tavon-ai/proteos/guestagent/internal/runas"
)

func newClaudeTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	return New([]string{"HOME=" + home}, runas.Root(), nil, nil), home
}

func readSettings(t *testing.T, home string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("settings not valid JSON: %v", err)
	}
	return m
}

func writeSettings(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func markerPath(home string) string {
	return filepath.Join(home, ".proteos", "claude-attribution-managed")
}

func TestSuppressAttribution_CreatesFileAndMarker(t *testing.T) {
	m, home := newClaudeTestManager(t)

	if err := m.applyClaudeConfig(guestwire.ClaudeConfigurePayload{Attribution: false}); err != nil {
		t.Fatalf("suppress: %v", err)
	}

	s := readSettings(t, home)
	if v, ok := s["includeCoAuthoredBy"].(bool); !ok || v {
		t.Errorf("includeCoAuthoredBy = %v, want false", s["includeCoAuthoredBy"])
	}
	attr, _ := s["attribution"].(map[string]any)
	if attr == nil || attr["commit"] != "" || attr["pr"] != "" {
		t.Errorf("attribution = %v, want blank commit/pr", s["attribution"])
	}
	if _, err := os.Stat(markerPath(home)); err != nil {
		t.Errorf("marker not written: %v", err)
	}
}

func TestSuppressAttribution_PreservesUserKeys(t *testing.T) {
	m, home := newClaudeTestManager(t)
	writeSettings(t, home, `{"model":"opus","attribution":{"commit":"custom line"},"env":{"FOO":"bar"}}`)

	if err := m.applyClaudeConfig(guestwire.ClaudeConfigurePayload{Attribution: false}); err != nil {
		t.Fatalf("suppress: %v", err)
	}

	s := readSettings(t, home)
	if s["model"] != "opus" {
		t.Errorf("model clobbered: %v", s["model"])
	}
	env, _ := s["env"].(map[string]any)
	if env == nil || env["FOO"] != "bar" {
		t.Errorf("env clobbered: %v", s["env"])
	}
	attr, _ := s["attribution"].(map[string]any)
	if attr["commit"] != "" || attr["pr"] != "" {
		t.Errorf("attribution not blanked: %v", attr)
	}
}

func TestRestoreAttribution_RemovesOnlyManagedValues(t *testing.T) {
	m, home := newClaudeTestManager(t)
	writeSettings(t, home, `{"model":"opus"}`)

	if err := m.applyClaudeConfig(guestwire.ClaudeConfigurePayload{Attribution: false}); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	if err := m.applyClaudeConfig(guestwire.ClaudeConfigurePayload{Attribution: true}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	s := readSettings(t, home)
	if _, ok := s["includeCoAuthoredBy"]; ok {
		t.Errorf("includeCoAuthoredBy left behind: %v", s["includeCoAuthoredBy"])
	}
	if _, ok := s["attribution"]; ok {
		t.Errorf("attribution left behind: %v", s["attribution"])
	}
	if s["model"] != "opus" {
		t.Errorf("model clobbered: %v", s["model"])
	}
	if _, err := os.Stat(markerPath(home)); !os.IsNotExist(err) {
		t.Errorf("marker not removed: %v", err)
	}
}

func TestRestoreAttribution_KeepsUserOverriddenValues(t *testing.T) {
	m, home := newClaudeTestManager(t)

	if err := m.applyClaudeConfig(guestwire.ClaudeConfigurePayload{Attribution: false}); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	// The user replaced the managed blank with custom text; a restore must keep it.
	writeSettings(t, home, `{"includeCoAuthoredBy":false,"attribution":{"commit":"my custom line","pr":""}}`)

	if err := m.applyClaudeConfig(guestwire.ClaudeConfigurePayload{Attribution: true}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	s := readSettings(t, home)
	attr, _ := s["attribution"].(map[string]any)
	if attr == nil || attr["commit"] != "my custom line" {
		t.Errorf("user's custom attribution.commit lost: %v", s["attribution"])
	}
	if _, ok := attr["pr"]; ok {
		t.Errorf("managed blank attribution.pr left behind")
	}
	// includeCoAuthoredBy=false is exactly the managed value, so it is removed.
	if _, ok := s["includeCoAuthoredBy"]; ok {
		t.Errorf("managed includeCoAuthoredBy left behind")
	}
}

func TestRestoreAttribution_NoMarkerNeverTouchesFile(t *testing.T) {
	m, home := newClaudeTestManager(t)
	// The user disabled attribution by hand — no marker, so hands off.
	writeSettings(t, home, `{"includeCoAuthoredBy":false,"attribution":{"commit":"","pr":""}}`)

	if err := m.applyClaudeConfig(guestwire.ClaudeConfigurePayload{Attribution: true}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	s := readSettings(t, home)
	if v, ok := s["includeCoAuthoredBy"].(bool); !ok || v {
		t.Errorf("hand-disabled includeCoAuthoredBy was modified: %v", s["includeCoAuthoredBy"])
	}
	attr, _ := s["attribution"].(map[string]any)
	if attr == nil || attr["commit"] != "" || attr["pr"] != "" {
		t.Errorf("hand-blanked attribution was modified: %v", s["attribution"])
	}
}

func TestSuppressAttribution_RefusesInvalidJSON(t *testing.T) {
	m, home := newClaudeTestManager(t)
	writeSettings(t, home, `{not json`)

	if err := m.applyClaudeConfig(guestwire.ClaudeConfigurePayload{Attribution: false}); err == nil {
		t.Fatal("suppress on invalid JSON should error, not clobber")
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil || string(b) != `{not json` {
		t.Errorf("user's (broken) file was modified: %q err=%v", b, err)
	}
	if _, err := os.Stat(markerPath(home)); !os.IsNotExist(err) {
		t.Errorf("marker written despite failure")
	}
}

func TestSuppressAttribution_Idempotent(t *testing.T) {
	m, home := newClaudeTestManager(t)

	for i := range 2 {
		if err := m.applyClaudeConfig(guestwire.ClaudeConfigurePayload{Attribution: false}); err != nil {
			t.Fatalf("suppress #%d: %v", i+1, err)
		}
	}
	s := readSettings(t, home)
	attr, _ := s["attribution"].(map[string]any)
	if attr == nil || attr["commit"] != "" || attr["pr"] != "" {
		t.Errorf("attribution = %v after double suppress", s["attribution"])
	}
}

func TestRestoreAttribution_Idempotent(t *testing.T) {
	m, _ := newClaudeTestManager(t)

	// Restore with nothing managed and no settings file at all: clean no-op.
	for i := range 2 {
		if err := m.applyClaudeConfig(guestwire.ClaudeConfigurePayload{Attribution: true}); err != nil {
			t.Fatalf("restore #%d: %v", i+1, err)
		}
	}
}
