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

	"github.com/tavon/proteos/cli/internal/client"
)

func runTask(env Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "usage: proteos task <run|ls|get|watch|cancel|send> [flags]")
		return client.ExitUsage
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
		fmt.Fprintf(env.Stderr, "proteos: unknown task subcommand %q\n", sub)
		return client.ExitUsage
	}
}

// taskRun dispatches a headless agent task. The prompt comes from the positional
// arg, --prompt-file, or stdin ("-"). --wait polls to terminal; --watch streams.
func taskRun(env Env, args []string) int {
	fs := flagSet(env, "task run")
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	provider := fs.String("provider", "claude", "agent provider")
	project := fs.String("project", "", "project under /workspace (required)")
	promptFile := fs.String("prompt-file", "", "read the prompt from a file ('-' for stdin)")
	wait := fs.Bool("wait", false, "poll until the task reaches a terminal state")
	watch := fs.Bool("watch", false, "stream the task's live events until it ends")
	timeout := fs.Duration("timeout", 30*time.Minute, "max time to wait/watch")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if *machineID == "" || *project == "" {
		fmt.Fprintln(env.Stderr, "usage: proteos task run --machine <id> --project <name> [--provider claude] \"<prompt>\"")
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
	fs := flagSet(env, "task ls")
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if *machineID == "" {
		fmt.Fprintln(env.Stderr, "usage: proteos task ls --machine <id> [--json]")
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
	fs := flagSet(env, "task get")
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if *machineID == "" || fs.NArg() < 1 {
		fmt.Fprintln(env.Stderr, "usage: proteos task get --machine <id> <tid> [--json]")
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
	fs := flagSet(env, "task watch")
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	timeout := fs.Duration("timeout", 30*time.Minute, "max time to watch")
	asJSON := fs.Bool("json", false, "emit normalized events as NDJSON")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if *machineID == "" || fs.NArg() < 1 {
		fmt.Fprintln(env.Stderr, "usage: proteos task watch --machine <id> <tid> [--json]")
		return client.ExitUsage
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	return watchTask(env, c, *machineID, fs.Arg(0), *timeout, *asJSON)
}

func taskCancel(env Env, args []string) int {
	fs := flagSet(env, "task cancel")
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	allRunning := fs.Bool("all-running", false, "cancel every running/queued task on the machine")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if *machineID == "" {
		fmt.Fprintln(env.Stderr, "usage: proteos task cancel --machine <id> <tid> | --all-running")
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
		fmt.Fprintln(env.Stderr, "usage: proteos task cancel --machine <id> <tid>")
		return client.ExitUsage
	}
	if _, err := c.CancelTask(ctx(), *machineID, fs.Arg(0)); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "cancel requested for %s\n", fs.Arg(0))
	return client.ExitOK
}

func taskSend(env Env, args []string) int {
	fs := flagSet(env, "task send")
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	promptFile := fs.String("prompt-file", "", "read the prompt from a file ('-' for stdin)")
	wait := fs.Bool("wait", false, "poll until the turn reaches a terminal state")
	watch := fs.Bool("watch", false, "stream the turn's live events until it ends")
	timeout := fs.Duration("timeout", 30*time.Minute, "max time to wait/watch")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if *machineID == "" || fs.NArg() < 1 {
		fmt.Fprintln(env.Stderr, "usage: proteos task send --machine <id> <tid> \"<prompt>\"")
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
