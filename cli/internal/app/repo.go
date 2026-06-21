package app

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/tavon/proteos/cli/internal/client"
)

func runRepos(env Env, args []string) int {
	if groupHelp(args) {
		reposGroupUsage(env.Stdout)
		return client.ExitOK
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return reposList(env, rest)
	default:
		fmt.Fprintf(env.Stderr, "proteos: unknown repo subcommand %q\n\n", sub)
		reposGroupUsage(env.Stderr)
		return client.ExitUsage
	}
}

// reposGroupUsage explains the repo command family.
func reposGroupUsage(w io.Writer) {
	fmt.Fprint(w, `proteos repo — list the GitHub repositories you can clone

These are the repos you have granted ProteOS access to. Use the full name
(owner/repo) you see here with 'proteos project clone' / 'project ensure' to put
one onto a machine.

Commands:
  ls               List the repos (full name, private, default branch)

Reads accept --json. Run 'proteos repo <command> -h' for flags.
`)
}

func reposList(env Env, args []string) int {
	fs := cmdFlags(env, "repo ls", cmdHelp{
		summary:  "List the GitHub repositories you can clone.",
		long:     "These are the repos granted to ProteOS. If the list is empty, the output\npoints you at the page where you can grant access.",
		usage:    "proteos repo ls [--json]",
		examples: []string{"proteos repo ls", "proteos repo ls --json"},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	res, err := c.ListRepos(ctx())
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, res); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	if len(res.Repos) == 0 {
		fmt.Fprintln(env.Stdout, "No repositories granted.")
		if res.GrantsURL != "" {
			fmt.Fprintf(env.Stdout, "Grant access at: %s\n", res.GrantsURL)
		}
		return client.ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FULL NAME\tPRIVATE\tDEFAULT BRANCH")
	for _, r := range res.Repos {
		fmt.Fprintf(tw, "%s\t%t\t%s\n", r.FullName, r.Private, r.DefaultBranch)
	}
	tw.Flush()
	return client.ExitOK
}
