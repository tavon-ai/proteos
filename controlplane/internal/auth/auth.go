// Package auth implements the GitHub App user-authorization (OAuth) login flow:
// login redirect, callback (code→tokens→user→session), and logout. GitHub
// tokens are written to the secrets store; only a secret_ref lands in Postgres.
package auth

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/github"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// Cookie names owned by the auth flow.
const (
	// SessionCookieName carries the opaque session token. Exported so the HTTP
	// middleware can read it.
	SessionCookieName = "proteos_session"
	// stateCookieName carries the signed OAuth state during the round-trip.
	stateCookieName = "proteos_oauth_state"
)

// Config holds the settings the auth handler needs.
type Config struct {
	BaseURL             string        // e.g. http://localhost:8080
	StateKey            []byte        // HMAC key for the state token
	CookieSecure        bool          // Secure attribute on cookies
	SessionTTL          time.Duration // session cookie lifetime
	AllowedGitHubLogins []string      // signup allowlist; empty = allow all
}

// Handler serves the auth endpoints.
type Handler struct {
	cfg      Config
	gh       *github.Client
	sessions *session.Manager
	store    *store.Queries
	secrets  secrets.Store
	audit    *audit.Recorder
}

// NewHandler wires the auth handler dependencies. aud may be nil (audit disabled).
func NewHandler(cfg Config, gh *github.Client, sessions *session.Manager, q *store.Queries, sec secrets.Store, aud *audit.Recorder) *Handler {
	return &Handler{cfg: cfg, gh: gh, sessions: sessions, store: q, secrets: sec, audit: aud}
}

func (h *Handler) callbackURL() string {
	return h.cfg.BaseURL + "/api/auth/github/callback"
}

// SetBaseURL overrides the configured base URL. Used in tests where the server
// address is only known after the httptest server starts.
func (h *Handler) SetBaseURL(u string) {
	h.cfg.BaseURL = u
}

// Login generates a signed state, stores it in a short-lived cookie, and
// redirects the browser to GitHub's authorize page.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	state, err := newState(h.cfg.StateKey)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "state_error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/api/auth/github",
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(stateTTL.Seconds()),
	})
	http.Redirect(w, r, h.gh.AuthorizeURL(state, h.callbackURL()), http.StatusFound)
}

// Callback validates state, exchanges the code, fetches the user, upserts the
// user + github_link, writes tokens to the secrets store, creates a session,
// and redirects to the dashboard.
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if ghErr := q.Get("error"); ghErr != "" {
		slog.Warn("github oauth error", "error", ghErr)
		h.redirectError(w, r, "github_error")
		return
	}

	// Validate state against the cookie (constant-time inside validateState).
	cookie, err := r.Cookie(stateCookieName)
	if err != nil || cookie.Value == "" {
		h.redirectError(w, r, "missing_state")
		return
	}
	clearStateCookie(w, h.cfg.CookieSecure)
	state := q.Get("state")
	if state == "" || cookie.Value != state {
		h.redirectError(w, r, "bad_state")
		return
	}
	if err := validateState(h.cfg.StateKey, state); err != nil {
		h.redirectError(w, r, "bad_state")
		return
	}

	code := q.Get("code")
	if code == "" {
		h.redirectError(w, r, "missing_code")
		return
	}

	tok, err := h.gh.Exchange(r.Context(), code, h.callbackURL())
	if err != nil {
		slog.Warn("token exchange failed", "err", err)
		h.redirectError(w, r, "exchange_failed")
		return
	}

	ghUser, err := h.gh.GetUser(r.Context(), tok.AccessToken)
	if err != nil {
		slog.Warn("fetch github user failed", "err", err)
		h.redirectError(w, r, "user_fetch_failed")
		return
	}

	// Signup allowlist (cross-cutting requirement before internet exposure).
	if len(h.cfg.AllowedGitHubLogins) > 0 && !slices.Contains(h.cfg.AllowedGitHubLogins, ghUser.Login) {
		slog.Info("login rejected by allowlist", "login", ghUser.Login)
		h.redirectError(w, r, "not_invited")
		return
	}

	user, err := h.store.UpsertUser(r.Context(), store.UpsertUserParams{
		GithubUserID: ghUser.ID,
		Login:        ghUser.Login,
		Email:        ghUser.Email,
		AvatarUrl:    ghUser.AvatarURL,
	})
	if err != nil {
		slog.Error("upsert user failed", "err", err)
		h.redirectError(w, r, "internal")
		return
	}

	// Write tokens to the secrets store — NEVER to Postgres. The absolute expiry
	// timestamps are stored alongside the tokens so the Phase 7 TokenSource knows
	// when to refresh (the relative expires_in is only meaningful at issue time).
	now := time.Now()
	userID := uuidString(user.ID)
	secretRef := secrets.UserGitHubPath(userID)
	if err := h.secrets.Put(secretRef, map[string]string{
		"access_token":             tok.AccessToken,
		"refresh_token":            tok.RefreshToken,
		"token_type":               tok.TokenType,
		"scope":                    tok.Scope,
		"access_token_expires_at":  now.Add(time.Duration(tok.ExpiresIn) * time.Second).UTC().Format(time.RFC3339),
		"refresh_token_expires_at": now.Add(time.Duration(tok.RefreshTokenExpiresIn) * time.Second).UTC().Format(time.RFC3339),
	}); err != nil {
		slog.Error("store github tokens failed", "err", err)
		h.redirectError(w, r, "internal")
		return
	}

	// Persist only the secret_ref + non-sensitive metadata. Re-login clears any
	// prior revoked flag (decision #4): a fresh grant re-enables git operations.
	meta, _ := json.Marshal(map[string]any{
		"expires_in":               tok.ExpiresIn,
		"refresh_token_expires_in": tok.RefreshTokenExpiresIn,
		"scope":                    tok.Scope,
		"revoked":                  false,
	})
	if _, err := h.store.UpsertGitHubLink(r.Context(), store.UpsertGitHubLinkParams{
		UserID:    user.ID,
		Metadata:  meta,
		SecretRef: secretRef,
	}); err != nil {
		slog.Error("upsert github link failed", "err", err)
		h.redirectError(w, r, "internal")
		return
	}

	token, err := h.sessions.Create(r.Context(), user.ID)
	if err != nil {
		slog.Error("create session failed", "err", err)
		h.redirectError(w, r, "internal")
		return
	}
	uid := uuidString(user.ID)
	h.audit.Record(r.Context(), audit.Entry{
		UserID:   uid,
		Actor:    audit.UserActor(uid),
		Action:   audit.ActionAuthLogin,
		Target:   uid,
		Metadata: map[string]any{"github_login": ghUser.Login},
	})
	http.SetCookie(w, h.sessionCookie(token))
	http.Redirect(w, r, "/", http.StatusFound)
}

// Logout revokes the current session and clears the cookie.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		if err := h.sessions.Revoke(r.Context(), c.Value); err != nil {
			slog.Warn("revoke session failed", "err", err)
		}
	}
	http.SetCookie(w, h.clearSessionCookie())
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) sessionCookie(token string) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(h.cfg.SessionTTL),
	}
}

func (h *Handler) clearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// redirectError sends the browser back to the SPA login with an error code in
// the query string so the UI can show a message.
func (h *Handler) redirectError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/login?error="+code, http.StatusFound)
}

func clearStateCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/api/auth/github",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAuthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

// uuidString renders a pgtype.UUID as the canonical 36-char string.
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	const hexdig = "0123456789abcdef"
	buf := make([]byte, 36)
	pos := 0
	for i := range 16 {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf[pos] = '-'
			pos++
		}
		buf[pos] = hexdig[b[i]>>4]
		buf[pos+1] = hexdig[b[i]&0x0f]
		pos += 2
	}
	return string(buf)
}
