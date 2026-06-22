package server

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	guestwire "github.com/tavon/proteos/guestagent/api"
)

// TestDownloadStreamsZip verifies the guest zips a project directory under the
// workspace in "all" mode: every entry is named under the project's base
// directory, file contents and empty directories are preserved, and the
// workspace root entry itself is skipped.
func TestDownloadStreamsZip(t *testing.T) {
	ts, root := newCwdTestServer(t)

	proj := filepath.Join(root, "myproj")
	files := map[string]string{
		"README.md":        "hello world",
		"src/main.go":      "package main",
		"src/util/util.go": "package util",
	}
	for rel, body := range files {
		p := filepath.Join(proj, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// An empty directory should survive the round trip as a dir entry.
	if err := os.MkdirAll(filepath.Join(proj, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	q := url.Values{}
	q.Set(guestwire.QueryParamCwd, proj)
	q.Set(guestwire.QueryParamDownloadMode, guestwire.DownloadModeAll)
	resp, err := http.Get(ts.URL + "/download?" + q.Encode())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q, want application/zip", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="myproj.zip"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}

	got := map[string]string{}
	var dirs []string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			dirs = append(dirs, f.Name)
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(data)
	}

	// Every file is present, prefixed with the project's base directory, with
	// its content intact.
	for rel, want := range files {
		name := "myproj/" + rel
		if got[name] != want {
			t.Errorf("entry %q = %q, want %q", name, got[name], want)
		}
	}
	// The empty directory is recorded as a dir entry.
	foundEmpty := false
	for _, d := range dirs {
		if d == "myproj/empty/" {
			foundEmpty = true
		}
	}
	if !foundEmpty {
		t.Errorf("empty dir entry missing; dirs = %v", dirs)
	}
}

// TestDownloadCleanModeExcludesGitAndIgnored verifies the default (clean) mode
// archives the files git reports for the working tree — tracked plus
// untracked-but-not-ignored, so an uncommitted change made by an agent is kept —
// while dropping .git internals and anything matched by .gitignore.
func TestDownloadCleanModeExcludesGitAndIgnored(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ts, root := newCwdTestServer(t)
	proj := filepath.Join(root, "repo")

	write := func(rel, body string) {
		p := filepath.Join(proj, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tracked.txt", "committed")
	write(".gitignore", "ignored.txt\nbuild/\n")

	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", proj}, args...)...)
		// Isolate from the developer's global/system git config (identity, and
		// notably commit.gpgsign, which would fail in CI / the sandbox).
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "tracked.txt", ".gitignore")
	git("commit", "-q", "-m", "init")

	// After the commit: an untracked file (the "agent's uncommitted work"), an
	// ignored file, and an ignored build dir — none committed.
	write("uncommitted.txt", "agent work")
	write("ignored.txt", "secret")
	write("build/out.bin", "artifact")

	q := url.Values{}
	q.Set(guestwire.QueryParamCwd, proj)
	// No mode param ⇒ clean is the default.
	resp, err := http.Get(ts.URL + "/download?" + q.Encode())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}

	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	wantPresent := []string{"repo/tracked.txt", "repo/.gitignore", "repo/uncommitted.txt"}
	for _, n := range wantPresent {
		if !names[n] {
			t.Errorf("clean zip missing %q; entries = %v", n, names)
		}
	}
	wantAbsent := []string{"repo/ignored.txt", "repo/build/out.bin"}
	for _, n := range wantAbsent {
		if names[n] {
			t.Errorf("clean zip should not contain %q", n)
		}
	}
	for name := range names {
		if strings.HasPrefix(name, "repo/.git/") {
			t.Errorf("clean zip leaked a .git entry: %q", name)
		}
	}
}

// TestDownloadBadPathRejected verifies a path outside the workspace (or absent)
// is rejected with 400 — the same defence-in-depth the cwd check applies.
func TestDownloadBadPathRejected(t *testing.T) {
	ts, root := newCwdTestServer(t)
	cases := map[string]string{
		"outside workspace": "/etc",
		"nonexistent":       filepath.Join(root, "nope"),
		"absent":            "",
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			q := url.Values{}
			if p != "" {
				q.Set(guestwire.QueryParamCwd, p)
			}
			resp, err := http.Get(ts.URL + "/download?" + q.Encode())
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}
