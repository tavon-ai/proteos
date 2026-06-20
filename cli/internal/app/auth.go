package app

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tavon/proteos/cli/internal/client"
	"github.com/tavon/proteos/cli/internal/config"
)

func runAuth(env Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "usage: proteos auth <login|status|logout>")
		return client.ExitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "login":
		return authLogin(env, rest)
	case "status":
		return authStatus(env, rest)
	case "logout":
		return authLogout(env)
	default:
		fmt.Fprintf(env.Stderr, "proteos: unknown auth subcommand %q\n", sub)
		return client.ExitUsage
	}
}

// authLogin verifies a token against the endpoint and stores it. The token comes
// from --token, else PROTEOS_TOKEN, else stdin (so it stays out of shell history
// and argv). The endpoint comes from --url or PROTEOS_URL.
func authLogin(env Env, args []string) int {
	fs := flagSet(env, "auth login")
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	tok := fs.String("token", "", "personal access token (or PROTEOS_TOKEN, or stdin)")
	if ok, code := parse(fs, args); !ok {
		return code
	}

	baseURL := *url
	if baseURL == "" {
		baseURL = os.Getenv(config.EnvURL)
	}
	if baseURL == "" {
		fmt.Fprintln(env.Stderr, "proteos: --url (or PROTEOS_URL) is required")
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
	fs := flagSet(env, "auth status")
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	if ok, code := parse(fs, args); !ok {
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

func authLogout(env Env) int {
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
