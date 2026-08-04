package app_test

import (
	"strings"
	"testing"
)

// TestSSHPrintBuildsProxyCommand pins the exact ssh(1) invocation the UI's
// instructions and docs/CLI.md tell users to expect: a ProxyCommand routed back
// through this binary's ssh-proxy, the guest run-as user, and the machine id as
// the host (so known_hosts is per machine, matching each guest persisting its
// own host key). --print is also the supported way to get the command for a
// client proteos can't launch itself, e.g. an IDE's remote-SSH config.
func TestSSHPrintBuildsProxyCommand(t *testing.T) {
	code, out, errb := runCLI(t, "https://cp.example", "tok", "ssh", "--print", "m-123")
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, errb)
	}
	got := strings.TrimSpace(out)
	for _, want := range []string{
		"ssh -o ProxyCommand=",
		"ssh-proxy",
		"--url https://cp.example",
		"m-123",
		"-l dev",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("printed command %q missing %q", got, want)
		}
	}
	// The host argument must be last, so trailing pass-through args land after it
	// the way ssh expects.
	if !strings.HasSuffix(got, " m-123") {
		t.Fatalf("printed command %q does not end with the host", got)
	}
}

// TestSSHPrintPassesThroughExtraArgs proves args after the machine id reach ssh
// verbatim — that is what makes `proteos ssh m-1 -- uname -a` and extra ssh
// flags work rather than being swallowed by our own flag parsing.
func TestSSHPrintPassesThroughExtraArgs(t *testing.T) {
	code, out, errb := runCLI(t, "https://cp.example", "tok", "ssh", "--print", "m-123", "--", "uname", "-a")
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, errb)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "m-123 uname -a") {
		t.Fatalf("pass-through args missing from %q", out)
	}
}

// TestSSHUserOverride covers the --no-user image case, where the guest has no
// 'dev' account and sessions land as root.
func TestSSHUserOverride(t *testing.T) {
	code, out, _ := runCLI(t, "https://cp.example", "tok", "ssh", "--user", "root", "--print", "m-123")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "-l root") {
		t.Fatalf("--user not honored: %q", out)
	}
}

// TestSSHRequiresMachine and TestSSHProxyRequiresMachine: both commands need a
// machine id, and neither should contact the server without one.
func TestSSHRequiresMachine(t *testing.T) {
	if code, _, _ := runCLI(t, "https://cp.example", "tok", "ssh"); code == 0 {
		t.Fatal("ssh with no machine id succeeded, want a usage error")
	}
	if code, _, _ := runCLI(t, "https://cp.example", "tok", "ssh-proxy"); code == 0 {
		t.Fatal("ssh-proxy with no machine id succeeded, want a usage error")
	}
}

// TestSSHUnauthenticated proves `proteos ssh` reports a missing credential
// itself, with the CLI's own exit code, rather than letting it surface as an
// opaque ssh "proxy command failed" from the child process.
func TestSSHUnauthenticated(t *testing.T) {
	code, _, errb := runCLI(t, "https://cp.example", "", "ssh", "--print", "m-123")
	if code == 0 {
		t.Fatal("ssh without a token succeeded, want an auth error")
	}
	if !strings.Contains(errb, "not authenticated") {
		t.Fatalf("stderr = %q, want an authentication message", errb)
	}
}
