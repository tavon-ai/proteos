package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/tavon-ai/proteos/cli/internal/client"
)

// defaultSSHUser is the account a guest session lands as: build-rootfs.sh's
// RUN_AS_USER. An image built --no-user has no such account and sshd falls back
// to root (still key-only), so --user exists as the escape hatch.
const defaultSSHUser = "dev"

// runSSH implements `proteos ssh <machine>`: a convenience wrapper that runs the
// local ssh(1) with the ProxyCommand already wired to `proteos ssh-proxy`. There
// is no inbound port to reach a machine on — the guest's sshd binds loopback
// only and the sole path in is the authenticated control-plane tunnel (see
// docs/plans/ssh-access.md §3.4) — so every SSH client must go through a
// ProxyCommand. This just saves typing it.
func runSSH(env Env, args []string) int {
	fs := cmdFlags(env, "ssh", cmdHelp{
		summary: "Open an SSH session to one of your machines.",
		long: "Runs your local ssh(1) with ProxyCommand pointed at 'proteos ssh-proxy',\n" +
			"which tunnels the connection through the control plane. Machines have no\n" +
			"inbound port of their own, so this (or an equivalent ProxyCommand) is the\n" +
			"only way in. Requires SSH to be enabled for your account and at least one\n" +
			"registered login key — see 'Settings → SSH access' in the web UI.\n\n" +
			"Everything after the machine id is passed straight through to ssh, so\n" +
			"remote commands and extra ssh flags work as usual. This command's own\n" +
			"flags therefore have to come BEFORE the machine id.",
		usage: "proteos ssh [--user NAME] [--print] <machine> [ssh args...]",
		examples: []string{
			"proteos ssh m-123",
			"proteos ssh m-123 -- uname -a",
			"proteos ssh --print m-123   # show the raw ssh command instead of running it",
			"proteos ssh --user root m-123",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	user := fs.String("user", defaultSSHUser, "remote user to log in as")
	printOnly := fs.Bool("print", false, "print the equivalent ssh command instead of running it")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return client.ExitUsage
	}
	machine := fs.Arg(0)

	// Resolve config here (rather than only in the child) so a missing endpoint
	// or token is reported by this command, with this command's exit codes,
	// instead of surfacing as an opaque ssh "proxy command failed".
	_, resolved, code, ok := newClient(env, *url)
	if !ok {
		return code
	}

	// Everything after the machine id goes to ssh. A leading "--" is the shell
	// idiom for "the rest is the remote command" and is how this command's own
	// examples are written, but ssh has no such separator after the destination
	// (it would run "--" as the command), so drop it here.
	passthrough := fs.Args()[1:]
	if len(passthrough) > 0 && passthrough[0] == "--" {
		passthrough = passthrough[1:]
	}
	// Pin the endpoint this command resolved into the ProxyCommand rather than
	// only forwarding an explicit --url, so the child resolves the same control
	// plane even if its environment or stored config would say otherwise.
	sshArgs := append(sshProxyArgs(resolved.BaseURL, machine, *user), passthrough...)
	if *printOnly {
		fmt.Fprintln(env.Stdout, "ssh "+strings.Join(sshArgs, " "))
		return client.ExitOK
	}

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		writeErrorMsg(env, "no_ssh_client", "no ssh(1) found on PATH — install an SSH client, or use 'proteos ssh --print' to get the command for your own client")
		return client.ExitError
	}

	cmd := exec.Command(sshBin, sshArgs...)
	// Hand ssh the real terminal when there is one: an *os.File passes through as
	// the process's own fd, which is what lets ssh allocate a pty, prompt for a
	// key passphrase, and render an interactive shell. Anything else (a test
	// buffer) becomes an ordinary pipe.
	cmd.Stdin = stdinFile(env.Stdin)
	cmd.Stdout = stdioWriter(env.Stdout, os.Stdout)
	cmd.Stderr = stdioWriter(env.Stderr, os.Stderr)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// ssh already explained itself on stderr; propagate its exit status so
			// scripts see what they would have seen running ssh directly.
			return ee.ExitCode()
		}
		return fail(env, fmt.Errorf("run ssh: %w", err))
	}
	return client.ExitOK
}

// sshProxyArgs builds the ssh(1) argument list: the ProxyCommand that tunnels
// through the control plane, the login user, and the machine id as the host.
// Using the machine id as the hostname means known_hosts entries are per
// machine, which lines up with each guest persisting its own host key across
// stop/start (guestagent/internal/persist).
func sshProxyArgs(baseURL, machine, user string) []string {
	proxy := shellQuote(selfPath()) + " ssh-proxy"
	if baseURL != "" {
		proxy += " --url " + shellQuote(baseURL)
	}
	proxy += " " + shellQuote(machine)
	return []string{"-o", "ProxyCommand=" + proxy, "-l", user, machine}
}

// selfPath returns this executable's path for embedding in ProxyCommand,
// falling back to the bare program name (resolved via PATH by the shell ssh
// runs the proxy command under) if the OS can't tell us.
func selfPath() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "proteos"
	}
	return exe
}

// shellQuote single-quotes s for the /bin/sh that ssh runs ProxyCommand under,
// so a path containing spaces survives.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`&|;<>()*?[]#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runSSHProxy implements `proteos ssh-proxy <machine>`: the ProxyCommand helper.
// It opens the control plane's /gw/ssh WebSocket and bridges it to stdin/stdout
// as a plain byte stream, which is exactly the contract ssh(1) expects from a
// ProxyCommand. Useful directly in ~/.ssh/config or an IDE's remote-SSH setup:
//
//	Host my-machine
//	    User dev
//	    ProxyCommand proteos ssh-proxy <machine-id>
func runSSHProxy(env Env, args []string) int {
	fs := cmdFlags(env, "ssh-proxy", cmdHelp{
		summary: "Bridge stdin/stdout to a machine's SSH server (for ssh ProxyCommand).",
		long: "Not usually run by hand — it is the transport 'proteos ssh' and any\n" +
			"ssh_config ProxyCommand entry use to reach a machine through the control\n" +
			"plane. It speaks raw bytes on stdin/stdout and writes nothing else to\n" +
			"stdout, so it composes with ssh, scp, sftp, rsync -e ssh, and IDE\n" +
			"remote-SSH features.",
		usage: "proteos ssh-proxy [--url URL] <machine>",
		examples: []string{
			"proteos ssh-proxy m-123",
			`ssh -o ProxyCommand="proteos ssh-proxy m-123" dev@m-123`,
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return client.ExitUsage
	}

	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	conn, err := c.DialSSH(ctx(), fs.Arg(0))
	if err != nil {
		return fail(env, err)
	}
	defer conn.Close()

	// Bridge both directions and stop as soon as either ends. The upstream copy
	// (stdin → tunnel) can block indefinitely on a stdin that never closes, so
	// only the downstream copy gates the return: when the tunnel closes, the
	// session is over regardless of what stdin is doing.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, env.Stdin)
		_ = conn.Close()
	}()
	if _, err := io.Copy(env.Stdout, conn); err != nil {
		return fail(env, fmt.Errorf("ssh tunnel: %w", err))
	}
	return client.ExitOK
}

// stdinFile picks what to hand a child process as stdin. An *os.File (the
// normal case — main wires env.Stdin to os.Stdin) is passed through so the child
// inherits the real fd and can drive the terminal; a nil or non-file reader
// yields whatever it is, which exec turns into a pipe.
func stdinFile(r io.Reader) io.Reader {
	if r == nil {
		return os.Stdin
	}
	return r
}

// stdioWriter is stdinFile's counterpart for the output streams.
func stdioWriter(w io.Writer, fallback *os.File) io.Writer {
	if w == nil {
		return fallback
	}
	return w
}
