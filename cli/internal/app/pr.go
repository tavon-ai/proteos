package app

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/tavon-ai/proteos/cli/internal/client"
)

func runPR(env Env, args []string) int {
	if groupHelp(args) {
		prGroupUsage(env.Stdout)
		return client.ExitOK
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "view":
		return prView(env, rest)
	case "files":
		return prFiles(env, rest)
	case "checks":
		return prChecks(env, rest)
	case "merge":
		return prMerge(env, rest)
	case "comment":
		return prComment(env, rest)
	default:
		fmt.Fprintf(env.Stderr, "proteos: unknown pr subcommand %q\n\n", sub)
		prGroupUsage(env.Stderr)
		return client.ExitUsage
	}
}

// prGroupUsage explains the pr command family.
func prGroupUsage(w io.Writer) {
	fmt.Fprint(w, `proteos pr — review a pull request from the command line

These act on a pull request directly through GitHub — no machine required, so a
PR stays reviewable and mergeable while its machine is stopped. Use the full
name (owner/repo) from 'proteos repo ls'.

Commands:
  view <r> <n>            Show a PR's summary
  files <r> <n>           List a PR's changed files
  checks <r> <n>          Show a PR's check-run summary
  merge <r> <n>           Merge a PR
  comment -m <s> <r> <n>  Post a comment on a PR

Flags must come before the <owner/repo> <number> positional arguments.

Reads accept --json. Run 'proteos pr <command> -h' for that command's flags and
examples.
`)
}

// splitFullName splits an "owner/repo" full name. It reports ok=false for a
// missing slash or an empty owner/repo half.
func splitFullName(fullName string) (owner, repo string, ok bool) {
	i := strings.IndexByte(fullName, '/')
	if i <= 0 || i == len(fullName)-1 {
		return "", "", false
	}
	return fullName[:i], fullName[i+1:], true
}

// prArgs parses the shared <owner/repo> <number> positional arguments every pr
// subcommand takes.
func prArgs(fs *flag.FlagSet) (owner, repo string, number int, ok bool) {
	if fs.NArg() < 2 {
		return "", "", 0, false
	}
	owner, repo, ok = splitFullName(fs.Arg(0))
	if !ok {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(fs.Arg(1))
	if err != nil || n <= 0 {
		return "", "", 0, false
	}
	return owner, repo, n, true
}

func prView(env Env, args []string) int {
	fs := cmdFlags(env, "pr view", cmdHelp{
		summary: "Show a pull request's summary.",
		usage:   "proteos pr view [--json] <owner/repo> <number>",
		examples: []string{
			"proteos pr view octocat/hello-world 42",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	owner, repo, number, ok := prArgs(fs)
	if !ok {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	pr, err := c.GetPR(ctx(), owner, repo, number)
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, pr); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "#%d %s [%s]\n", pr.Number, pr.Title, pr.State)
	fmt.Fprintf(env.Stdout, "%s -> %s (by @%s)\n", pr.Head, pr.Base, pr.Author.Login)
	fmt.Fprintf(env.Stdout, "+%d -%d, %d files changed\n", pr.Additions, pr.Deletions, pr.ChangedFiles)
	fmt.Fprintln(env.Stdout, pr.HTMLURL)
	if pr.Body != "" {
		fmt.Fprintf(env.Stdout, "\n%s\n", pr.Body)
	}
	return client.ExitOK
}

func prFiles(env Env, args []string) int {
	fs := cmdFlags(env, "pr files", cmdHelp{
		summary: "List a pull request's changed files.",
		usage:   "proteos pr files [--json] <owner/repo> <number>",
		examples: []string{
			"proteos pr files octocat/hello-world 42",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	asJSON := fs.Bool("json", false, "emit raw JSON (includes each file's patch)")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	owner, repo, number, ok := prArgs(fs)
	if !ok {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	res, err := c.ListPRFiles(ctx(), owner, repo, number)
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, res); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	if len(res.Files) == 0 {
		fmt.Fprintln(env.Stdout, "No files changed.")
		return client.ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\t+\t-\tPATH")
	for _, f := range res.Files {
		path := f.Path
		if f.PrevPath != "" {
			path = f.PrevPath + " -> " + f.Path
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", f.Status, f.Additions, f.Deletions, path)
	}
	tw.Flush()
	return client.ExitOK
}

func prChecks(env Env, args []string) int {
	fs := cmdFlags(env, "pr checks", cmdHelp{
		summary: "Show a pull request's check-run summary.",
		usage:   "proteos pr checks [--json] <owner/repo> <number>",
		examples: []string{
			"proteos pr checks octocat/hello-world 42",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	owner, repo, number, ok := prArgs(fs)
	if !ok {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	res, err := c.ListPRChecks(ctx(), owner, repo, number)
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, res); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "%d total: %d passed, %d failed, %d pending\n", res.Total, res.Passed, res.Failed, res.Pending)
	if len(res.Runs) == 0 {
		return client.ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tCONCLUSION")
	for _, run := range res.Runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", run.Name, run.Status, run.Conclusion)
	}
	tw.Flush()
	return client.ExitOK
}

func prMerge(env Env, args []string) int {
	fs := cmdFlags(env, "pr merge", cmdHelp{
		summary: "Merge a pull request.",
		long:    "Method defaults to a merge commit; use --method to squash or rebase instead.",
		usage:   "proteos pr merge [--method merge|squash|rebase] [--json] <owner/repo> <number>",
		examples: []string{
			"proteos pr merge octocat/hello-world 42",
			"proteos pr merge --method squash octocat/hello-world 42",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	method := fs.String("method", "merge", "merge method: merge, squash, or rebase")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	owner, repo, number, ok := prArgs(fs)
	if !ok {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	res, err := c.MergePR(ctx(), owner, repo, number, *method)
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, res); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "merged %s/%s#%d (%s)\n", owner, repo, number, res.SHA)
	return client.ExitOK
}

func prComment(env Env, args []string) int {
	fs := cmdFlags(env, "pr comment", cmdHelp{
		summary: "Post a comment on a pull request.",
		usage:   `proteos pr comment -m "<text>" [--json] <owner/repo> <number>`,
		examples: []string{
			`proteos pr comment -m "LGTM" octocat/hello-world 42`,
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	body := fs.String("m", "", "comment body (required)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	owner, repo, number, ok := prArgs(fs)
	if !ok || *body == "" {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	res, err := c.CommentPR(ctx(), owner, repo, number, *body)
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, res); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "commented on %s/%s#%d: %s\n", owner, repo, number, res.HTMLURL)
	return client.ExitOK
}
