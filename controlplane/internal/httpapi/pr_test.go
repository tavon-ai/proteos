package httpapi_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/github"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

// fakePRReviewServer serves the GitHub endpoints the PR review handlers touch,
// with caller-overridable handlers per route.
func fakePRReviewServer(t *testing.T, overrides map[string]http.HandlerFunc) string {
	t.Helper()
	handlers := map[string]http.HandlerFunc{}
	handlers["GET /repos/octocat/hello/pulls/7"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"number":7,"state":"open","merged":false,"draft":false,
			"title":"Node-agent reliability hardening","body":"","html_url":"https://github.com/octocat/hello/pull/7",
			"head":{"ref":"feat/x","sha":"abc123"},"base":{"ref":"main"},
			"user":{"login":"octocat","avatar_url":"https://a.example/1"},
			"additions":128,"deletions":34,"changed_files":2}`))
	}
	handlers["GET /repos/octocat/hello/pulls/7/files"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"filename":"a.go","status":"modified","additions":61,"deletions":12,"patch":"@@ -1 +1 @@"},
			{"filename":"b.go","previous_filename":"c.go","status":"renamed","additions":0,"deletions":0}]`))
	}
	handlers["GET /repos/octocat/hello/commits/abc123/check-runs"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":3,"check_runs":[
			{"name":"build","status":"completed","conclusion":"success"},
			{"name":"test","status":"completed","conclusion":"failure"},
			{"name":"lint","status":"in_progress","conclusion":""}]}`))
	}
	handlers["PUT /repos/octocat/hello/pulls/7/merge"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sha":"deadbeef","merged":true}`))
	}
	handlers["POST /repos/octocat/hello/issues/7/comments"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":42,"html_url":"https://github.com/octocat/hello/pull/7#issuecomment-42"}`))
	}
	maps.Copy(handlers, overrides)
	mux := http.NewServeMux()
	for pattern, h := range handlers {
		mux.HandleFunc(pattern, h)
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

// setupPRReview wires a server with just the PR review surface: sessions, a
// seeded (optionally revoked) GitHub token, and a real GitHub client pointed
// at ghURL. No machine or worktree — the endpoints must not need one.
func setupPRReview(t *testing.T, revoked bool, ghURL string) wtFixture {
	t.Helper()
	ctx := context.Background()
	_, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 9, Login: "octocat", Email: "o@example.com"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	uid := machine.UUIDString(user.ID)

	sec := secrets.NewMemStore()
	if err := sec.Put(secrets.UserGitHubPath(uid), map[string]string{
		"access_token":            "gho_valid",
		"refresh_token":           "ghr_valid",
		"access_token_expires_at": time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	meta, _ := json.Marshal(map[string]any{"revoked": revoked})
	if _, err := q.UpsertGitHubLink(ctx, store.UpsertGitHubLinkParams{UserID: user.ID, Metadata: meta, SecretRef: secrets.UserGitHubPath(uid)}); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	gh := github.NewClient(github.Config{ClientID: "id", ClientSecret: "s", APIBaseURL: ghURL})
	srv := &httpapi.Server{
		Sessions: sessions,
		Queries:  q,
		Audit:    audit.NewRecorder(q),
		GitHub:   gh,
		Tokens:   github.NewTokenSource(gh, q, sec),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return wtFixture{url: ts.URL, token: token}
}

func TestPRDetail_200(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, false, fakePRReviewServer(t, nil))
	resp := fx.get(t, "/api/git/repos/octocat/hello/pulls/7")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Number  int    `json:"number"`
		State   string `json:"state"`
		Title   string `json:"title"`
		Head    string `json:"head"`
		Base    string `json:"base"`
		HeadSHA string `json:"head_sha"`
		Author  struct {
			Login string `json:"login"`
		} `json:"author"`
		Additions    int `json:"additions"`
		Deletions    int `json:"deletions"`
		ChangedFiles int `json:"changed_files"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Number != 7 || body.State != "open" || body.Head != "feat/x" || body.HeadSHA != "abc123" ||
		body.Author.Login != "octocat" || body.Additions != 128 || body.Deletions != 34 || body.ChangedFiles != 2 {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestPRDetail_MergedState(t *testing.T) {
	t.Parallel()
	gh := fakePRReviewServer(t, map[string]http.HandlerFunc{
		"GET /repos/octocat/hello/pulls/7": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"number":7,"state":"closed","merged":true,"title":"T",
				"html_url":"u","head":{"ref":"feat/x","sha":"abc123"},"base":{"ref":"main"},"user":{"login":"o"}}`))
		},
	})
	fx := setupPRReview(t, false, gh)
	resp := fx.get(t, "/api/git/repos/octocat/hello/pulls/7")
	defer resp.Body.Close()
	var body struct {
		State string `json:"state"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.State != "merged" {
		t.Fatalf("state = %q, want merged", body.State)
	}
}

func TestPRDetail_404(t *testing.T) {
	t.Parallel()
	gh := fakePRReviewServer(t, map[string]http.HandlerFunc{
		"GET /repos/octocat/hello/pulls/7": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	fx := setupPRReview(t, false, gh)
	resp := fx.get(t, "/api/git/repos/octocat/hello/pulls/7")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "no_pr" {
		t.Fatalf("error = %q, want no_pr", code)
	}
}

func TestPRDetail_409Reconnect(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, true, fakePRReviewServer(t, nil)) // revoked grant
	resp := fx.get(t, "/api/git/repos/octocat/hello/pulls/7")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "reconnect_github" {
		t.Fatalf("error = %q, want reconnect_github", code)
	}
}

func TestPRDetail_400BadNumber(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, false, fakePRReviewServer(t, nil))
	resp := fx.get(t, "/api/git/repos/octocat/hello/pulls/nope")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPRFiles_200(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, false, fakePRReviewServer(t, nil))
	resp := fx.get(t, "/api/git/repos/octocat/hello/pulls/7/files")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Files []struct {
			Path      string `json:"path"`
			PrevPath  string `json:"prev_path"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
			Patch     string `json:"patch"`
		} `json:"files"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(body.Files))
	}
	if f := body.Files[0]; f.Path != "a.go" || f.Status != "M" || f.Additions != 61 || f.Deletions != 12 || f.Patch == "" {
		t.Fatalf("unexpected file 0: %+v", f)
	}
	if f := body.Files[1]; f.Status != "R" || f.PrevPath != "c.go" {
		t.Fatalf("unexpected file 1: %+v", f)
	}
}

func TestPRChecks_200(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, false, fakePRReviewServer(t, nil))
	resp := fx.get(t, "/api/git/repos/octocat/hello/pulls/7/checks")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Total   int `json:"total"`
		Passed  int `json:"passed"`
		Failed  int `json:"failed"`
		Pending int `json:"pending"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Total != 3 || body.Passed != 1 || body.Failed != 1 || body.Pending != 1 {
		t.Fatalf("unexpected summary: %+v", body)
	}
}

func TestPRMerge_200(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, false, fakePRReviewServer(t, nil))
	resp := fx.post(t, "/api/git/repos/octocat/hello/pulls/7/merge", `{"method":"squash"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Merged bool   `json:"merged"`
		SHA    string `json:"sha"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if !body.Merged || body.SHA != "deadbeef" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestPRMerge_EmptyBodyDefaults(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, false, fakePRReviewServer(t, nil))
	resp := fx.post(t, "/api/git/repos/octocat/hello/pulls/7/merge", ``, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestPRMerge_400BadMethod(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, false, fakePRReviewServer(t, nil))
	resp := fx.post(t, "/api/git/repos/octocat/hello/pulls/7/merge", `{"method":"fast-forward"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPRMerge_422NotMergeable(t *testing.T) {
	t.Parallel()
	gh := fakePRReviewServer(t, map[string]http.HandlerFunc{
		"PUT /repos/octocat/hello/pulls/7/merge": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"message":"Pull Request is not mergeable"}`))
		},
	})
	fx := setupPRReview(t, false, gh)
	resp := fx.post(t, "/api/git/repos/octocat/hello/pulls/7/merge", `{}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "not_mergeable" {
		t.Fatalf("error = %q, want not_mergeable", code)
	}
}

func TestPRMerge_409HeadChanged(t *testing.T) {
	t.Parallel()
	gh := fakePRReviewServer(t, map[string]http.HandlerFunc{
		"PUT /repos/octocat/hello/pulls/7/merge": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"Head branch was modified"}`))
		},
	})
	fx := setupPRReview(t, false, gh)
	resp := fx.post(t, "/api/git/repos/octocat/hello/pulls/7/merge", `{}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "head_changed" {
		t.Fatalf("error = %q, want head_changed", code)
	}
}

func TestPRMerge_RequiresCSRF(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, false, fakePRReviewServer(t, nil))
	resp := fx.post(t, "/api/git/repos/octocat/hello/pulls/7/merge", `{}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF)", resp.StatusCode)
	}
}

func TestPRComment_200(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, false, fakePRReviewServer(t, nil))
	resp := fx.post(t, "/api/git/repos/octocat/hello/pulls/7/comments", `{"body":"LGTM"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.ID != 42 || body.HTMLURL == "" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestPRComment_400Empty(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, false, fakePRReviewServer(t, nil))
	resp := fx.post(t, "/api/git/repos/octocat/hello/pulls/7/comments", `{"body":"  "}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "empty_comment" {
		t.Fatalf("error = %q, want empty_comment", code)
	}
}

func TestPRComment_RequiresCSRF(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, false, fakePRReviewServer(t, nil))
	resp := fx.post(t, "/api/git/repos/octocat/hello/pulls/7/comments", `{"body":"x"}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF)", resp.StatusCode)
	}
}

func TestPRDetail_401Unauthenticated(t *testing.T) {
	t.Parallel()
	fx := setupPRReview(t, false, fakePRReviewServer(t, nil))
	req, _ := http.NewRequest(http.MethodGet, fx.url+"/api/git/repos/octocat/hello/pulls/7", nil)
	resp, err := http.DefaultClient.Do(req) // no session cookie
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
