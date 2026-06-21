// Package app implements the proteos CLI command tree: argument dispatch, the
// shared client/credential plumbing, and output helpers. main is a thin wrapper
// around Run so the whole CLI is testable in-process.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/tavon/proteos/cli/internal/client"
	"github.com/tavon/proteos/cli/internal/config"
)

// Env carries the process I/O and build metadata into the command tree, so tests
// can capture output and drive commands without touching os.Stdout/Args.
type Env struct {
	Stdout  io.Writer
	Stderr  io.Writer
	Version string
}

// Run dispatches args (os.Args[1:]) and returns the process exit code.
func Run(env Env, args []string) int {
	if len(args) == 0 {
		usage(env.Stderr)
		return client.ExitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "auth":
		return runAuth(env, rest)
	case "machines", "machine":
		return runMachines(env, rest)
	case "templates", "template":
		return runTemplates(env, rest)
	case "repo", "repos":
		return runRepos(env, rest)
	case "project", "projects":
		return runProjects(env, rest)
	case "git":
		return runGit(env, rest)
	case "task", "tasks":
		return runTask(env, rest)
	case "version", "--version", "-v":
		fmt.Fprintln(env.Stdout, env.Version)
		return client.ExitOK
	case "help", "--help", "-h":
		usage(env.Stdout)
		return client.ExitOK
	default:
		fmt.Fprintf(env.Stderr, "proteos: unknown command %q\n\n", cmd)
		usage(env.Stderr)
		return client.ExitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `proteos — drive the ProteOS Agent Task lane from the command line

Usage:
  proteos <command> [subcommand] [flags]

Commands:
  auth login           Store a personal access token for this CLI
  auth status          Show the current login / endpoint
  auth logout          Remove the stored credentials
  machines ls          List your machines (with their template/type)
  machines get <id>    Show one machine
  templates ls         List the machine templates (types) you can create
  repo ls              List the GitHub repos you can clone
  project ls           List the repos cloned in a machine's workspace
  project clone <r>    Clone owner/repo into a machine
  project ensure <r>   Clone owner/repo into a machine if not already present
  git status           Show a project's working-tree changes
  git diff             Show a project's diff
  git branch <name>    Create/checkout a branch in a project
  git commit -m <msg>  Commit a project's changes
  git push             Push a project's branch to origin
  git pr               Open a pull request for a project
  task run             Dispatch a headless agent task
  task ls              List a machine's tasks
  task get <tid>       Show one task
  task watch <tid>     Stream a task's live events
  task cancel <tid>    Cancel a running task
  task send <tid>      Send a follow-up turn to a task
  version              Print the CLI version

Authentication:
  Mint a token in the browser under Settings → CLI tokens, then either run
  'proteos auth login' or set the PROTEOS_TOKEN environment variable. The
  endpoint comes from --url, PROTEOS_URL, or the stored login.

Run 'proteos <command> -h' for command-specific flags.
`)
}

// newClient resolves credentials (flag > env > file) and returns a ready client.
// It fails with a friendly, exit-coded message when the endpoint or token is
// missing so commands can `return` its second value directly.
func newClient(env Env, flagURL string) (*client.Client, config.Resolved, int, bool) {
	r, err := config.Resolve(flagURL)
	if err != nil {
		fmt.Fprintf(env.Stderr, "proteos: %v\n", err)
		return nil, r, client.ExitError, false
	}
	if r.BaseURL == "" {
		fmt.Fprintln(env.Stderr, "proteos: no endpoint configured — pass --url, set PROTEOS_URL, or run 'proteos auth login'")
		return nil, r, client.ExitUsage, false
	}
	if r.Token == "" {
		fmt.Fprintln(env.Stderr, "proteos: not authenticated — set PROTEOS_TOKEN or run 'proteos auth login' (mint a token under Settings → CLI tokens)")
		return nil, r, client.ExitAuth, false
	}
	return client.New(r.BaseURL, r.Token), r, client.ExitOK, true
}

// fail prints an error (mapping *APIError to a clean message) and returns the
// matching exit code.
func fail(env Env, err error) int {
	fmt.Fprintf(env.Stderr, "proteos: %v\n", err)
	return client.ExitCodeFor(err)
}

// printJSON writes v as indented JSON.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printJSONLine writes v as a single compact JSON line (NDJSON).
func printJSONLine(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// ctx is the shared request context (placeholder for future cancellation wiring).
func ctx() context.Context { return context.Background() }

// cmdHelp describes a leaf command for its --help output.
type cmdHelp struct {
	summary  string   // one-line description
	long     string   // optional paragraph(s) of detail
	usage    string   // the invocation line
	examples []string // example invocations
}

// flagSet builds a FlagSet that prints to env.Stderr and suppresses the default
// "flag provided but not defined" double-printing.
func flagSet(env Env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

// cmdFlags builds a FlagSet whose -h/--help (and usage errors) print a full
// description, usage line, examples, and the flag list — not just flag defaults.
func cmdFlags(env Env, name string, h cmdHelp) *flag.FlagSet {
	fs := flagSet(env, name)
	fs.Usage = func() {
		out := fs.Output()
		if h.summary != "" {
			fmt.Fprintf(out, "%s\n", h.summary)
		}
		if h.long != "" {
			fmt.Fprintf(out, "\n%s\n", h.long)
		}
		if h.usage != "" {
			fmt.Fprintf(out, "\nUsage:\n  %s\n", h.usage)
		}
		if len(h.examples) > 0 {
			fmt.Fprintf(out, "\nExamples:\n")
			for _, e := range h.examples {
				fmt.Fprintf(out, "  %s\n", e)
			}
		}
		var hasFlags bool
		fs.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintf(out, "\nFlags:\n")
			fs.PrintDefaults()
		}
	}
	return fs
}

// groupHelp reports whether args is a request for a command group's help (no
// args, or help/-h/--help as the first token).
func groupHelp(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "help", "-h", "--help":
		return true
	}
	return false
}

// parse runs fs.Parse and maps the outcome to (ok, exit code): success → (true,
// 0); -h/-help → (false, ExitOK) since help is a successful, intentional request;
// any other parse error → (false, ExitUsage).
func parse(fs *flag.FlagSet, args []string) (bool, int) {
	err := fs.Parse(args)
	if err == nil {
		return true, client.ExitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		return false, client.ExitOK
	}
	return false, client.ExitUsage
}
