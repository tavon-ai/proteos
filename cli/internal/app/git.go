package app

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/tavon-ai/proteos/cli/internal/client"
)

func runGit(env Env, args []string) int {
	if groupHelp(args) {
		gitGroupUsage(env.Stdout)
		return client.ExitOK
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "status":
		return gitStatus(env, rest)
	case "diff":
		return gitDiff(env, rest)
	case "branch":
		return gitBranch(env, rest)
	case "commit":
		return gitCommit(env, rest)
	case "push":
		return gitPush(env, rest)
	case "pr":
		return gitPR(env, rest)
	default:
		fmt.Fprintf(env.Stderr, "proteos: unknown git subcommand %q\n\n", sub)
		gitGroupUsage(env.Stderr)
		return client.ExitUsage
	}
}

// gitGroupUsage explains the git command family — source control on a project.
func gitGroupUsage(w io.Writer) {
	fmt.Fprint(w, `proteos git — source control on a project in a machine

A task run leaves a dirty working tree but never commits. These commands are the
explicit review → commit → push → PR path over that tree, run against a project
cloned on a machine.

Commands:
  status           Show the working-tree change set
  diff             Show the unified diff (--staged for the index diff)
  branch <name>    Create (and by default checkout) a branch
  commit -m <msg>  Stage and commit changes (all, or the given paths)
  push             Push a branch to origin
  pr               Open a pull request

Every command needs --machine <id> and --project <name>.
Run 'proteos git <command> -h' for that command's flags and examples.
`)
}

func gitStatus(env Env, args []string) int {
	fs := cmdFlags(env, "git status", cmdHelp{
		summary: "Show a project's working-tree change set.",
		usage:   "proteos git status --machine <id> --project <name> [--json]",
		examples: []string{
			"proteos git status --machine m-123 --project myrepo",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	project := fs.String("project", "", "project directory under /workspace (required)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if *machineID == "" || *project == "" {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	st, err := c.GitStatusOf(ctx(), *machineID, *project)
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, st); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "On branch %s\n", st.Branch)
	if len(st.Files) == 0 {
		fmt.Fprintln(env.Stdout, "Working tree clean.")
		return client.ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "INDEX\tWORKTREE\tPATH")
	for _, f := range st.Files {
		path := f.Path
		if f.Orig != "" {
			path = f.Orig + " -> " + f.Path
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", f.Index, f.Worktree, path)
	}
	tw.Flush()
	return client.ExitOK
}

func gitDiff(env Env, args []string) int {
	fs := cmdFlags(env, "git diff", cmdHelp{
		summary: "Show a project's unified diff.",
		long:    "Shows the worktree diff by default; --staged shows the index (staged) diff.\nLarge diffs are truncated by the server (a note is printed when that happens).",
		usage:   "proteos git diff --machine <id> --project <name> [--staged] [--json]",
		examples: []string{
			"proteos git diff --machine m-123 --project myrepo",
			"proteos git diff --machine m-123 --project myrepo --staged",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	project := fs.String("project", "", "project directory under /workspace (required)")
	staged := fs.Bool("staged", false, "show the staged (index) diff instead of the worktree diff")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if *machineID == "" || *project == "" {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	d, err := c.GitDiffOf(ctx(), *machineID, *project, *staged)
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, d); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprint(env.Stdout, d.Diff)
	if d.Truncated {
		fmt.Fprintln(env.Stderr, "proteos: diff truncated (too large to show in full)")
	}
	return client.ExitOK
}

func gitBranch(env Env, args []string) int {
	fs := cmdFlags(env, "git branch", cmdHelp{
		summary: "Create a branch in a project (and check it out by default).",
		usage:   "proteos git branch --machine <id> --project <name> [--from <ref>] [--no-checkout] <name>",
		examples: []string{
			"proteos git branch --machine m-123 --project myrepo fix/login",
			"proteos git branch --machine m-123 --project myrepo --from main feature/x",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	project := fs.String("project", "", "project directory under /workspace (required)")
	from := fs.String("from", "", "start point (branch, tag, or sha; defaults to current HEAD)")
	noCheckout := fs.Bool("no-checkout", false, "create the branch without switching to it")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if *machineID == "" || *project == "" || fs.NArg() < 1 {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	branch, err := c.GitBranch(ctx(), *machineID, client.GitBranchRequest{
		Project: *project, Name: fs.Arg(0), Checkout: !*noCheckout, From: *from,
	})
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, struct {
			Branch string `json:"branch"`
		}{branch}); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "on branch %s\n", branch)
	return client.ExitOK
}

func gitCommit(env Env, args []string) int {
	fs := cmdFlags(env, "git commit", cmdHelp{
		summary: "Stage and commit a project's changes.",
		long:    "Commits all changes by default, or only the given repo-relative paths.\nThis is the explicit review gate — the agent never commits on its own.",
		usage:   `proteos git commit --machine <id> --project <name> -m "<message>" [paths...]`,
		examples: []string{
			`proteos git commit --machine m-123 --project myrepo -m "add health check"`,
			`proteos git commit --machine m-123 --project myrepo -m "fix" main.go util.go`,
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	project := fs.String("project", "", "project directory under /workspace (required)")
	message := fs.String("m", "", "commit message (required)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if *machineID == "" || *project == "" || *message == "" {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	res, err := c.GitCommit(ctx(), *machineID, client.GitCommitRequest{
		Project: *project, Message: *message, Paths: fs.Args(),
	})
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, res); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "committed %s %s\n", res.Sha, res.Subject)
	return client.ExitOK
}

func gitPush(env Env, args []string) int {
	fs := cmdFlags(env, "git push", cmdHelp{
		summary: "Push a project's branch to origin.",
		long:    "Asynchronous: returns the op id once the push is dispatched. Use\n--set-upstream on the first push of a new branch.",
		usage:   "proteos git push --machine <id> --project <name> --branch <b> [--set-upstream]",
		examples: []string{
			"proteos git push --machine m-123 --project myrepo --branch fix/login --set-upstream",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	project := fs.String("project", "", "project directory under /workspace (required)")
	branch := fs.String("branch", "", "branch to push (required)")
	setUpstream := fs.Bool("set-upstream", false, "set the upstream (-u) on first push")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if *machineID == "" || *project == "" || *branch == "" {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	opID, err := c.GitPush(ctx(), *machineID, client.GitPushRequest{
		Project: *project, Branch: *branch, SetUpstream: *setUpstream,
	})
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, struct {
			OpID string `json:"op_id"`
		}{opID}); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "push of %s dispatched (op %s)\n", *branch, opID)
	return client.ExitOK
}

func gitPR(env Env, args []string) int {
	fs := cmdFlags(env, "git pr", cmdHelp{
		summary: "Open a pull request for a project.",
		long:    "Opens a PR from --head into --base (base defaults to the repo's default\nbranch). The head branch must already be pushed.",
		usage:   `proteos git pr --machine <id> --project <name> --head <b> --title "<t>" [--base <b>] [--body "<s>"]`,
		examples: []string{
			`proteos git pr --machine m-123 --project myrepo --head fix/login --title "Fix login"`,
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	project := fs.String("project", "", "project directory under /workspace (required)")
	head := fs.String("head", "", "branch with the changes (required)")
	base := fs.String("base", "", "target branch (defaults to the repo's default branch)")
	title := fs.String("title", "", "PR title (required)")
	body := fs.String("body", "", "PR body")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if *machineID == "" || *project == "" || *head == "" || *title == "" {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	pr, err := c.GitPR(ctx(), *machineID, client.GitPRRequest{
		Project: *project, Title: *title, Body: *body, Head: *head, Base: *base,
	})
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, pr); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "opened PR #%d: %s\n", pr.Number, pr.PRURL)
	return client.ExitOK
}
