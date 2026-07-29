package auth_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// An operator has to be able to tell "the IdP refused this token" from "this
// build predates bearer auth". Both are a flat 401 to the caller, so the log is
// the only thing that can say which.
func TestRejectedBearerIsExplainedInTheLog(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	idp := newFakeIdP(t)
	idp.liveAccessToken = "zat_access"
	h := newHarness(t, idp, newFakeGitHub(t), nil)

	resp := meWithBearer(t, h, "a-token-the-idp-will-refuse")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := buf.String(); !strings.Contains(got, "idp bearer rejected") {
		t.Errorf("nothing in the log explains the refusal:\n%s", got)
	}
}

func TestAcceptedBearerIsVisibleAtDebug(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	idp := newFakeIdP(t)
	idp.liveAccessToken = "zat_access"
	h := newHarness(t, idp, newFakeGitHub(t), nil)

	resp := meWithBearer(t, h, "zat_access")
	resp.Body.Close()
	if got := buf.String(); !strings.Contains(got, "idp bearer accepted") {
		t.Errorf("an accepted bearer left no trace:\n%s", got)
	}
}
