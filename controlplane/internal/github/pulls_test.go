package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tavon-ai/proteos/controlplane/internal/github"
)

func TestGetPR(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/octocat/hello/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"number":7,"state":"open","merged":false,"draft":true,
			"title":"T","body":"B","html_url":"https://github.com/octocat/hello/pull/7",
			"head":{"ref":"feature/x","sha":"abc123"},"base":{"ref":"main"},
			"user":{"login":"octocat","avatar_url":"https://a.example/1"},
			"additions":128,"deletions":34,"changed_files":5}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := github.NewClient(github.Config{APIBaseURL: srv.URL})
	pr, err := c.GetPR(context.Background(), "tok", "octocat", "hello", 7)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.Number != 7 || !pr.Draft || pr.HeadRef != "feature/x" || pr.HeadSHA != "abc123" ||
		pr.BaseRef != "main" || pr.AuthorLogin != "octocat" || pr.Additions != 128 ||
		pr.Deletions != 34 || pr.ChangedFiles != 5 {
		t.Fatalf("unexpected pr: %+v", pr)
	}
}

func TestGetPR_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := github.NewClient(github.Config{APIBaseURL: srv.URL})
	if _, err := c.GetPR(context.Background(), "tok", "octocat", "hello", 7); !errors.Is(err, github.ErrPRNotFound) {
		t.Fatalf("err = %v, want ErrPRNotFound", err)
	}
}

func TestListPRFiles_Paginates(t *testing.T) {
	// Page 1 returns a full page (100 rows) so the client must fetch page 2.
	var page1 strings.Builder
	page1.WriteString("[")
	for i := range 100 {
		if i > 0 {
			page1.WriteString(",")
		}
		fmt.Fprintf(&page1, `{"filename":"f%d.go","status":"modified","additions":1,"deletions":0}`, i)
	}
	page1.WriteString("]")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/octocat/hello/pulls/7/files", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(page1.String()))
			return
		}
		_, _ = w.Write([]byte(`[{"filename":"last.go","previous_filename":"old.go","status":"renamed","additions":2,"deletions":3,"patch":"@@ -1 +1 @@"}]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := github.NewClient(github.Config{APIBaseURL: srv.URL})
	files, err := c.ListPRFiles(context.Background(), "tok", "octocat", "hello", 7)
	if err != nil {
		t.Fatalf("ListPRFiles: %v", err)
	}
	if len(files) != 101 {
		t.Fatalf("len = %d, want 101", len(files))
	}
	last := files[100]
	if last.Filename != "last.go" || last.PreviousFilename != "old.go" || last.Status != "renamed" ||
		last.Additions != 2 || last.Deletions != 3 || last.Patch != "@@ -1 +1 @@" {
		t.Fatalf("unexpected last file: %+v", last)
	}
}

func TestListCheckRuns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[
			{"name":"build","status":"completed","conclusion":"success"},
			{"name":"lint","status":"in_progress","conclusion":""}]}`))
	}))
	t.Cleanup(srv.Close)

	c := github.NewClient(github.Config{APIBaseURL: srv.URL})
	runs, err := c.ListCheckRuns(context.Background(), "tok", "octocat", "hello", "abc123")
	if err != nil {
		t.Fatalf("ListCheckRuns: %v", err)
	}
	if len(runs) != 2 || runs[0].Conclusion != "success" || runs[1].Status != "in_progress" {
		t.Fatalf("unexpected runs: %+v", runs)
	}
}

func TestMergePR(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /repos/octocat/hello/pulls/7/merge", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sha":"deadbeef","merged":true,"message":"Pull Request successfully merged"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := github.NewClient(github.Config{APIBaseURL: srv.URL})
	m, err := c.MergePR(context.Background(), "tok", "octocat", "hello", 7, "merge")
	if err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	if m.SHA != "deadbeef" || !m.Merged {
		t.Fatalf("unexpected merge: %+v", m)
	}
}

func TestMergePR_Refusals(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusMethodNotAllowed, github.ErrPRNotMergeable},
		{http.StatusConflict, github.ErrPRHeadChanged},
		{http.StatusForbidden, github.ErrPRMergeForbidden},
		{http.StatusNotFound, github.ErrPRNotFound},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"message":"nope"}`))
		}))
		c := github.NewClient(github.Config{APIBaseURL: srv.URL})
		_, err := c.MergePR(context.Background(), "tok", "octocat", "hello", 7, "merge")
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestCreateIssueComment(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/octocat/hello/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":42,"html_url":"https://github.com/octocat/hello/pull/7#issuecomment-42"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := github.NewClient(github.Config{APIBaseURL: srv.URL})
	cm, err := c.CreateIssueComment(context.Background(), "tok", "octocat", "hello", 7, "LGTM")
	if err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	if cm.ID != 42 || cm.HTMLURL == "" {
		t.Fatalf("unexpected comment: %+v", cm)
	}
}
