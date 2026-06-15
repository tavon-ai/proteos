package runas

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func TestResolveDegradesToRoot(t *testing.T) {
	for _, name := range []string{"", "root", "definitely-no-such-user-xyz"} {
		id := Resolve(name)
		if !id.IsRoot {
			t.Errorf("Resolve(%q): want root identity, got %+v", name, id)
		}
		if id.UID != 0 || id.Home != "/root" {
			t.Errorf("Resolve(%q): want uid 0 home /root, got %+v", name, id)
		}
		if id.Credential() != nil {
			t.Errorf("Resolve(%q): root identity must have nil Credential", name)
		}
	}
}

func TestResolveExistingUser(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skipf("cannot determine current user: %v", err)
	}
	id := Resolve(cur.Username)
	if id.Name != cur.Username {
		t.Fatalf("Name = %q, want %q", id.Name, cur.Username)
	}
	if id.Home != cur.HomeDir {
		t.Errorf("Home = %q, want %q", id.Home, cur.HomeDir)
	}
	// A non-root current user must yield a credential; root must not.
	if id.IsRoot != (id.UID == 0) {
		t.Errorf("IsRoot=%v but UID=%d", id.IsRoot, id.UID)
	}
	if id.IsRoot && id.Credential() != nil {
		t.Error("root identity must have nil Credential")
	}
	if !id.IsRoot && id.Credential() == nil {
		t.Error("non-root identity must have a Credential")
	}
}

func TestChownNoopForRoot(t *testing.T) {
	// Root identity chowns must be no-ops and never error, even on a path we do
	// not own — proving the legacy path touches nothing.
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Root()
	if err := r.Chown(f); err != nil {
		t.Errorf("Root().Chown: %v", err)
	}
	if err := r.ChownTree(dir); err != nil {
		t.Errorf("Root().ChownTree: %v", err)
	}
}
