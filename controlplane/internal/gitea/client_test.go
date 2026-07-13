package gitea

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fake serves the three endpoints the client touches, gated on a known token.
func fake(t *testing.T, prStatus int, prBody string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	authed := func(r *http.Request) bool { return r.Header.Get("Authorization") == "token pat-ok" }
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"token is required"}`))
			return
		}
		_, _ = w.Write([]byte(`{"login":"ivan","id":1}`))
	})
	mux.HandleFunc("GET /repos/ivan/hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"full_name":"ivan/hello","private":false,"default_branch":"main"}`))
	})
	mux.HandleFunc("GET /repos/ivan/secret", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"full_name":"ivan/secret","private":true,"default_branch":"main"}`))
	})
	mux.HandleFunc("POST /repos/ivan/hello/pulls", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"token is required"}`))
			return
		}
		w.WriteHeader(prStatus)
		_, _ = w.Write([]byte(prBody))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return New(ts.URL)
}

func TestGetUser(t *testing.T) {
	c := fake(t, http.StatusCreated, `{}`)
	login, err := c.GetUser(context.Background(), "pat-ok")
	if err != nil || login != "ivan" {
		t.Fatalf("GetUser = (%q, %v), want (ivan, nil)", login, err)
	}
	if _, err := c.GetUser(context.Background(), "pat-bad"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("bad token: err = %v, want ErrBadToken", err)
	}
}

func TestGetRepo(t *testing.T) {
	c := fake(t, http.StatusCreated, `{}`)
	// Public repo resolves tokenless.
	r, err := c.GetRepo(context.Background(), "", "ivan", "hello")
	if err != nil || r.DefaultBranch != "main" {
		t.Fatalf("GetRepo = (%+v, %v)", r, err)
	}
	// Private repo: 404 without the token (Gitea hides existence), found with it.
	if _, err := c.GetRepo(context.Background(), "", "ivan", "secret"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("private tokenless: err = %v, want ErrNotFound", err)
	}
	if r, err := c.GetRepo(context.Background(), "pat-ok", "ivan", "secret"); err != nil || !r.Private {
		t.Fatalf("private with token = (%+v, %v)", r, err)
	}
}

func TestCreatePR(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		token   string
		wantErr error
		wantNum int
	}{
		{"created", http.StatusCreated, `{"number":7,"html_url":"https://g.example/ivan/hello/pulls/7"}`, "pat-ok", nil, 7},
		{"exists 409", http.StatusConflict, `{"message":"pull request already exists for these targets"}`, "pat-ok", ErrPRAlreadyExists, 0},
		{"no diff 409", http.StatusConflict, `{"message":"no commits between branches"}`, "pat-ok", ErrNoPRCommits, 0},
		{"validation 422", http.StatusUnprocessableEntity, `{"message":"head and base are identical"}`, "pat-ok", ErrNoPRCommits, 0},
		{"bad token", http.StatusCreated, `{}`, "pat-bad", ErrBadToken, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake(t, tc.status, tc.body)
			pr, err := c.CreatePR(context.Background(), tc.token, "ivan", "hello", "feature/x", "main", "T", "")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || pr.Number != tc.wantNum {
				t.Fatalf("CreatePR = (%+v, %v), want number %d", pr, err, tc.wantNum)
			}
		})
	}
}
