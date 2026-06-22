package server

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	guestwire "github.com/tavon/proteos/guestagent/api"
)

// TestDownloadStreamsZip verifies the guest zips a project directory under the
// workspace, names entries under the project's base directory, preserves file
// contents and empty directories, and skips the workspace root entry itself.
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
