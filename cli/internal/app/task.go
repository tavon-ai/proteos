package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tavon-ai/proteos/cli/internal/client"
)

func runTask(env Env, args []string) int {
	if groupHelp(args) {
		taskGroupUsage(env.Stdout)
		return client.ExitOK
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "run":
		return taskRun(env, rest)
	case "ls", "list":
		return taskList(env, rest)
	case "get", "show":
		return taskGet(env, rest)
	case "watch":
		return taskWatch(env, rest)
	case "cancel":
		return taskCancel(env, rest)
	case "send", "message":
		return taskSend(env, rest)
	default:
		fmt.Fprintf(env.Stderr, "proteos: unknown task subcommand %q\n\n", sub)
		taskGroupUsage(env.Stderr)
		return client.ExitUsage
	}
}

// taskGroupUsage explains the task command family — the headless Agent Task lane.
func taskGroupUsage(w io.Writer) {
	fmt.Fprint(w, `proteos task — run and observe headless coding-agent tasks

A task hands a machine a natural-language prompt and runs a coding agent against a
repo cloned in its /workspace, non-interactively. The run produces a dirty working
tree and stops there — it never commits or pushes. You observe a task as a
first-class resource: poll its status, or stream its structured events (assistant
text, tool calls, tool results, final result).

Commands:
  run      Dispatch a task and (optionally) wait for or watch it
  ls       List a machine's tasks
  get      Show one task's status and result
  watch    Stream a task's live events until it ends
  cancel   Cancel a running task (or all running tasks on a machine)
  send     Send a follow-up turn that resumes a task's agent session

Every command needs --machine <id>. Reads accept --json for scripting.
Run 'proteos task <command> -h' for that command's flags and examples.

Exit codes: 0 ok · 2 usage · 3 auth · 4 not found · 5 task failed/canceled
`)
}

// taskRun dispatches a headless agent task. The prompt comes from the positional
// arg, --prompt-file, or stdin ("-"). --wait polls to terminal; --watch streams.
func taskRun(env Env, args []string) int {
	fs := cmdFlags(env, "task run", cmdHelp{
		summary: "Dispatch a headless agent task against a project in a machine.",
		long: "The prompt may be given as arguments, read from a file with --prompt-file,\n" +
			"or piped via '--prompt-file -'. Without --wait/--watch the command returns as\n" +
			"soon as the task is dispatched, printing its id. The agent leaves a dirty\n" +
			"working tree; it never commits.",
		usage: `proteos task run --machine <id> --project <name> [flags] "<prompt>"`,
		examples: []string{
			`proteos task run --machine m-123 --project myrepo "add a health check endpoint"`,
			`proteos task run --machine m-123 --project myrepo --watch "fix the failing test"`,
			`proteos task run --machine m-123 --project myrepo --wait --prompt-file task.md`,
			`git diff | proteos task run --machine m-123 --project myrepo --prompt-file - --watch`,
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	provider := fs.String("provider", "claude", "agent provider (headless lane: claude, pi)")
	project := fs.String("project", "", "project directory under /workspace (required)")
	promptFile := fs.String("prompt-file", "", "read the prompt from a file ('-' for stdin)")
	wait := fs.Bool("wait", false, "poll until the task reaches a terminal state, then print it")
	watch := fs.Bool("watch", false, "stream the task's live events until it ends")
	timeout := fs.Duration("timeout", 30*time.Minute, "max time to wait/watch")
	asJSON := fs.Bool("json", false, "with --wait/--watch, emit JSON instead of text")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if *machineID == "" || *project == "" {
		fs.Usage()
		return client.ExitUsage
	}
	prompt, err := readPrompt(fs.Args(), *promptFile)
	if err != nil {
		fmt.Fprintf(env.Stderr, "proteos: %v\n", err)
		return client.ExitUsage
	}
	if prompt == "" {
		fmt.Fprintln(env.Stderr, "proteos: empty prompt")
		return client.ExitUsage
	}

	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	taskID, err := c.CreateTask(ctx(), *machineID, client.CreateTaskRequest{
		Prompt: prompt, Provider: *provider, Project: *project,
	})
	if err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "task %s dispatched\n", taskID)

	switch {
	case *watch:
		return watchTask(env, c, *machineID, taskID, *timeout, *asJSON)
	case *wait:
		return waitTask(env, c, *machineID, taskID, *timeout, *asJSON)
	default:
		return client.ExitOK
	}
}

func taskList(env Env, args []string) int {
	fs := cmdFlags(env, "task ls", cmdHelp{
		summary: "List a machine's agent tasks, newest first.",
		usage:   "proteos task ls --machine <id> [--json]",
		examples: []string{
			"proteos task ls --machine m-123",
			"proteos task ls --machine m-123 --json",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if *machineID == "" {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	tasks, err := c.ListTasks(ctx(), *machineID)
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, tasks); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	if len(tasks) == 0 {
		fmt.Fprintln(env.Stdout, "No tasks.")
		return client.ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tPROVIDER\tPROJECT\tCREATED")
	for _, t := range tasks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.Status, t.Provider, t.Project, t.CreatedAt)
	}
	tw.Flush()
	return client.ExitOK
}

func taskGet(env Env, args []string) int {
	fs := cmdFlags(env, "task get", cmdHelp{
		summary: "Show one task's status and, when terminal, its result.",
		long:    "Result fields (session id, usage/cost, summary, error) appear once the task\nhas finished. Exits 5 if the task ended failed or canceled.",
		usage:   "proteos task get --machine <id> <task-id> [--json]",
		examples: []string{
			"proteos task get --machine m-123 t-456",
			"proteos task get --machine m-123 t-456 --json",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if *machineID == "" || fs.NArg() < 1 {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	t, err := c.GetTask(ctx(), *machineID, fs.Arg(0))
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, t); err != nil {
			return fail(env, err)
		}
	} else {
		printTask(env.Stdout, t)
	}
	if t.Failed() {
		return client.ExitTaskFail
	}
	return client.ExitOK
}

func taskWatch(env Env, args []string) int {
	fs := cmdFlags(env, "task watch", cmdHelp{
		summary: "Stream a task's live events until it reaches a terminal state.",
		long: "Renders assistant text, tool calls, tool results, and the final result as\n" +
			"they arrive, reconnecting automatically (Last-Event-ID) if the connection\n" +
			"drops. --json emits one normalized event per line (NDJSON) for an agent\n" +
			"consumer. Exits 5 if the task ends failed or canceled.",
		usage: "proteos task watch --machine <id> <task-id> [--json]",
		examples: []string{
			"proteos task watch --machine m-123 t-456",
			"proteos task watch --machine m-123 t-456 --json | jq .",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	timeout := fs.Duration("timeout", 30*time.Minute, "max time to watch")
	asJSON := fs.Bool("json", false, "emit normalized events as NDJSON")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if *machineID == "" || fs.NArg() < 1 {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	return watchTask(env, c, *machineID, fs.Arg(0), *timeout, *asJSON)
}

func taskCancel(env Env, args []string) int {
	fs := cmdFlags(env, "task cancel", cmdHelp{
		summary: "Cancel a running task (idempotent), or all running tasks on a machine.",
		long:    "Cancelling leaves whatever partial changes the agent made in the working\ntree for review. Cancelling an already-finished task is a no-op.",
		usage:   "proteos task cancel --machine <id> <task-id> | --all-running",
		examples: []string{
			"proteos task cancel --machine m-123 t-456",
			"proteos task cancel --machine m-123 --all-running",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	allRunning := fs.Bool("all-running", false, "cancel every running/queued task on the machine")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if *machineID == "" {
		fs.Usage()
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}

	if *allRunning {
		tasks, err := c.ListTasks(ctx(), *machineID)
		if err != nil {
			return fail(env, err)
		}
		n := 0
		for _, t := range tasks {
			if t.IsTerminal() {
				continue
			}
			if _, err := c.CancelTask(ctx(), *machineID, t.ID); err != nil {
				fmt.Fprintf(env.Stderr, "proteos: cancel %s: %v\n", t.ID, err)
				continue
			}
			fmt.Fprintf(env.Stdout, "canceled %s\n", t.ID)
			n++
		}
		fmt.Fprintf(env.Stdout, "%d task(s) canceled\n", n)
		return client.ExitOK
	}

	if fs.NArg() < 1 {
		fs.Usage()
		return client.ExitUsage
	}
	if _, err := c.CancelTask(ctx(), *machineID, fs.Arg(0)); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "cancel requested for %s\n", fs.Arg(0))
	return client.ExitOK
}

func taskSend(env Env, args []string) int {
	fs := cmdFlags(env, "task send", cmdHelp{
		summary: "Send a follow-up turn that resumes a finished task's agent session.",
		long: "Continues the same agent context (e.g. \"now also update the tests\") rather\n" +
			"than starting cold. Fails with no_session if the task never captured a\n" +
			"session, or task_running if a turn is still in flight. The prompt may come\n" +
			"from arguments, --prompt-file, or stdin ('-').",
		usage: `proteos task send --machine <id> <task-id> [flags] "<prompt>"`,
		examples: []string{
			`proteos task send --machine m-123 t-456 "now also update the docs"`,
			`proteos task send --machine m-123 t-456 --watch "address the review comments"`,
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	promptFile := fs.String("prompt-file", "", "read the prompt from a file ('-' for stdin)")
	wait := fs.Bool("wait", false, "poll until the turn reaches a terminal state, then print it")
	watch := fs.Bool("watch", false, "stream the turn's live events until it ends")
	timeout := fs.Duration("timeout", 30*time.Minute, "max time to wait/watch")
	asJSON := fs.Bool("json", false, "with --wait/--watch, emit JSON instead of text")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if *machineID == "" || fs.NArg() < 1 {
		fs.Usage()
		return client.ExitUsage
	}
	taskID := fs.Arg(0)
	prompt, err := readPrompt(fs.Args()[1:], *promptFile)
	if err != nil {
		fmt.Fprintf(env.Stderr, "proteos: %v\n", err)
		return client.ExitUsage
	}
	if prompt == "" {
		fmt.Fprintln(env.Stderr, "proteos: empty prompt")
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	if _, err := c.SendMessage(ctx(), *machineID, taskID, prompt); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "follow-up sent to %s\n", taskID)

	switch {
	case *watch:
		return watchTask(env, c, *machineID, taskID, *timeout, *asJSON)
	case *wait:
		return waitTask(env, c, *machineID, taskID, *timeout, *asJSON)
	default:
		return client.ExitOK
	}
}

// waitTask polls a task until it reaches a terminal state or the timeout fires.
func waitTask(env Env, c *client.Client, machineID, taskID string, timeout time.Duration, asJSON bool) int {
	deadline := time.Now().Add(timeout)
	interval := time.Second
	const maxInterval = 5 * time.Second
	for {
		t, err := c.GetTask(ctx(), machineID, taskID)
		if err != nil {
			return fail(env, err)
		}
		if t.IsTerminal() {
			if asJSON {
				_ = printJSON(env.Stdout, t)
			} else {
				printTask(env.Stdout, t)
			}
			if t.Failed() {
				return client.ExitTaskFail
			}
			return client.ExitOK
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(env.Stderr, "proteos: timed out waiting for task %s (last status: %s)\n", taskID, t.Status)
			return client.ExitError
		}
		time.Sleep(interval)
		if interval *= 2; interval > maxInterval {
			interval = maxInterval
		}
	}
}

// watchTask streams the task's live events to the terminal until it ends.
func watchTask(env Env, c *client.Client, machineID, taskID string, timeout time.Duration, asJSON bool) int {
	c2, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	exit := client.ExitOK
	err := c.StreamEvents(c2, machineID, taskID, "", func(id string, ev client.Event) error {
		if asJSON {
			return printJSONLine(env.Stdout, ev)
		}
		renderEvent(env.Stdout, ev)
		if ev.Kind == "result" && (ev.Status == "failed" || ev.Status == "canceled") {
			exit = client.ExitTaskFail
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintf(env.Stderr, "proteos: timed out watching task %s\n", taskID)
			return client.ExitError
		}
		return fail(env, err)
	}
	return exit
}

// readPrompt resolves the prompt from a file ('-' = stdin), else joins the args.
func readPrompt(args []string, promptFile string) (string, error) {
	if promptFile != "" {
		var b []byte
		var err error
		if promptFile == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(promptFile)
		}
		if err != nil {
			return "", fmt.Errorf("read prompt: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(strings.Join(args, " ")), nil
}

func printTask(w io.Writer, t client.Task) {
	fmt.Fprintf(w, "ID:       %s\n", t.ID)
	fmt.Fprintf(w, "Status:   %s\n", t.Status)
	fmt.Fprintf(w, "Provider: %s\n", t.Provider)
	fmt.Fprintf(w, "Project:  %s\n", t.Project)
	if t.SessionID != "" {
		fmt.Fprintf(w, "Session:  %s\n", t.SessionID)
	}
	if len(t.Usage) > 0 {
		fmt.Fprintf(w, "Usage:    %s\n", string(t.Usage))
	}
	if t.ResultSummary != "" {
		fmt.Fprintf(w, "Summary:  %s\n", t.ResultSummary)
	}
	if t.Error != "" {
		fmt.Fprintf(w, "Error:    %s\n", t.Error)
	}
}

// renderEvent prints one event in human form.
func renderEvent(w io.Writer, ev client.Event) {
	switch ev.Kind {
	case "assistant_text":
		if ev.Text != "" {
			fmt.Fprintln(w, ev.Text)
		}
	case "tool_use":
		if len(ev.Input) > 0 {
			fmt.Fprintf(w, "▸ %s %s\n", ev.Tool, string(ev.Input))
		} else {
			fmt.Fprintf(w, "▸ %s\n", ev.Tool)
		}
	case "tool_result":
		out := strings.TrimRight(ev.Output, "\n")
		if ev.IsError {
			fmt.Fprintf(w, "  ✗ %s\n", out)
		} else if out != "" {
			fmt.Fprintf(w, "  %s\n", out)
		}
	case "result":
		fmt.Fprintf(w, "— %s (cost $%.4f, %d turns, %dms)\n", ev.Status, ev.CostUSD, ev.NumTurns, ev.DurationMS)
		if ev.Error != "" {
			fmt.Fprintf(w, "  error: %s\n", ev.Error)
		}
	}
}
