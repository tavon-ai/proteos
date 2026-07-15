package app

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/tavon-ai/proteos/cli/internal/client"
)

// runGitHosts is the `proteos git hosts` family (Gitea/Forgejo phase 2):
// per-user PATs for the additional git hosts the operator has allowlisted.
func runGitHosts(env Env, args []string) int {
	if groupHelp(args) {
		gitHostsGroupUsage(env.Stdout)
		return client.ExitOK
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return gitHostsList(env, rest)
	case "set-token":
		return gitHostsSetToken(env, rest)
	case "rm-token":
		return gitHostsRmToken(env, rest)
	default:
		return unknownSubcommand(env, "git hosts subcommand", sub, gitHostsGroupUsage)
	}
}

func gitHostsGroupUsage(w io.Writer) {
	fmt.Fprint(w, `proteos git hosts — tokens for additional git hosts (Gitea/Forgejo)

The operator allowlists extra git hosts (PROTEOS_GIT_PUBLIC_HOSTS); public
repos on them clone anonymously. Saving a Personal Access Token for a host
unlocks private repos, pushes, and pull requests there. The token is validated
against the host, stored server-side, and never shown again.

Commands:
  ls                 List the allowed hosts and whether you have a token saved
  set-token <host>   Save a token for a host (reads the token from stdin)
  rm-token <host>    Remove your token for a host

Run 'proteos git hosts <command> -h' for flags.
`)
}

func gitHostsList(env Env, args []string) int {
	fs := cmdFlags(env, "git hosts ls", cmdHelp{
		summary: "List the allowed additional git hosts and your token state.",
		usage:   "proteos git hosts ls [--json]",
		examples: []string{
			"proteos git hosts ls",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	hosts, err := c.ListGitHosts(ctx())
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, struct {
			Hosts []client.GitHost `json:"hosts"`
		}{Hosts: hosts}); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	if len(hosts) == 0 {
		fmt.Fprintln(env.Stdout, "No additional git hosts are configured on this server.")
		return client.ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tTOKEN\tLOGIN")
	for _, h := range hosts {
		state := "-"
		if h.Linked {
			state = "saved"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", h.Host, state, h.Login)
	}
	tw.Flush()
	return client.ExitOK
}

func gitHostsSetToken(env Env, args []string) int {
	fs := cmdFlags(env, "git hosts set-token", cmdHelp{
		summary: "Save a Personal Access Token for an allowed git host.",
		long: "The token is read from stdin (never from an argument — argv leaks into\n" +
			"shell history and process listings), validated against the host, and stored\n" +
			"server-side. On success the host reports the account the token belongs to.",
		usage: "proteos git hosts set-token <host>",
		examples: []string{
			"echo \"$GITEA_PAT\" | proteos git hosts set-token gitea.example.com",
			"proteos git hosts set-token gitea.example.com < token.txt",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return client.ExitUsage
	}
	host := fs.Arg(0)

	token, err := readTokenLine(env.Stdin)
	if err != nil || token == "" {
		writeErrorMsg(env, "bad_request", "no token on stdin — pipe it in, e.g.: echo \"$PAT\" | proteos git hosts set-token "+host)
		return client.ExitUsage
	}

	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	view, err := c.SetGitHostToken(ctx(), host, token)
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, view); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "token saved for %s (account: %s)\n", view.Host, view.Login)
	return client.ExitOK
}

func gitHostsRmToken(env Env, args []string) int {
	fs := cmdFlags(env, "git hosts rm-token", cmdHelp{
		summary: "Remove your stored token for a git host.",
		usage:   "proteos git hosts rm-token <host>",
		examples: []string{
			"proteos git hosts rm-token gitea.example.com",
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
	host := fs.Arg(0)
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	if err := c.DeleteGitHostToken(ctx(), host); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "token removed for %s\n", host)
	return client.ExitOK
}

// readTokenLine reads the first line from r, trimmed — the PAT. A nil reader
// (stdin not wired) reads as empty.
func readTokenLine(r io.Reader) (string, error) {
	if r == nil {
		return "", nil
	}
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		return "", sc.Err()
	}
	return strings.TrimSpace(sc.Text()), nil
}
