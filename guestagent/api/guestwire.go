// Package guestwire is the terminal WebSocket protocol contract spoken across
// browser ↔ gateway ↔ guest agent. It is the ONLY guestagent package the
// control plane imports (same pattern as nodeagent/api), so it must stay free
// of any dependency the control plane should not pull in — pure types, consts,
// and a tiny validation helper only.
//
// The protocol rides a single WebSocket:
//
//   - Binary frames carry raw PTY bytes. client→guest = keystrokes; guest→client
//     = terminal output. On attach the guest first replays its scrollback ring
//     (≤ scrollback bytes) as binary, then streams live output.
//   - Text frames carry small JSON control messages (Frame below), discriminated
//     by "type": hello (guest→client, once, first), resize (client→guest),
//     exit (guest→client, last, followed by a normal close).
package guestwire

import (
	"encoding/json"
	"regexp"
)

// FrameType is the discriminator of a text (JSON) control frame.
type FrameType string

const (
	// FrameHello is sent once by the guest immediately after upgrade, before any
	// binary output, announcing the resolved session name and how many bytes of
	// scrollback are about to be replayed.
	FrameHello FrameType = "hello"

	// FrameResize is sent by the client whenever the terminal viewport changes;
	// the guest applies it to the PTY (TIOCSWINSZ).
	FrameResize FrameType = "resize"

	// FrameExit is sent by the guest when the shell process exits, carrying its
	// exit code. The guest then closes the WebSocket with code 1000.
	FrameExit FrameType = "exit"
)

// Frame is the envelope for every text control message. Only the fields
// relevant to its Type are populated; the rest serialize as zero/omitted.
type Frame struct {
	Type FrameType `json:"type"`

	// Hello fields.
	Session     string `json:"session,omitempty"`
	ReplayBytes int    `json:"replay_bytes,omitempty"`

	// Resize fields. Cols/Rows are the terminal dimensions in cells.
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`

	// Exit field. ExitCode is the shell's exit status (pointer so 0 is sent
	// explicitly rather than omitted — a clean exit must be distinguishable from
	// "field absent").
	ExitCode *int `json:"exit_code,omitempty"`
}

// WebSocket close codes the gateway sends to the browser. 1000/1011 are the
// RFC6455 standard codes; the 4000-range is application-private.
const (
	CloseNormal              = 1000 // clean shutdown (shell exited, or client closed)
	CloseSessionRevoked      = 4001 // the user's session was revoked/logged out
	CloseMachineStopped      = 4002 // the machine stopped out from under the session
	CloseProviderUnavailable = 4003 // an agent session asked for an un-injected provider
	CloseInternal            = 1011 // unexpected server-side failure
)

// CloseProviderUnavailable close reasons. The reason is the human-readable
// string sent alongside code 4003, distinguishing why the provider is
// unavailable so the browser can show an actionable message.
const (
	// CloseReasonNotInjected: the provider was never injected (no key set, or the
	// push has not reached this guest yet).
	CloseReasonNotInjected = "provider unavailable"
	// CloseReasonSetupFailed: the provider's setup_command failed on the last
	// push, so it is marked degraded and cannot be launched until a re-push
	// (e.g. key rotation) re-runs setup successfully (Phase 6 decision #3).
	CloseReasonSetupFailed = "setup_failed"
)

// DefaultSession is the session name attached to when ?session= is absent.
const DefaultSession = "main"

// Phase 4 control surface, served by the guest agent on the same private
// transport (vsock/unix). These are NOT browser-facing; the node-agent calls
// them directly after a snapshot restore (decision #9). Routes:
//
//	PUT /resume  {ResumeRequest}  → 200 {ResumeResponse}
//	GET /info                     → 200 {Info}
const (
	RouteResume = "PUT /resume"
	RouteInfo   = "GET /info"
)

// Phase 5 secret injection. The control plane pushes provider definitions
// (launch command + env) over the private transport; the guest stores them in
// memory and as 0600 tmpfs env files. Replace-all, idempotent.
//
//	PUT /secrets  {SecretsRequest}  → 204
const RouteSecrets = "PUT /secrets"

// RouteSecretsPath is the path portion of RouteSecrets, used by the control
// plane to build the PUT URL over the guest tunnel.
const RouteSecretsPath = "/secrets"

// Phase 7 control channel. A single bidirectional WebSocket the control plane
// dials at the guest's GET /control endpoint (CP-initiated, so the guest never
// opens a connection and the channel inherits the vsock topology-attested
// identity — Phase 7 decision #1). It carries JSON request/response frames in
// both directions:
//
//   - CP → guest: git.configure (write ~/.gitconfig), git.clone (clone a repo
//     into the workspace). git.clone is acked immediately; completion arrives
//     later as a guest → CP git.clone.done frame.
//   - guest → CP: git.credential (the in-VM credential helper asks for a fresh
//     git token on demand), git.clone.done (clone completion notification).
//
// Exactly one channel exists per running machine; on reconnect the CP re-sends
// git.configure (it is idempotent).
const RouteControl = "GET /control"

// RouteControlPath is the path portion of RouteControl, used by the control
// plane to build the WebSocket URL over the guest tunnel.
const RouteControlPath = "/control"

// AgentSockPath is the in-VM unix socket (on tmpfs) the credential helper
// subprocess talks to. The guest agent serves it and relays git.credential
// requests over the control channel (Phase 7 decision #5). Never written to by
// anything but the agent; carries no secret at rest.
const AgentSockPath = "/run/proteos/agent.sock"

// HelperBinPath is the credential.helper command baked into the pushed gitconfig:
// the same static guestagent binary, invoked with the git-credential subcommand
// (Phase 7 decision #5). git appends the action (get/store/erase) as an argument.
const HelperBinPath = "/usr/local/bin/guestagent git-credential"

// ControlKind discriminates a control frame: a request, a successful response,
// or an error response.
type ControlKind string

const (
	ControlReq  ControlKind = "req"  // initiates an operation; carries Op + Payload
	ControlResp ControlKind = "resp" // success reply to the req with the same ID
	ControlErr  ControlKind = "err"  // failure reply to the req with the same ID
)

// Control operation names (the Op field of a req frame).
const (
	OpGitConfigure  = "git.configure"  // CP → guest
	OpGitClone      = "git.clone"      // CP → guest (acked; completion via OpGitCloneDone)
	OpGitCloneDone  = "git.clone.done" // guest → CP (completion notification)
	OpGitCredential = "git.credential" // guest → CP
)

// ControlError codes (the Code field of an err frame's ControlErrorPayload).
const (
	// ErrCodeReconnectGitHub: the user's GitHub grant is revoked or its refresh
	// token is dead; the user must re-run the login flow. The helper surfaces this
	// on stderr and exits non-zero so git stops cleanly.
	ErrCodeReconnectGitHub = "reconnect_github"
	// ErrCodeForbiddenHost: the credential request named a host/protocol the
	// control plane will not mint a credential for (only github.com/https).
	ErrCodeForbiddenHost = "forbidden_host"
	// ErrCodeUnavailable: a transient failure (token store unreachable, no owner
	// resolvable, etc.). The helper exits non-zero; git reports the failure.
	ErrCodeUnavailable = "unavailable"
)

// ControlFrame is the envelope for every control-channel message. ID pairs a
// resp/err with its req; it is unique per direction. For a req, Op + Payload are
// set. For a resp, Payload carries the operation's result. For an err, Payload
// is a ControlErrorPayload.
type ControlFrame struct {
	ID      int64           `json:"id"`
	Kind    ControlKind     `json:"kind"`
	Op      string          `json:"op,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// GitConfigurePayload is the body of a git.configure req: the identity and
// helper wiring written to ~/.gitconfig. It contains NO secret (the token is
// fetched on demand by the helper), so persisting the file is safe.
type GitConfigurePayload struct {
	Name   string `json:"name"`   // user.name (GitHub login or display name)
	Email  string `json:"email"`  // user.email (primary/noreply)
	Helper string `json:"helper"` // credential.helper (HelperBinPath)
}

// GitClonePayload is the body of a git.clone req. The URL never embeds a token —
// the credential helper supplies it at fetch time, so .git/config keeps a clean
// remote URL.
type GitClonePayload struct {
	URL  string `json:"url"`   // https clone URL, no credentials
	Dest string `json:"dest"`  // absolute path under the workspace
	OpID string `json:"op_id"` // correlates the later git.clone.done frame
}

// GitCloneDonePayload is the body of a guest → CP git.clone.done req: the
// outcome of an earlier git.clone, correlated by OpID.
type GitCloneDonePayload struct {
	OpID   string `json:"op_id"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"` // sanitized failure detail (never a token)
}

// GitCredentialRequest is the body of a guest → CP git.credential req.
type GitCredentialRequest struct {
	Host     string `json:"host"`     // must be the configured git host (github.com)
	Protocol string `json:"protocol"` // must be https
}

// GitCredentialResponse is the body of a successful git.credential resp: a
// short-lived credential and its absolute expiry (RFC3339), so the helper can
// pass password_expiry_utc back to git.
type GitCredentialResponse struct {
	Username string `json:"username"`         // x-access-token for GitHub App user tokens
	Password string `json:"password"`         // the access token (never logged/persisted)
	Expiry   string `json:"expiry,omitempty"` // RFC3339 access-token expiry
}

// ControlErrorPayload is the body of an err frame: a machine-readable code plus
// an optional human message (never carrying secret material).
type ControlErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// AgentSessionPrefix marks a terminal session that should spawn a provider's
// injected launch command instead of the login shell. The provider key is the
// remainder, e.g. session "agent-claude" → provider "claude".
const AgentSessionPrefix = "agent-"

// ProviderDef is one provider's injected launch definition: the command to run
// for an agent session, the environment (secret) it runs with, and an optional
// setup command run once per push to complete login-style auth (Phase 6
// decision #3). Env values are sensitive (API keys) and must never be logged.
type ProviderDef struct {
	Command string            `json:"command"`
	Env     map[string]string `json:"env"`

	// SetupCommand, when non-empty, is a shell command the guest runs as a root
	// login shell after the env file is written, on every push (start, resume,
	// rotation). It completes auth styles that need a login step rather than
	// pure env — e.g. Codex's `printenv OPENAI_API_KEY | codex login
	// --with-api-key`, which writes ~/.codex/auth.json. It runs asynchronously
	// with its output captured to the guest-agent log and MUST be idempotent
	// (run-on-every-push avoids any "has it run yet" state machine). A failing
	// setup marks the provider degraded; launching it then closes the agent WS
	// with CloseProviderUnavailable and reason CloseReasonSetupFailed instead of
	// spawning a broken TUI.
	SetupCommand string `json:"setup_command,omitempty"`
}

// SecretsRequest is the body of PUT /secrets: the full set of provider
// definitions to install, replacing any previously injected set.
type SecretsRequest struct {
	Providers map[string]ProviderDef `json:"providers"`
}

// ProviderKeyFromSession returns the provider key for an agent session name, and
// whether name was an agent session at all.
func ProviderKeyFromSession(session string) (key string, ok bool) {
	if len(session) > len(AgentSessionPrefix) && session[:len(AgentSessionPrefix)] == AgentSessionPrefix {
		return session[len(AgentSessionPrefix):], true
	}
	return "", false
}

// Persistence modes reported in Info.Persist.
const (
	PersistDisk = "disk" // /persist is a mounted (encrypted) block device
	PersistDir  = "dir"  // a plain directory (dev override), no mount
	PersistNone = "none" // degraded: no persistent storage available
)

// ResumeRequest is the body of PUT /resume: the host-provided wall clock and a
// blob of fresh entropy to reseed the guest CRNG after a snapshot restore
// (decision #9). Driving it from the host keeps resume deterministic — no
// dependency on guest NTP egress at the resume instant.
type ResumeRequest struct {
	UnixNanos  int64  `json:"unix_nanos"`
	EntropyB64 string `json:"entropy_b64,omitempty"`
}

// ResumeResponse is the body of a successful PUT /resume: the wall-clock skew
// the guest corrected, in milliseconds (signed: positive ⇒ guest clock was
// behind the host and was advanced).
type ResumeResponse struct {
	SkewCorrectedMS int64 `json:"skew_corrected_ms"`
}

// Boot describes one boot/resume event recorded in the machine SQLite.
type Boot struct {
	Kind string `json:"kind"` // "cold" | "resumed"
	TS   int64  `json:"ts"`   // unix seconds
}

// Info is the body of GET /info: the guest agent version, the persistence mode,
// and the most recent boot event (used by tests and the control plane).
type Info struct {
	Version  string `json:"version"`
	Persist  string `json:"persist"` // PersistDisk | PersistDir | PersistNone
	LastBoot *Boot  `json:"last_boot,omitempty"`
}

// Boot kinds recorded in the machine SQLite and reported in Boot.Kind.
const (
	BootCold    = "cold"
	BootResumed = "resumed"
)

// sessionNameRe constrains session names to a small, path/identifier-safe set:
// 1–32 chars of lowercase letters, digits, and hyphens.
var sessionNameRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidSessionName reports whether name is an acceptable session identifier.
// An empty name is NOT valid here; callers substitute DefaultSession before
// validating.
func ValidSessionName(name string) bool {
	return sessionNameRe.MatchString(name)
}
