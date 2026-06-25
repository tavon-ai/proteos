package ctlchan

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	"github.com/tavon-ai/proteos/guestagent/internal/runas"
)

// gitInit creates a git repo at dir with one commit on branch main, an origin
// remote, and returns. It skips the test if git is unavailable.
func gitInit(t *testing.T, dir, remote string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Isolate from the developer's global git config (which may enable gpg
	// signing, hooks, etc.) by pointing HOME/XDG at a throwaway dir and forcing
	// signing off.
	gitHome := t.TempDir()
	run := func(args ...string) {
		full := append([]string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)
		cmd := exec.Command("git", full...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"HOME="+gitHome, "XDG_CONFIG_HOME="+gitHome, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if remote != "" {
		run("remote", "add", "origin", remote)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "initial commit")
}

func TestListProjects(t *testing.T) {
	work := t.TempDir()
	m := New([]string{"PROTEOS_WORKSPACE=" + work}, runas.Root(), nil, nil)

	gitInit(t, filepath.Join(work, "alpha"), "https://github.com/tavon/alpha.git")
	gitInit(t, filepath.Join(work, "beta"), "")
	// A plain directory under the workspace is NOT a project.
	if err := os.MkdirAll(filepath.Join(work, "notarepo"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := m.listProjects(context.Background())
	if err != nil {
		t.Fatalf("listProjects: %v", err)
	}
	byName := map[string]guestwire.Project{}
	for _, p := range got {
		byName[p.Name] = p
	}
	if len(byName) != 2 {
		t.Fatalf("want 2 projects, got %d: %+v", len(byName), got)
	}
	if _, ok := byName["notarepo"]; ok {
		t.Errorf("non-git dir should be excluded")
	}
	alpha, ok := byName["alpha"]
	if !ok {
		t.Fatalf("alpha missing")
	}
	if alpha.Path != filepath.Join(work, "alpha") {
		t.Errorf("alpha.Path = %q", alpha.Path)
	}
	if alpha.Remote != "https://github.com/tavon/alpha.git" {
		t.Errorf("alpha.Remote = %q", alpha.Remote)
	}
	if alpha.Branch != "main" {
		t.Errorf("alpha.Branch = %q, want main", alpha.Branch)
	}
	if alpha.Dirty {
		t.Errorf("alpha should be clean")
	}
	if alpha.LastCommitMsg != "initial commit" {
		t.Errorf("alpha.LastCommitMsg = %q", alpha.LastCommitMsg)
	}
	if alpha.LastCommitAt == "" {
		t.Errorf("alpha.LastCommitAt empty")
	}

	// Dirty detection: write an uncommitted change.
	if err := os.WriteFile(filepath.Join(work, "alpha", "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ = m.listProjects(context.Background())
	for _, p := range got {
		if p.Name == "alpha" && !p.Dirty {
			t.Errorf("alpha should be dirty after an uncommitted change")
		}
	}

	// Deleting a repo removes it from the list (filesystem is source of truth).
	if err := os.RemoveAll(filepath.Join(work, "beta")); err != nil {
		t.Fatal(err)
	}
	got, _ = m.listProjects(context.Background())
	for _, p := range got {
		if p.Name == "beta" {
			t.Errorf("deleted repo still listed")
		}
	}
}

func TestListProjects_NoWorkspace(t *testing.T) {
	m := New([]string{"PROTEOS_WORKSPACE=" + filepath.Join(t.TempDir(), "absent")}, runas.Root(), nil, nil)
	got, err := m.listProjects(context.Background())
	if err != nil {
		t.Fatalf("listProjects on absent workspace: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %+v", got)
	}
}

// fakeKV is an in-memory KV for the kv.* handler tests.
type fakeKV struct{ m map[string]string }

func (f *fakeKV) Get(key string) (string, bool) { v, ok := f.m[key]; return v, ok }
func (f *fakeKV) Set(key, value string) error   { f.m[key] = value; return nil }

func TestKVHandlers(t *testing.T) {
	kv := &fakeKV{m: map[string]string{}}
	m := New(nil, runas.Root(), kv, nil)
	ctx := context.Background()

	// kv.get on an absent key returns a null value.
	raw, cerr := m.handle(ctx, guestwire.OpKVGet, mustJSON(guestwire.KVGetPayload{Key: guestwire.KeyDesktopLayout}))
	if cerr != nil {
		t.Fatalf("kv.get: %v", cerr)
	}
	var get guestwire.KVGetResponse
	if err := json.Unmarshal(raw, &get); err != nil {
		t.Fatal(err)
	}
	if get.Value != nil {
		t.Errorf("absent key should be null, got %q", *get.Value)
	}

	// kv.set then kv.get round-trips the value.
	layout := `{"windows":[{"id":"w1"}]}`
	if _, cerr := m.handle(ctx, guestwire.OpKVSet, mustJSON(guestwire.KVSetPayload{Key: guestwire.KeyDesktopLayout, Value: layout})); cerr != nil {
		t.Fatalf("kv.set: %v", cerr)
	}
	raw, cerr = m.handle(ctx, guestwire.OpKVGet, mustJSON(guestwire.KVGetPayload{Key: guestwire.KeyDesktopLayout}))
	if cerr != nil {
		t.Fatalf("kv.get(2): %v", cerr)
	}
	_ = json.Unmarshal(raw, &get)
	if get.Value == nil || *get.Value != layout {
		t.Errorf("round-trip = %v, want %q", get.Value, layout)
	}
}

func TestKVHandlers_NilKVIsNoOp(t *testing.T) {
	m := New(nil, runas.Root(), nil, nil)
	ctx := context.Background()
	// kv.set on a diskless guest acks (no error) but does not persist.
	raw, cerr := m.handle(ctx, guestwire.OpKVSet, mustJSON(guestwire.KVSetPayload{Key: "k", Value: "v"}))
	if cerr != nil {
		t.Fatalf("kv.set nil kv: %v", cerr)
	}
	var set guestwire.KVSetResponse
	_ = json.Unmarshal(raw, &set)
	if !set.OK {
		t.Errorf("kv.set should ack ok even with nil kv")
	}
	// kv.get returns null.
	raw, _ = m.handle(ctx, guestwire.OpKVGet, mustJSON(guestwire.KVGetPayload{Key: "k"}))
	var get guestwire.KVGetResponse
	_ = json.Unmarshal(raw, &get)
	if get.Value != nil {
		t.Errorf("nil kv get should be null")
	}
}
