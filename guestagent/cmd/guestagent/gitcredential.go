package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	"github.com/tavon-ai/proteos/guestagent/internal/localsock"
)

// gitCredential implements the git credential helper protocol (Phase 7 decision
// #5). git invokes `guestagent git-credential <action>` and speaks the credential
// protocol on stdio. Only `get` is meaningful: it relays the request to the guest
// agent over the local socket, which fetches a fresh token over the control
// channel. `store`/`erase` are no-ops — nothing token-shaped is ever written to
// disk in the VM.
func gitCredential(args []string) error {
	action := ""
	if len(args) > 0 {
		action = args[0]
	}

	attrs, err := readCredentialAttrs(os.Stdin)
	if err != nil {
		return err
	}

	if action != "get" {
		// store / erase: deliberately do nothing.
		return nil
	}

	host := attrs["host"]
	protocol := attrs["protocol"]
	if protocol == "" {
		protocol = "https"
	}

	// The socket path is fixed in production (the agent serves AgentSockPath); the
	// override exists only so the e2e harness can point the helper at a temp socket.
	sockPath := guestwire.AgentSockPath
	if v := os.Getenv("PROTEOS_AGENT_SOCK"); v != "" {
		sockPath = v
	}
	resp, err := localsock.Fetch(sockPath, localsock.Request{Host: host, Protocol: protocol})
	if err != nil {
		fmt.Fprintln(os.Stderr, "proteos: credential helper could not reach the guest agent:", err)
		return err
	}
	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, credentialErrMessage(resp.Error))
		return fmt.Errorf("credential error: %s", resp.Error)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "username=%s\n", resp.Username)
	fmt.Fprintf(&b, "password=%s\n", resp.Password)
	if exp := unixExpiry(resp.Expiry); exp != "" {
		fmt.Fprintf(&b, "password_expiry_utc=%s\n", exp)
	}
	_, err = io.WriteString(os.Stdout, b.String())
	return err
}

// readCredentialAttrs parses the key=value lines git writes on stdin, stopping
// at the first blank line or EOF.
func readCredentialAttrs(r io.Reader) (map[string]string, error) {
	attrs := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		attrs[k] = v
	}
	return attrs, sc.Err()
}

// unixExpiry converts an RFC3339 expiry to the integer Unix seconds git expects
// for password_expiry_utc. An empty/unparseable value yields "" (omitted).
func unixExpiry(rfc3339 string) string {
	if rfc3339 == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d", t.UTC().Unix())
}

func credentialErrMessage(code string) string {
	switch code {
	case guestwire.ErrCodeReconnectGitHub:
		return "proteos: your GitHub connection is no longer valid — reconnect GitHub in the ProteOS dashboard, then retry."
	case guestwire.ErrCodeForbiddenHost:
		return "proteos: ProteOS only provides credentials for github.com over https."
	default:
		return "proteos: a git credential is temporarily unavailable; try again shortly."
	}
}
