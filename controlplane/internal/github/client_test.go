package github_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/tavon-ai/proteos/controlplane/internal/github"
)

func TestAuthorizeURL(t *testing.T) {
	c := github.NewClient(github.Config{ClientID: "cid", AuthorizeURL: "https://gh/authorize"})
	got := c.AuthorizeURL("st4te", "https://app/cb")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("client_id") != "cid" || q.Get("state") != "st4te" || q.Get("redirect_uri") != "https://app/cb" {
		t.Fatalf("bad authorize url: %s", got)
	}
}

func TestExchangeAndGetUser(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("want JSON Accept header")
		}
		_, _ = w.Write([]byte(`{"access_token":"tok","refresh_token":"ref","token_type":"bearer","scope":"repo"}`))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("want bearer token, got %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"id":7,"login":"u","email":"e","avatar_url":"a"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := github.NewClient(github.Config{
		ClientID:     "cid",
		ClientSecret: "sec",
		TokenURL:     srv.URL + "/login/oauth/access_token",
		APIBaseURL:   srv.URL,
	})

	tok, err := c.Exchange(context.Background(), "code", "https://app/cb")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "tok" || tok.RefreshToken != "ref" {
		t.Fatalf("bad token: %+v", tok)
	}

	u, err := c.GetUser(context.Background(), tok.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != 7 || u.Login != "u" {
		t.Fatalf("bad user: %+v", u)
	}
}

func TestExchangeErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`))
	}))
	defer srv.Close()
	c := github.NewClient(github.Config{TokenURL: srv.URL})
	if _, err := c.Exchange(context.Background(), "code", "cb"); err == nil {
		t.Fatal("expected error on GitHub error body")
	}
}
