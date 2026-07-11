package app

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tavon-ai/proteos/cli/internal/client"
	"github.com/tavon-ai/proteos/cli/internal/config"
)

func runAuth(env Env, args []string) int {
	if groupHelp(args) {
		authGroupUsage(env.Stdout)
		return client.ExitOK
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "login":
		return authLogin(env, rest)
	case "status":
		return authStatus(env, rest)
	case "logout":
		return authLogout(env, rest)
	default:
		return unknownSubcommand(env, "auth subcommand", sub, authGroupUsage)
	}
}

// authGroupUsage explains how authentication works.
func authGroupUsage(w io.Writer) {
	fmt.Fprint(w, `proteos auth — authenticate the CLI

Mint a personal access token in the browser under Settings → CLI tokens, then
either run 'proteos auth login' to store it, or set PROTEOS_TOKEN (and PROTEOS_URL)
for a headless/agent run. The token is sent as a bearer credential and never
logged; stored credentials live in ~/.config/proteos/credentials.json (0600).

Commands:
  login    Verify a token against the endpoint and store it
  status   Show the resolved endpoint and login (never the token)
  logout   Remove the stored credentials

Run 'proteos auth <command> -h' for flags.
`)
}

// authLogin verifies a token against the endpoint and stores it. The token comes
// from --token, else PROTEOS_TOKEN, else stdin (so it stays out of shell history
// and argv). The endpoint comes from --url or PROTEOS_URL.
func authLogin(env Env, args []string) int {
	fs := cmdFlags(env, "auth login", cmdHelp{
		summary: "Verify a personal access token against the endpoint and store it.",
		long: "The token is read from --token, else PROTEOS_TOKEN, else an interactive\n" +
			"prompt (kept out of your shell history and argv). On success the endpoint,\n" +
			"token, and login are saved to ~/.config/proteos/credentials.json (0600).",
		usage: "proteos auth login --url <url> [--token <token>]",
		examples: []string{
			"proteos auth login --url https://proteos.example.com",
			"proteos auth login --url https://proteos.example.com --token proteos_pat_xxxx",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	tok := fs.String("token", "", "personal access token (or PROTEOS_TOKEN, or stdin)")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}

	baseURL := *url
	if baseURL == "" {
		baseURL = os.Getenv(config.EnvURL)
	}
	if baseURL == "" {
		fmt.Fprintln(env.Stderr, "proteos: --url (or PROTEOS_URL) is required")
		fmt.Fprintln(env.Stderr)
		fs.Usage()
		return client.ExitUsage
	}

	token := *tok
	if token == "" {
		token = os.Getenv(config.EnvToken)
	}
	if token == "" {
		fmt.Fprintln(env.Stderr, "Paste your personal access token (Settings → CLI tokens):")
		token = readLine(os.Stdin)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		fmt.Fprintln(env.Stderr, "proteos: no token provided")
		return client.ExitUsage
	}

	c := client.New(baseURL, token)
	me, err := c.Me(ctx())
	if err != nil {
		fmt.Fprintf(env.Stderr, "proteos: token verification failed: %v\n", err)
		return client.ExitCodeFor(err)
	}

	if err := config.Save(config.Credentials{BaseURL: c.BaseURL, Token: token, Login: me.User.Login}); err != nil {
		return fail(env, err)
	}
	p, _ := config.Path()
	fmt.Fprintf(env.Stdout, "Logged in as %s at %s\n", me.User.Login, c.BaseURL)
	fmt.Fprintf(env.Stdout, "Credentials saved to %s\n", p)
	return client.ExitOK
}

// authStatus reports the resolved endpoint, login, and where each value came
// from — without printing the token.
func authStatus(env Env, args []string) int {
	fs := cmdFlags(env, "auth status", cmdHelp{
		summary: "Show the resolved endpoint and login, and where each value came from.",
		long:    "Never prints the token itself — only whether one is set.",
		usage:   "proteos auth status",
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	r, err := config.Resolve(*url)
	if err != nil {
		return fail(env, err)
	}
	if r.BaseURL == "" && r.Token == "" {
		fmt.Fprintln(env.Stdout, "Not logged in.")
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "Endpoint: %s (from %s)\n", orNone(r.BaseURL), r.URLSource)
	fmt.Fprintf(env.Stdout, "Token:    %s (from %s)\n", tokenState(r.Token), r.TokSource)
	if r.Login != "" {
		fmt.Fprintf(env.Stdout, "Login:    %s\n", r.Login)
	}
	return client.ExitOK
}

// authLogout removes the stored credentials. It registers no flags, but still
// goes through cmdFlags/parse so -h prints help (instead of logging out) and
// --help-json can introspect it like every other leaf.
func authLogout(env Env, args []string) int {
	fs := cmdFlags(env, "auth logout", cmdHelp{
		summary: "Remove the stored credentials.",
		long:    "Deletes ~/.config/proteos/credentials.json. PROTEOS_TOKEN/PROTEOS_URL, if set,\nare untouched — unset those in your shell.",
		usage:   "proteos auth logout",
	})
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if err := config.Delete(); err != nil {
		return fail(env, err)
	}
	fmt.Fprintln(env.Stdout, "Logged out (stored credentials removed).")
	return client.ExitOK
}

func readLine(r io.Reader) string {
	s := bufio.NewScanner(r)
	if s.Scan() {
		return s.Text()
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func tokenState(s string) string {
	if s == "" {
		return "(none)"
	}
	return "set"
}
