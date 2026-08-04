package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tavon-ai/proteos/controlplane/internal/auth"
)

// getSSH issues a plain HTTP GET to /gw/ssh (no WebSocket upgrade), so
// pre-upgrade authz outcomes surface as ordinary status codes — same shape as
// getTerminal in gateway_test.go.
func getSSH(t *testing.T, fx cpFixture, withCookie bool, origin, machineParam string) int {
	t.Helper()
	u := fx.url + "/gw/ssh"
	if machineParam != "" {
		u += "?machine=" + machineParam
	}
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	if withCookie {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// getSSHError is getSSH plus the {error} code from the response envelope, for
// distinguishing the two different 403s this route can return
// (origin_forbidden vs ssh_disabled).
func getSSHError(t *testing.T, fx cpFixture, origin, machineParam string) (int, string) {
	t.Helper()
	u := fx.url + "/gw/ssh"
	if machineParam != "" {
		u += "?machine=" + machineParam
	}
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Error
}

// enableSSHFor flips the fixture user's account-wide inbound-SSH switch. A user
// created by setupCP has it off (migration 000019 defaults to false), so any
// /gw/ssh test that expects to get past that gate must call this first.
func enableSSHFor(t *testing.T, fx cpFixture, on bool) {
	t.Helper()
	if _, err := fx.pool.Exec(context.Background(),
		"UPDATE users SET ssh_enabled=$2 WHERE id=$1", fx.userID, on); err != nil {
		t.Fatal(err)
	}
}

// TestGatewaySSHAuthzTable mirrors TestGatewayAuthzTable with the one
// deliberate difference /gw/ssh has from /gw/terminal: a request with NO Origin
// header at all (the CLI's bearer-token ProxyCommand shape, which never sends
// one) is NOT rejected for that reason — only a request that sends an Origin
// not on the allowlist is. Every other outcome (auth, machine ownership,
// running state) matches the terminal gateway exactly. setupCP seeds the user
// with exactly one running machine, so an empty ?machine= resolves to it
// (machine.Service.OnlyMachine) rather than 404ing — a request that clears every
// authz gate reaches SSHProxy.Serve, which attempts the WebSocket upgrade; a
// plain (non-WS) GET fails that with 426 Upgrade Required, distinguishing "authz
// passed, upgrade attempted" from an actual rejection (401/403/404/409).
func TestGatewaySSHAuthzTable(t *testing.T) {
	t.Parallel()
	fx := setupCP(t, failDialer{t}, []string{testWSOrigin})
	ctx := context.Background()
	// Inbound SSH is opt-in per account; TestGatewaySSHRespectsAccountSwitch
	// covers the off state. Everything below is about the other authz gates.
	enableSSHFor(t, fx, true)

	if code := getSSH(t, fx, false, testWSOrigin, ""); code != http.StatusUnauthorized {
		t.Fatalf("no cookie: want 401, got %d", code)
	}
	if code := getSSH(t, fx, true, "http://evil.example", ""); code != http.StatusForbidden {
		t.Fatalf("bad Origin: want 403, got %d", code)
	}
	if code := getSSH(t, fx, true, testWSOrigin, "00000000-0000-0000-0000-000000000000"); code != http.StatusNotFound {
		t.Fatalf("foreign machine: want 404, got %d", code)
	}
	// The key divergence from /gw/terminal: no Origin header at all (the CLI
	// shape) is not itself a 403 — it clears authz and reaches the upgrade
	// attempt, same as a request with a valid Origin.
	if code := getSSH(t, fx, true, "", ""); code != http.StatusUpgradeRequired {
		t.Fatalf("missing Origin (CLI shape): want 426 (authz passed, non-WS GET), got %d", code)
	}
	if code := getSSH(t, fx, true, testWSOrigin, fx.machineID); code != http.StatusUpgradeRequired {
		t.Fatalf("valid Origin + owned running machine: want 426 (authz passed, non-WS GET), got %d", code)
	}

	// Not running ⇒ 409 (machine exists and is owned, but stopped) — checked
	// before the Serve/upgrade attempt, for both request shapes.
	if _, err := fx.pool.Exec(ctx, "UPDATE machines SET state='stopped' WHERE id=$1", fx.machinePgID); err != nil {
		t.Fatal(err)
	}
	if code := getSSH(t, fx, true, testWSOrigin, ""); code != http.StatusConflict {
		t.Fatalf("stopped machine: want 409, got %d", code)
	}
	if code := getSSH(t, fx, true, "", ""); code != http.StatusConflict {
		t.Fatalf("stopped machine (CLI shape, no Origin): want 409, got %d", code)
	}
}

// TestGatewaySSHRespectsAccountSwitch proves the Settings → SSH access checkbox
// gates the tunnel itself, not just key injection: with the switch off a fully
// authorized request for an owned, running machine is refused with a distinct
// ssh_disabled code. This is the enforcement point that makes a disable
// immediate — the injector's authorized_keys removal is a best-effort push, so
// it cannot on its own guarantee the next connection attempt fails. The refusal
// lands before the machine lookup, so a disabled account can't probe for which
// machine ids exist.
func TestGatewaySSHRespectsAccountSwitch(t *testing.T) {
	t.Parallel()
	fx := setupCP(t, failDialer{t}, []string{testWSOrigin})

	// Off (the default for a new account).
	if code, errCode := getSSHError(t, fx, testWSOrigin, fx.machineID); code != http.StatusForbidden || errCode != "ssh_disabled" {
		t.Fatalf("SSH disabled: want 403/ssh_disabled, got %d/%q", code, errCode)
	}
	// The CLI shape (no Origin) is refused for the same reason, not silently
	// exempted by the Origin carve-out.
	if code, errCode := getSSHError(t, fx, "", fx.machineID); code != http.StatusForbidden || errCode != "ssh_disabled" {
		t.Fatalf("SSH disabled (CLI shape): want 403/ssh_disabled, got %d/%q", code, errCode)
	}
	// A bogus machine id is still ssh_disabled, not 404: the switch is checked
	// first, so nothing about machine existence leaks while SSH is off.
	if code, errCode := getSSHError(t, fx, testWSOrigin, "00000000-0000-0000-0000-000000000000"); code != http.StatusForbidden || errCode != "ssh_disabled" {
		t.Fatalf("SSH disabled + unknown machine: want 403/ssh_disabled, got %d/%q", code, errCode)
	}

	// On: the same request now clears authz and reaches the upgrade attempt.
	enableSSHFor(t, fx, true)
	if code := getSSH(t, fx, true, testWSOrigin, fx.machineID); code != http.StatusUpgradeRequired {
		t.Fatalf("SSH enabled: want 426 (authz passed, non-WS GET), got %d", code)
	}

	// And back off again — the switch is live per request, with no restart or
	// re-inject in between.
	enableSSHFor(t, fx, false)
	if code, errCode := getSSHError(t, fx, testWSOrigin, fx.machineID); code != http.StatusForbidden || errCode != "ssh_disabled" {
		t.Fatalf("SSH re-disabled: want 403/ssh_disabled, got %d/%q", code, errCode)
	}
}
