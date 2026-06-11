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

import "regexp"

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
	CloseNormal         = 1000 // clean shutdown (shell exited, or client closed)
	CloseSessionRevoked = 4001 // the user's session was revoked/logged out
	CloseMachineStopped = 4002 // the machine stopped out from under the session
	CloseInternal       = 1011 // unexpected server-side failure
)

// DefaultSession is the session name attached to when ?session= is absent.
const DefaultSession = "main"

// sessionNameRe constrains session names to a small, path/identifier-safe set:
// 1–32 chars of lowercase letters, digits, and hyphens.
var sessionNameRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidSessionName reports whether name is an acceptable session identifier.
// An empty name is NOT valid here; callers substitute DefaultSession before
// validating.
func ValidSessionName(name string) bool {
	return sessionNameRe.MatchString(name)
}
