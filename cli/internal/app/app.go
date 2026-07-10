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
	"strings"

	"github.com/tavon-ai/proteos/cli/internal/client"
	"github.com/tavon-ai/proteos/cli/internal/config"
)

// Env carries the process I/O and build metadata into the command tree, so tests
// can capture output and drive commands without touching os.Stdout/Args.
type Env struct {
	Stdin   io.Reader // defaults to nothing read; main wires this to os.Stdin
	Stdout  io.Writer
	Stderr  io.Writer
	Version string // semantic version, e.g. "v1.2.3" or "dev"
	Commit  string // git short sha the binary was built from
	Date    string // build timestamp (RFC 3339, UTC)

	// JSON is true when --json appeared anywhere in the invocation. It is
	// detected once in Run (before any per-command flag parsing) so every error
	// path — including ones before a command's own --json flag is parsed, like a
	// missing endpoint/token — can emit the machine-readable envelope instead of
	// prose. Commands that also use --json for success output read their own
	// parsed flag; this field only governs error formatting.
	JSON bool

	// describe, when non-nil, puts the command tree in introspection mode: each
	// leaf registers its flags as usual but parse() captures them and returns
	// before any server contact, so '--help-json' can dump the full tree offline.
	describe *describer
}

// Run dispatches args (os.Args[1:]) and returns the process exit code.
func Run(env Env, args []string) int {
	env.JSON = hasJSONFlag(args)
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
	case "providers", "provider":
		return runProviders(env, rest)
	case "secrets", "secret":
		return runSecrets(env, rest)
	case "version", "--version", "-v":
		printVersion(env)
		return client.ExitOK
	case "help", "--help", "-h":
		usage(env.Stdout)
		return client.ExitOK
	case "help-json", "--help-json":
		return emitHelpJSON(env)
	default:
		return unknownSubcommand(env, "command", cmd, usage)
	}
}

// hasJSONFlag reports whether args carries a --json/-json flag anywhere before
// the "--" end-of-flags terminator (matching Go's flag package, which stops
// recognizing flags there too). It recognizes the bare, "=true", and "=1"
// forms; "=false"/"=0" opt back out. This is a lightweight heuristic, not a
// full flag parse: it can't know which preceding flag (if any) a leaf's own
// FlagSet would consume "--json" as the *value* of, so a command whose flags
// take a following argument (e.g. --url --json) can still be misdetected —
// an accepted tradeoff for not needing every leaf's flag shape up front.
func hasJSONFlag(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "json" {
			continue
		}
		if !hasVal {
			return true
		}
		switch val {
		case "false", "0":
			return false
		default:
			return true
		}
	}
	return false
}

// unknownSubcommand reports an unrecognized command/subcommand: JSON envelope
// under --json, else the prose message followed by the group's usage text.
func unknownSubcommand(env Env, kind, name string, usage func(io.Writer)) int {
	if env.JSON {
		code := "unknown_" + strings.ReplaceAll(kind, " ", "_")
		writeErrorMsg(env, code, fmt.Sprintf("unknown %s %q", kind, name))
		return client.ExitUsage
	}
	fmt.Fprintf(env.Stderr, "proteos: unknown %s %q\n\n", kind, name)
	usage(env.Stderr)
	return client.ExitUsage
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
  machines create      Create a new machine from a template
  machines start <id>  Start a stopped machine
  machines stop <id>   Stop a running machine
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
  providers ls         List AI providers and whether a key is set for each
  providers get <key>  Show one provider
  secrets set <key>    Set (or replace) a provider's API key
  secrets unset <key>  Remove a provider's stored key
  version              Print the CLI version

Authentication:
  Mint a token in the browser under Settings → CLI tokens, then either run
  'proteos auth login' or set the PROTEOS_TOKEN environment variable. The
  endpoint comes from --url, PROTEOS_URL, or the stored login.

Run 'proteos <command> -h' for command-specific flags, or 'proteos --help-json'
for the whole command tree (flags included) as JSON — handy for tools and agents.
`)
}

// printVersion writes the build identity: version on the first line (so
// `proteos version | head -1` stays simple), then the commit and build date.
func printVersion(env Env) {
	fmt.Fprintf(env.Stdout, "proteos %s\n", orUnknown(env.Version))
	fmt.Fprintf(env.Stdout, "commit: %s\n", orUnknown(env.Commit))
	fmt.Fprintf(env.Stdout, "built:  %s\n", orUnknown(env.Date))
}

// orUnknown renders empty build metadata (e.g. an Env built without stamping) as
// "unknown" rather than a blank field.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// newClient resolves credentials (flag > env > file) and returns a ready client.
// It fails with a friendly, exit-coded message when the endpoint or token is
// missing so commands can `return` its second value directly.
func newClient(env Env, flagURL string) (*client.Client, config.Resolved, int, bool) {
	r, err := config.Resolve(flagURL)
	if err != nil {
		writeErrorMsg(env, "config_error", err.Error())
		return nil, r, client.ExitError, false
	}
	if r.BaseURL == "" {
		writeErrorMsg(env, "no_endpoint", "no endpoint configured — pass --url, set PROTEOS_URL, or run 'proteos auth login'")
		return nil, r, client.ExitUsage, false
	}
	if r.Token == "" {
		writeErrorMsg(env, "unauthenticated", "not authenticated — set PROTEOS_TOKEN or run 'proteos auth login' (mint a token under Settings → CLI tokens)")
		return nil, r, client.ExitAuth, false
	}
	return client.New(r.BaseURL, r.Token), r, client.ExitOK, true
}

// errorEnvelope is the --json error shape written to stderr: a stable,
// scriptable code plus a human-readable detail. It deliberately mirrors the
// control-plane's {error,detail} response envelope (see client.APIError) so
// CLI and API errors look the same to a machine consumer.
type errorEnvelope struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

// fail prints an error (mapping *APIError to a clean message, or the {error,
// detail} JSON envelope under --json) and returns the matching exit code.
func fail(env Env, err error) int {
	if env.JSON {
		writeErrorJSON(env, errorEnvelopeFor(err))
	} else {
		fmt.Fprintf(env.Stderr, "proteos: %v\n", err)
	}
	return client.ExitCodeFor(err)
}

// errorEnvelopeFor renders err as an errorEnvelope: an *APIError contributes
// its server-given code (or "http_<status>" if the server sent none) and
// detail; anything else becomes a generic "error" code with err's message as
// the detail.
func errorEnvelopeFor(err error) errorEnvelope {
	if ae, ok := errors.AsType[*client.APIError](err); ok {
		code := ae.Code
		if code == "" {
			code = fmt.Sprintf("http_%d", ae.Status)
		}
		return errorEnvelope{Error: code, Detail: ae.Detail}
	}
	return errorEnvelope{Error: "error", Detail: err.Error()}
}

// writeErrorMsg emits a code+message pair as the JSON envelope under --json,
// else "proteos: <message>" prose — the shared tail of every hand-rolled error
// site that isn't wrapping a client.APIError (fail handles those).
func writeErrorMsg(env Env, code, message string) {
	if env.JSON {
		writeErrorJSON(env, errorEnvelope{Error: code, Detail: message})
		return
	}
	fmt.Fprintf(env.Stderr, "proteos: %s\n", message)
}

// writeErrorJSON writes e to env.Stderr as JSON. Encoding failures here have no
// good recovery (we're already on the error path), so they're swallowed.
func writeErrorJSON(env Env, e errorEnvelope) {
	_ = printJSON(env.Stderr, e)
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
	if env.describe != nil {
		// Stash the help metadata so the next parse() call can pair it with the
		// fully-registered flag set when capturing the command for --help-json.
		env.describe.pendingName = name
		env.describe.pendingHelp = h
	}
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
//
// In describe mode (env.describe set) it captures the now-registered flag set for
// --help-json and returns (false, ExitOK) so the leaf returns before touching the
// server — every leaf calls parse right after registering its flags.
func parse(env Env, fs *flag.FlagSet, args []string) (bool, int) {
	if env.describe != nil {
		env.describe.capture(fs)
		return false, client.ExitOK
	}
	err := fs.Parse(args)
	if err == nil {
		return true, client.ExitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		return false, client.ExitOK
	}
	return false, client.ExitUsage
}
