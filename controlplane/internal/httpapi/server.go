// Package httpapi wires the control-plane HTTP routes, middleware, and handlers
// together. The router is built with the stdlib net/http 1.22 pattern syntax;
// no framework is involved.
package httpapi

import (
	"net/http"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/gateway"
	"github.com/tavon-ai/proteos/controlplane/internal/github"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/profile"
	"github.com/tavon-ai/proteos/controlplane/internal/providers"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/taskevents"
	"github.com/tavon-ai/proteos/controlplane/internal/token"
)

// Server holds the dependencies shared by all handlers.
type Server struct {
	Sessions *session.Manager
	Auth     *auth.Handler

	// PATs validates Authorization: Bearer personal access tokens (AC1) and backs
	// the /api/tokens management routes. When nil, bearer auth is rejected and the
	// token routes are disabled — only the browser session cookie works.
	PATs *token.Manager

	// Machines drives the machine lifecycle (Phase 2). Required by /api/me and
	// the /api/machine routes.
	Machines *machine.Service

	// Broker and Queries back the SSE endpoint (snapshot + replay + live
	// stream). Queries is the read-only side used for the snapshot/replay.
	Broker  *machine.Broker
	Queries *store.Queries

	// Gateway proxies the browser terminal WebSocket to the machine's guest
	// agent (Phase 3). Nil disables the /gw/terminal route.
	Gateway *gateway.Proxy

	// Guests dials the opaque byte tunnel to a machine's guest agent. It backs
	// the project-download proxy (GET /api/projects/download), which streams a
	// zip of a project straight from the guest. *nodeclient.Client satisfies it.
	// Nil disables the download route.
	Guests gateway.GuestDialer

	// MachineWeb serves the per-machine code-server editor origin
	// (m-<uuid>.<domain>) and mints its web-session tokens (Phase 8). Nil
	// (PROTEOS_MACHINE_DOMAIN unset) disables host-first routing and the
	// /api/machine/web-session route entirely.
	MachineWeb *gateway.MachineWeb

	// Phase 5: the provider registry, the user secrets store, and the audit
	// recorder back the providers/secrets API. Nil disables those routes.
	Providers *providers.Registry
	Secrets   secrets.Store
	Audit     *audit.Recorder

	// Profile backs the portable user-profile API (/api/profile/items): user-scoped
	// credentials/dotfiles that the injector materializes into the user's machines.
	// Nil disables those routes.
	Profile *profile.Store

	// GitConfigurer re-applies a portable git-identity change to running machines
	// (Phase 4). Satisfied by *guestctl.Manager; nil ⇒ changes apply on next boot.
	GitConfigurer GitConfigurer

	// Injector pushes provider secrets into a running guest before an agent
	// launch (Phase 5). Nil ⇒ the push step is skipped (the poller's start-time
	// injection is then the only path).
	Injector Injector

	// Phase 7: git operations. GitHub (REST client), Tokens (per-user token
	// lifecycle), and GitChannel (the guest control channel) back /api/git/*.
	// When any is nil the git routes are disabled. GitHost is the only host clones
	// target; GitHubAppSlug builds the grants URL the Repos panel links to.
	GitHub        *github.Client
	Tokens        *github.TokenSource
	GitChannel    GitChannel
	GitHost       string
	GitHubAppSlug string

	// Phase 9: the project/desktop control-channel surface (projects.list, kv.*).
	// *guestctl.Manager satisfies it. Nil disables /api/projects and
	// /api/machine/desktop and makes session cwd validation reject any non-empty
	// cwd (no listable set to check against).
	Projects ProjectChannel

	// GR1: the worktree-review control-channel surface (git status/diff over a
	// listable project). *guestctl.Manager satisfies it. Nil disables the
	// /api/machines/{id}/git/* routes.
	GitWorktree GitWorktree

	// AT1: the headless agent-run dispatch surface. *guestctl.Manager satisfies
	// it. Nil (or Providers/GitWorktree/Secrets unset) disables the
	// /api/machines/{id}/tasks routes.
	TaskChannel TaskChannel

	// AT2: the live agent-task event fan-out the task SSE endpoint subscribes to.
	// Nil disables GET /api/machines/{id}/tasks/{tid}/events.
	TaskEvents *taskevents.Hub
}

// Handler builds the fully-wired http.Handler with all routes and middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Liveness probe — unauthenticated, no logging noise needed but harmless.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Auth flow (public).
	if s.Auth != nil {
		mux.HandleFunc("GET /api/auth/github/login", s.Auth.Login)
		mux.HandleFunc("GET /api/auth/github/callback", s.Auth.Callback)
		// Logout mutates state, so it requires the CSRF header and a session.
		mux.Handle("POST /api/auth/logout", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.Auth.Logout))))
	}

	// Current user (authenticated).
	mux.Handle("GET /api/me", s.requireAuth(http.HandlerFunc(s.handleMe)))

	// Account preferences (e.g. the project-download mode). A cookie-authed
	// mutation, so it also requires the CSRF header. Reads ride GET /api/me.
	mux.Handle("PATCH /api/user/preferences", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleUpdateUserPrefs))))

	// Personal access tokens (AC1): the user manages their own CLI credentials.
	// Reads are auth-only; create/revoke mutate so they also require the CSRF
	// header (cookie-authed browser settings page) — bearer-authed callers are
	// exempt inside csrfHeader. Enabled only when the token manager is wired.
	if s.PATs != nil {
		mux.Handle("GET /api/tokens", s.requireAuth(http.HandlerFunc(s.handleListTokens)))
		mux.Handle("POST /api/tokens", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleCreateToken))))
		mux.Handle("DELETE /api/tokens/{id}", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleRevokeToken))))
	}

	// Machine routes. Multi-machine: a RESTful collection (/api/machines) plus
	// per-machine ops keyed by {id}. Reads are auth-only; mutations also require
	// the CSRF header. The SSE stream is a GET (no CSRF) — EventSource cannot set
	// custom headers, and it is read-only.
	mux.Handle("GET /api/machines", s.requireAuth(http.HandlerFunc(s.handleListMachines)))
	mux.Handle("POST /api/machines", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleCreateMachine))))
	mux.Handle("GET /api/machines/{id}", s.requireAuth(http.HandlerFunc(s.handleGetMachine)))
	mux.Handle("PATCH /api/machines/{id}", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleRenameMachine))))
	mux.Handle("DELETE /api/machines/{id}", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleDestroyMachine))))
	mux.Handle("POST /api/machines/{id}/start", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleStartMachine))))
	mux.Handle("POST /api/machines/{id}/stop", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleStopMachine))))
	mux.Handle("GET /api/machine/events", s.requireAuth(http.HandlerFunc(s.handleMachineEvents)))

	// Machine-template catalog (read-only): backs the create-machine picker.
	mux.Handle("GET /api/templates", s.requireAuth(http.HandlerFunc(s.handleListTemplates)))

	// Machine-web session mint (Phase 8): the main-origin endpoint that issues the
	// short-lived editor URL. Enabled only when machine-web is configured.
	if s.MachineWeb != nil {
		mux.Handle("POST /api/machine/web-session", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleWebSession))))
	}

	// Providers + secrets API (Phase 5). Reads are auth-only; the write-only key
	// mutations also require the CSRF header. No read route for key material
	// exists — the API shape makes leakage impossible, not merely avoided.
	if s.Providers != nil {
		mux.Handle("GET /api/providers", s.requireAuth(http.HandlerFunc(s.handleListProviders)))
		mux.Handle("PUT /api/secrets/providers/{key}", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleSetProviderKey))))
		mux.Handle("DELETE /api/secrets/providers/{key}", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleDeleteProviderKey))))
	}

	// Portable user profile (Phase 1). The list is a metadata-only read (auth);
	// set/delete mutate so they also require the CSRF header. As with the secrets
	// API, no read route for the stored value exists. Enabled only when wired.
	if s.Profile != nil {
		mux.Handle("GET /api/profile/items", s.requireAuth(http.HandlerFunc(s.handleListProfileItems)))
		mux.Handle("PUT /api/profile/items/{key}", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleSetProfileItem))))
		mux.Handle("DELETE /api/profile/items/{key}", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleDeleteProfileItem))))

		// Typed Phase 4 conveniences over the same store: git identity (reconciled
		// with the Phase 7 git.configure path) and the SSH key. Reads are auth-only;
		// mutations require the CSRF header. No private key is ever returned.
		mux.Handle("GET /api/profile/git", s.requireAuth(http.HandlerFunc(s.handleGetGitIdentity)))
		mux.Handle("PUT /api/profile/git", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleSetGitIdentity))))
		mux.Handle("DELETE /api/profile/git", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleDeleteGitIdentity))))
		mux.Handle("GET /api/profile/ssh", s.requireAuth(http.HandlerFunc(s.handleGetSSHKey)))
		mux.Handle("POST /api/profile/ssh", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleGenerateSSHKey))))
		mux.Handle("DELETE /api/profile/ssh", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleDeleteSSHKey))))
	}

	// Git operations (Phase 7). Reads are auth-only; clone mutates state so it
	// also requires the CSRF header. Enabled only when the full git stack is wired.
	if s.GitHub != nil && s.Tokens != nil && s.GitChannel != nil {
		mux.Handle("GET /api/git/repos", s.requireAuth(http.HandlerFunc(s.handleGitRepos)))
		mux.Handle("POST /api/git/clone", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleGitClone))))
	}

	// Projects + desktop layout (Phase 9). Projects is a read over the control
	// channel; the desktop layout is a read/write of machine SQLite (PUT mutates
	// so it also requires the CSRF header). Enabled only when the control channel
	// surface is wired.
	if s.Projects != nil {
		mux.Handle("GET /api/projects", s.requireAuth(http.HandlerFunc(s.handleProjects)))
		mux.Handle("GET /api/machine/desktop", s.requireAuth(http.HandlerFunc(s.handleGetDesktop)))
		mux.Handle("PUT /api/machine/desktop", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handlePutDesktop))))
		// Download a project as a zip: authorize the project against the listable
		// set (the resolveSessionCwd gate), then proxy the guest's zip stream. A
		// read-only GET — the response is Content-Disposition: attachment, so a
		// same-origin navigation downloads without a CSRF header (EventSource-style).
		if s.Guests != nil {
			mux.Handle("GET /api/projects/download", s.requireAuth(http.HandlerFunc(s.handleProjectDownload)))
		}
	}

	// Worktree review (GR1): read a project's git status/diff over the control
	// channel. Both are reads (auth only, no CSRF). Enabled only when the
	// worktree surface is wired.
	if s.GitWorktree != nil {
		mux.Handle("GET /api/machines/{id}/git/status", s.requireAuth(http.HandlerFunc(s.handleGitStatus)))
		mux.Handle("GET /api/machines/{id}/git/diff", s.requireAuth(http.HandlerFunc(s.handleGitDiff)))
		// Branch create/checkout mutates, so it also requires the CSRF header (GR2).
		mux.Handle("POST /api/machines/{id}/git/branch", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleGitBranch))))
		// Commit mutates too (GR3) — the explicit, CSRF-guarded review gate.
		mux.Handle("POST /api/machines/{id}/git/commit", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleGitCommit))))
		// Push is async (GR4): 202 + op_id, completion over SSE. CSRF-guarded.
		mux.Handle("POST /api/machines/{id}/git/push", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleGitPush))))
		// Open PR (GR5) — a CP→GitHub call, so it also needs the GitHub client +
		// token source. The final hop of the review→ship loop.
		if s.GitHub != nil && s.Tokens != nil {
			mux.Handle("POST /api/machines/{id}/git/pr", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleGitPR))))
		}
	}

	// Headless agent tasks (AT1). Creating a task mutates + dispatches a run, so
	// it needs the provider registry + secrets (key check) and the worktree
	// surface (project resolution); reads are auth-only. POST is CSRF-guarded.
	if s.TaskChannel != nil && s.Providers != nil && s.GitWorktree != nil && s.Secrets != nil {
		mux.Handle("POST /api/machines/{id}/tasks", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleCreateTask))))
		mux.Handle("GET /api/machines/{id}/tasks", s.requireAuth(http.HandlerFunc(s.handleListTasks)))
		mux.Handle("GET /api/machines/{id}/tasks/{tid}", s.requireAuth(http.HandlerFunc(s.handleGetTask)))
		// Cancel a running task (AT3): a mutation, so CSRF-guarded.
		mux.Handle("POST /api/machines/{id}/tasks/{tid}/cancel", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleCancelTask))))
		// Follow-up turn on a finished task (AT4: resume the agent session). A
		// dispatch mutation, so CSRF-guarded.
		mux.Handle("POST /api/machines/{id}/tasks/{tid}/messages", s.requireAuth(s.csrfHeader(http.HandlerFunc(s.handleSendMessage))))
	}

	// AT2: live agent-event SSE for one task. A GET stream (no CSRF — like the
	// machine SSE; EventSource cannot set headers and it is read-only). It only
	// reads the task row + the event hub, so it is wired independently of the
	// dispatch stack (which needs the provider/secret/worktree surfaces).
	if s.TaskEvents != nil {
		mux.Handle("GET /api/machines/{id}/tasks/{tid}/events", s.requireAuth(http.HandlerFunc(s.handleTaskEvents)))
	}

	// Terminal gateway (Phase 3). requireAuth handles the 401; the Origin check
	// and ownership resolution happen inside the handler/proxy. EventSource-style
	// CSRF does not apply — the WS Origin check is the CSRF equivalent here.
	if s.Gateway != nil {
		mux.Handle("GET /gw/terminal", s.requireAuth(http.HandlerFunc(s.handleGatewayTerminal)))
		// Agent terminal session (Phase 5): same chain as /gw/terminal plus
		// provider registration/key checks and an idempotent secret push.
		if s.Providers != nil {
			mux.Handle("GET /gw/agent/{provider}", s.requireAuth(http.HandlerFunc(s.handleGatewayAgent)))
		}
	}

	// Host-first routing (Phase 8 decision #1): a machine-web host
	// (m-<uuid>.<domain>) is served ONLY by the machine-web handler — never the
	// API/SPA mux — and the main host never reaches the machine-web handler. This
	// makes the origin split structural, not just conventional.
	var root http.Handler = mux
	if s.MachineWeb != nil {
		root = s.hostRouter(mux)
	}

	// Wrap everything in request logging and panic recovery.
	return requestLogger(recoverer(root))
}

// hostRouter dispatches a request to the machine-web handler when its Host is a
// machine subdomain, else to the main mux.
func (s *Server) hostRouter(mainMux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.MachineWeb.Matches(r.Host) {
			s.MachineWeb.ServeHTTP(w, r)
			return
		}
		mainMux.ServeHTTP(w, r)
	})
}
