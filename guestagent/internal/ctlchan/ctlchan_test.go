package ctlchan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	guestwire "github.com/tavon/proteos/guestagent/api"
	"github.com/tavon/proteos/guestagent/internal/runas"
)

func TestWriteGitConfig_NoSecretAndIdempotent(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	m := New([]string{"HOME=" + home, "PROTEOS_WORKSPACE=" + work}, runas.Root())

	p := guestwire.GitConfigurePayload{Name: "Ivan Pedrazas", Email: "ivan@example.com", Helper: guestwire.HelperBinPath}
	if err := m.writeGitConfig(p); err != nil {
		t.Fatalf("writeGitConfig: %v", err)
	}
	path := filepath.Join(home, ".gitconfig")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	got := string(first)
	for _, want := range []string{"name = Ivan Pedrazas", "email = ivan@example.com", "helper = " + guestwire.HelperBinPath, "useHttpPath = false"} {
		if !strings.Contains(got, want) {
			t.Fatalf("gitconfig missing %q:\n%s", want, got)
		}
	}
	// The config must never carry a token — only the helper wiring.
	for _, bad := range []string{"token", "password", "x-access-token", "ghp_", "gho_"} {
		if strings.Contains(strings.ToLower(got), bad) {
			t.Fatalf("gitconfig unexpectedly contains %q:\n%s", bad, got)
		}
	}

	// Idempotent: a second configure yields byte-identical content.
	if err := m.writeGitConfig(p); err != nil {
		t.Fatalf("writeGitConfig (2nd): %v", err)
	}
	second, _ := os.ReadFile(path)
	if string(second) != got {
		t.Fatalf("writeGitConfig not idempotent:\nfirst:\n%s\nsecond:\n%s", got, second)
	}
}

func TestValidateDest(t *testing.T) {
	work := "/workspace"
	m := New([]string{"PROTEOS_WORKSPACE=" + work}, runas.Root())
	cases := []struct {
		dest string
		ok   bool
	}{
		{"/workspace/hello", true},
		{"/workspace/owner-repo", true},
		{"/workspace", true},
		{"/workspace/../etc/passwd", false},
		{"/etc/passwd", false},
		{"/workspaceother/x", false},
		{"", false},
	}
	for _, c := range cases {
		err := m.validateDest(c.dest)
		if c.ok && err != nil {
			t.Errorf("validateDest(%q) = %v, want ok", c.dest, err)
		}
		if !c.ok && err == nil {
			t.Errorf("validateDest(%q) = ok, want error", c.dest)
		}
	}
}
