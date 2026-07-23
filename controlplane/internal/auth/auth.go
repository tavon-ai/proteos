// Package auth implements login and account linking (TAV-149):
//
//   - Login is Zitadel OIDC (authorization code + PKCE): /api/auth/login
//     redirects to the IdP, /api/auth/callback resolves the identity, upserts
//     the user, and mints the opaque ProteOS session cookie. Sessions stay
//     fully server-side — the IdP is consulted only at login time.
//   - "Connect GitHub" is the old GitHub App user-authorization (OAuth) flow,
//     now run for an already-authenticated user: it links the GitHub account
//     and stores the GitHub tokens the git operations need. GitHub tokens are
//     written to the secrets store; only a secret_ref lands in Postgres.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/github"
	"github.com/tavon-ai/proteos/controlplane/internal/oidc"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// Cookie names owned by the auth flows.
const (
	// SessionCookieName carries the opaque session token. Exported so the HTTP
	// middleware can read it.
	SessionCookieName = "proteos_session"
	// oidcStateCookieName carries the signed OIDC state during the login round-trip.
	oidcStateCookieName = "proteos_oidc_state"
	// pkceCookieName carries the PKCE code verifier during the login round-trip.
	pkceCookieName = "proteos_pkce"
	// ghStateCookieName carries the signed OAuth state during the GitHub connect
	// round-trip (the pre-TAV-149 login state cookie, path unchanged).
	ghStateCookieName = "proteos_oauth_state"
)

// Config holds the settings the auth handler needs.
type Config struct {
	BaseURL             string        // e.g. http://localhost:8080
	StateKey            []byte        // HMAC key for the state token
	CookieSecure        bool          // Secure attribute on cookies
	SessionTTL          time.Duration // session cookie lifetime
	AllowedGitHubLogins []string      // GitHub-linking allowlist; empty = allow all
}

// Handler serves the auth endpoints.
type Handler struct {
	cfg      Config
	oidc     *oidc.Client
	gh       *github.Client
	sessions *session.Manager
	store    *store.Queries
	secrets  secrets.Store
	audit    *audit.Recorder
}

// NewHandler wires the auth handler dependencies. aud may be nil (audit disabled).
func NewHandler(cfg Config, oc *oidc.Client, gh *github.Client, sessions *session.Manager, q *store.Queries, sec secrets.Store, aud *audit.Recorder) *Handler {
	return &Handler{cfg: cfg, oidc: oc, gh: gh, sessions: sessions, store: q, secrets: sec, audit: aud}
}

func (h *Handler) oidcCallbackURL() string {
	return h.cfg.BaseURL + "/api/auth/callback"
}

func (h *Handler) githubCallbackURL() string {
	return h.cfg.BaseURL + "/api/auth/github/callback"
}

// SetBaseURL overrides the configured base URL. Used in tests where the server
// address is only known after the httptest server starts.
func (h *Handler) SetBaseURL(u string) {
	h.cfg.BaseURL = u
}

// Login generates a signed state and a PKCE verifier, stores both in
// short-lived cookies, and redirects the browser to the IdP's authorize page.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	state, err := newState(h.cfg.StateKey)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "state_error")
		return
	}
	verifier, err := oidc.NewVerifier()
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "state_error")
		return
	}
	authorizeURL, err := h.oidc.AuthorizeURL(r.Context(), state, h.oidcCallbackURL(), verifier)
	if err != nil {
		slog.Warn("oidc authorize url failed", "err", err)
		h.redirectLoginError(w, r, "idp_unreachable")
		return
	}
	h.setFlowCookie(w, oidcStateCookieName, state, "/api/auth")
	h.setFlowCookie(w, pkceCookieName, verifier, "/api/auth")
	http.Redirect(w, r, authorizeURL, http.StatusFound)
}

// Callback validates state, exchanges the code (PKCE), fetches the userinfo,
// resolves the ProteOS user (by OIDC identity, verified-email linking, or
// first-login creation), creates a session, and redirects to the dashboard.
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if idpErr := q.Get("error"); idpErr != "" {
		slog.Warn("oidc error", "error", idpErr)
		h.redirectLoginError(w, r, "idp_error")
		return
	}

	// Validate state against the cookie (constant-time inside validateState).
	stateCookie, err := r.Cookie(oidcStateCookieName)
	if err != nil || stateCookie.Value == "" {
		h.redirectLoginError(w, r, "missing_state")
		return
	}
	verifierCookie, err := r.Cookie(pkceCookieName)
	if err != nil || verifierCookie.Value == "" {
		h.redirectLoginError(w, r, "missing_state")
		return
	}
	h.clearFlowCookie(w, oidcStateCookieName, "/api/auth")
	h.clearFlowCookie(w, pkceCookieName, "/api/auth")
	state := q.Get("state")
	if state == "" || stateCookie.Value != state {
		h.redirectLoginError(w, r, "bad_state")
		return
	}
	if err := validateState(h.cfg.StateKey, state); err != nil {
		h.redirectLoginError(w, r, "bad_state")
		return
	}

	code := q.Get("code")
	if code == "" {
		h.redirectLoginError(w, r, "missing_code")
		return
	}

	tok, err := h.oidc.Exchange(r.Context(), code, h.oidcCallbackURL(), verifierCookie.Value)
	if err != nil {
		slog.Warn("oidc token exchange failed", "err", err)
		h.redirectLoginError(w, r, "exchange_failed")
		return
	}
	info, err := h.oidc.GetUserInfo(r.Context(), tok.AccessToken)
	if err != nil {
		slog.Warn("oidc userinfo failed", "err", err)
		h.redirectLoginError(w, r, "user_fetch_failed")
		return
	}

	user, errCode := h.resolveOIDCUser(r.Context(), info)
	if errCode != "" {
		h.redirectLoginError(w, r, errCode)
		return
	}

	token, err := h.sessions.Create(r.Context(), user.ID)
	if err != nil {
		slog.Error("create session failed", "err", err)
		h.redirectLoginError(w, r, "internal")
		return
	}
	uid := uuidString(user.ID)
	h.audit.Record(r.Context(), audit.Entry{
		UserID:   uid,
		Actor:    audit.UserActor(uid),
		Action:   audit.ActionAuthLogin,
		Target:   uid,
		Metadata: map[string]any{"oidc_subject": info.Subject},
	})
	http.SetCookie(w, h.sessionCookie(token))
	http.Redirect(w, r, "/", http.StatusFound)
}

// resolveOIDCUser maps an IdP identity to a users row:
//  1. an existing row with this (issuer, subject) — profile refreshed;
//  2. else a pre-Zitadel row linked by verified email, only when exactly one
//     unlinked row matches (ambiguity is an error, never a guess — a wrong
//     link hands over the target's machines and secrets);
//  3. else a fresh row (github_user_id stays NULL until Connect GitHub).
//
// A non-empty errCode is a /login?error= code and means no user was resolved.
func (h *Handler) resolveOIDCUser(ctx context.Context, info *oidc.UserInfo) (store.User, string) {
	issuer, err := h.oidc.Issuer(ctx)
	if err != nil {
		slog.Error("oidc issuer lookup failed", "err", err)
		return store.User{}, "internal"
	}

	user, err := h.store.GetUserByOIDC(ctx, store.GetUserByOIDCParams{
		OidcIssuer:  &issuer,
		OidcSubject: &info.Subject,
	})
	switch {
	case err == nil:
		updated, uerr := h.store.UpdateOIDCUserProfile(ctx, store.UpdateOIDCUserProfileParams{
			OidcIssuer:  &issuer,
			OidcSubject: &info.Subject,
			Login:       info.Login(),
			Email:       info.Email,
			AvatarUrl:   info.Picture,
		})
		if uerr != nil {
			slog.Warn("refresh user profile failed", "err", uerr)
			return user, "" // stale profile is not worth failing login over
		}
		return updated, ""
	case !errors.Is(err, pgx.ErrNoRows):
		slog.Error("lookup user by oidc failed", "err", err)
		return store.User{}, "internal"
	}

	// First OIDC login. Link a pre-Zitadel account only on a verified email
	// with exactly one candidate.
	if info.EmailVerified && info.Email != "" {
		candidates, err := h.store.ListLinkableUsersByEmail(ctx, info.Email)
		if err != nil {
			slog.Error("list linkable users failed", "err", err)
			return store.User{}, "internal"
		}
		if len(candidates) > 1 {
			slog.Warn("oidc link ambiguous", "email_candidates", len(candidates))
			return store.User{}, "link_ambiguous"
		}
		if len(candidates) == 1 {
			linked, err := h.store.LinkUserOIDC(ctx, store.LinkUserOIDCParams{
				ID:          candidates[0].ID,
				OidcIssuer:  &issuer,
				OidcSubject: &info.Subject,
			})
			if err != nil {
				slog.Error("link user oidc failed", "err", err)
				return store.User{}, "internal"
			}
			return linked, ""
		}
	}

	created, err := h.store.CreateOIDCUser(ctx, store.CreateOIDCUserParams{
		OidcIssuer:  &issuer,
		OidcSubject: &info.Subject,
		Login:       info.Login(),
		Email:       info.Email,
		AvatarUrl:   info.Picture,
	})
	if err != nil {
		slog.Error("create oidc user failed", "err", err)
		return store.User{}, "internal"
	}
	return created, ""
}

// GitHubConnect starts the GitHub App OAuth flow for an already-authenticated
// user (the httpapi layer enforces auth before calling this): signed state in a
// short-lived cookie, then redirect to GitHub's authorize page.
func (h *Handler) GitHubConnect(w http.ResponseWriter, r *http.Request) {
	state, err := newState(h.cfg.StateKey)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "state_error")
		return
	}
	h.setFlowCookie(w, ghStateCookieName, state, "/api/auth/github")
	http.Redirect(w, r, h.gh.AuthorizeURL(state, h.githubCallbackURL()), http.StatusFound)
}

// GitHubCallback completes Connect GitHub for user: validates state, exchanges
// the code, fetches the GitHub profile, records the link on the user row,
// writes the tokens to the secrets store, and returns to the dashboard. Errors
// land on /?github_error= so the connect screen (not the login page) shows them.
func (h *Handler) GitHubCallback(w http.ResponseWriter, r *http.Request, user store.User) {
	q := r.URL.Query()
	if ghErr := q.Get("error"); ghErr != "" {
		slog.Warn("github oauth error", "error", ghErr)
		h.redirectConnectError(w, r, "github_error")
		return
	}

	cookie, err := r.Cookie(ghStateCookieName)
	if err != nil || cookie.Value == "" {
		h.redirectConnectError(w, r, "missing_state")
		return
	}
	h.clearFlowCookie(w, ghStateCookieName, "/api/auth/github")
	state := q.Get("state")
	if state == "" || cookie.Value != state {
		h.redirectConnectError(w, r, "bad_state")
		return
	}
	if err := validateState(h.cfg.StateKey, state); err != nil {
		h.redirectConnectError(w, r, "bad_state")
		return
	}

	code := q.Get("code")
	if code == "" {
		h.redirectConnectError(w, r, "missing_code")
		return
	}

	tok, err := h.gh.Exchange(r.Context(), code, h.githubCallbackURL())
	if err != nil {
		slog.Warn("github token exchange failed", "err", err)
		h.redirectConnectError(w, r, "exchange_failed")
		return
	}

	ghUser, err := h.gh.GetUser(r.Context(), tok.AccessToken)
	if err != nil {
		slog.Warn("fetch github user failed", "err", err)
		h.redirectConnectError(w, r, "user_fetch_failed")
		return
	}

	// Linking allowlist (the pre-TAV-149 signup allowlist, kept as a belt-and-
	// braces bound on which GitHub accounts may back git operations; Zitadel
	// now gates signup itself).
	if len(h.cfg.AllowedGitHubLogins) > 0 && !slices.Contains(h.cfg.AllowedGitHubLogins, ghUser.Login) {
		slog.Info("github connect rejected by allowlist", "login", ghUser.Login)
		h.redirectConnectError(w, r, "not_invited")
		return
	}

	if _, err := h.store.SetUserGitHub(r.Context(), store.SetUserGitHubParams{
		ID:           user.ID,
		GithubUserID: &ghUser.ID,
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			slog.Info("github account already linked to another user", "github_login", ghUser.Login)
			h.redirectConnectError(w, r, "github_already_linked")
			return
		}
		slog.Error("set user github failed", "err", err)
		h.redirectConnectError(w, r, "internal")
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
		h.unsetUserGitHub(r.Context(), user.ID)
		h.redirectConnectError(w, r, "internal")
		return
	}

	// Persist only the secret_ref + non-sensitive metadata. Re-connect clears any
	// prior revoked flag: a fresh grant re-enables git operations.
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
		h.unsetUserGitHub(r.Context(), user.ID)
		h.redirectConnectError(w, r, "internal")
		return
	}

	h.audit.Record(r.Context(), audit.Entry{
		UserID:   userID,
		Actor:    audit.UserActor(userID),
		Action:   audit.ActionGitHubConnect,
		Target:   userID,
		Metadata: map[string]any{"github_login": ghUser.Login},
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// unsetUserGitHub rolls github_user_id back to NULL after a partial Connect
// GitHub failure, so /api/me github_connected stays false and the SPA keeps
// gating — a "connected" user with no stored tokens would be stuck with
// reconnect_github on every git operation instead.
func (h *Handler) unsetUserGitHub(ctx context.Context, userID pgtype.UUID) {
	if _, err := h.store.SetUserGitHub(ctx, store.SetUserGitHubParams{ID: userID, GithubUserID: nil}); err != nil {
		slog.Error("rollback github link failed", "err", err)
	}
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

// redirectLoginError sends the browser back to the SPA login with an error code
// in the query string so the UI can show a message.
func (h *Handler) redirectLoginError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/login?error="+code, http.StatusFound)
}

// redirectConnectError returns to the dashboard, where the connect-GitHub gate
// screen shows the message (the user is logged in — /login would be wrong).
func (h *Handler) redirectConnectError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/?github_error="+code, http.StatusFound)
}

// setFlowCookie stores a short-lived, HttpOnly round-trip value (state/PKCE).
func (h *Handler) setFlowCookie(w http.ResponseWriter, name, value, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(stateTTL.Seconds()),
	})
}

func (h *Handler) clearFlowCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
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
