package gateway

// Signed values for the machine-web flow (decision #2). Both the one-shot
// web-session token (main origin → editor frame) and the subdomain cookie are
// HMAC-signed with the reused StateSigningKey. Format mirrors the OAuth state
// token: base64url(json-claims).base64url(hmac). The token is short-lived (≤60s)
// and single-purpose; the cookie binds to the parent session id so a logout /
// revoke kills the editor via the per-request alive check (not via the cookie's
// own expiry).

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var errBadMachineToken = errors.New("gateway: invalid or expired machine token")

// machineToken is the one-shot web-session token minted at the main origin.
type machineToken struct {
	MachineID string `json:"m"`
	UserID    string `json:"u"`
	SessionID string `json:"s"`
	Exp       int64  `json:"e"`           // unix seconds
	Folder    string `json:"f,omitempty"` // Phase 9: validated open-folder path (/workspace/<repo>)
	Port      uint32 `json:"p,omitempty"` // PP1: preview target port; 0 ⇒ the port-less editor origin
}

// machineCookie is the subdomain-scoped editor/preview cookie value (signed). It
// carries no user secret — just the machine + the parent session id it is bound
// to, plus the preview port so the cookie is scoped to a single (machine, port)
// origin (0 ⇒ the editor). The cookie is host-only (no Domain), so the browser
// already keeps it per-origin; the Port check is server-side defence in depth.
type machineCookie struct {
	MachineID string `json:"m"`
	SessionID string `json:"s"`
	Exp       int64  `json:"e"`
	Port      uint32 `json:"p,omitempty"`
}

func signMachineToken(key []byte, t machineToken) string { return signClaims(key, t) }

func verifyMachineToken(key []byte, raw string) (machineToken, error) {
	var t machineToken
	if err := verifyClaims(key, raw, &t); err != nil {
		return machineToken{}, err
	}
	if t.MachineID == "" || t.SessionID == "" || time.Now().Unix() > t.Exp {
		return machineToken{}, errBadMachineToken
	}
	return t, nil
}

func signMachineCookie(key []byte, c machineCookie) string { return signClaims(key, c) }

func verifyMachineCookie(key []byte, raw string) (machineCookie, error) {
	var c machineCookie
	if err := verifyClaims(key, raw, &c); err != nil {
		return machineCookie{}, err
	}
	if c.MachineID == "" || c.SessionID == "" || time.Now().Unix() > c.Exp {
		return machineCookie{}, errBadMachineToken
	}
	return c, nil
}

// signClaims marshals v to JSON and returns base64url(json).base64url(hmac).
func signClaims(key []byte, v any) string {
	b, _ := json.Marshal(v)
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + macOf(key, payload)
}

// verifyClaims checks the signature (constant-time) and decodes the payload into
// v. Expiry is the caller's responsibility (the claim shapes differ).
func verifyClaims(key []byte, raw string, v any) error {
	payload, sig, ok := strings.Cut(raw, ".")
	if !ok || payload == "" || sig == "" {
		return errBadMachineToken
	}
	if subtle.ConstantTimeCompare([]byte(macOf(key, payload)), []byte(sig)) != 1 {
		return errBadMachineToken
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return errBadMachineToken
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errBadMachineToken
	}
	return nil
}

func macOf(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
