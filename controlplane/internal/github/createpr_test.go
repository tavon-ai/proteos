package github_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tavon-ai/proteos/controlplane/internal/github"
)

func TestCreatePR(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/octocat/hello/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":7,"html_url":"https://github.com/octocat/hello/pull/7"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := github.NewClient(github.Config{APIBaseURL: srv.URL})
	pr, err := c.CreatePR(context.Background(), "tok", "octocat", "hello", "feature/x", "main", "Title", "Body")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if pr.Number != 7 || pr.HTMLURL != "https://github.com/octocat/hello/pull/7" {
		t.Fatalf("unexpected pr: %+v", pr)
	}
}

func TestCreatePR_NoCommits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"No commits between main and feature/x"}]}`))
	}))
	t.Cleanup(srv.Close)

	c := github.NewClient(github.Config{APIBaseURL: srv.URL})
	_, err := c.CreatePR(context.Background(), "tok", "octocat", "hello", "feature/x", "main", "T", "B")
	if !errors.Is(err, github.ErrNoPRCommits) {
		t.Fatalf("err = %v, want ErrNoPRCommits", err)
	}
}

func TestCreatePR_AlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"A pull request already exists for octocat:feature/x."}]}`))
	}))
	t.Cleanup(srv.Close)

	c := github.NewClient(github.Config{APIBaseURL: srv.URL})
	_, err := c.CreatePR(context.Background(), "tok", "octocat", "hello", "feature/x", "main", "T", "B")
	if !errors.Is(err, github.ErrPRAlreadyExists) {
		t.Fatalf("err = %v, want ErrPRAlreadyExists", err)
	}
}

func TestGetRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"full_name":"octocat/hello","default_branch":"trunk"}`))
	}))
	t.Cleanup(srv.Close)

	c := github.NewClient(github.Config{APIBaseURL: srv.URL})
	r, err := c.GetRepo(context.Background(), "tok", "octocat", "hello")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if r.DefaultBranch != "trunk" {
		t.Fatalf("default branch = %q, want trunk", r.DefaultBranch)
	}
}
