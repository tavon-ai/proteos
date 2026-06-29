package app

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tavon-ai/proteos/cli/internal/client"
)

func runProjects(env Env, args []string) int {
	if groupHelp(args) {
		projectsGroupUsage(env.Stdout)
		return client.ExitOK
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return projectsList(env, rest)
	case "clone":
		return projectClone(env, rest)
	case "ensure":
		return projectEnsure(env, rest)
	default:
		fmt.Fprintf(env.Stderr, "proteos: unknown project subcommand %q\n\n", sub)
		projectsGroupUsage(env.Stderr)
		return client.ExitUsage
	}
}

// projectsGroupUsage explains the project command family.
func projectsGroupUsage(w io.Writer) {
	fmt.Fprint(w, `proteos project — manage the repositories cloned on a machine

A project is a repo cloned under /workspace on a machine. A task runs against a
project, so the typical agent flow is: 'project ensure' the repo onto the machine,
then 'task run --project <name>'.

Commands:
  ls               List the projects cloned on a machine
  clone <r>        Clone owner/repo onto a machine (async; --wait to block)
  ensure <r>       Clone owner/repo onto a machine only if not already present

Every command needs --machine <id>. Reads accept --json.
Run 'proteos project <command> -h' for that command's flags and examples.
`)
}

func projectsList(env Env, args []string) int {
	fs := cmdFlags(env, "project ls", cmdHelp{
		summary: "List the repositories cloned in a machine's workspace.",
		usage:   "proteos project ls --machine <id> [--json]",
		examples: []string{
			"proteos project ls --machine m-123",
			"proteos project ls --machine m-123 --json",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
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
	projects, err := c.ListProjects(ctx(), *machineID)
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, projects); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	if len(projects) == 0 {
		fmt.Fprintln(env.Stdout, "No projects.")
		return client.ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tBRANCH\tDIRTY\tREMOTE")
	for _, p := range projects {
		fmt.Fprintf(tw, "%s\t%s\t%t\t%s\n", p.Name, p.Branch, p.Dirty, p.Remote)
	}
	tw.Flush()
	return client.ExitOK
}

func projectClone(env Env, args []string) int {
	fs := cmdFlags(env, "project clone", cmdHelp{
		summary: "Clone owner/repo into a machine's workspace.",
		long: "The clone is asynchronous: by default the command returns once the clone is\n" +
			"dispatched. With --wait it polls until the repo appears in the project list\n" +
			"(or the timeout fires). Use 'proteos repo ls' to find the full name.",
		usage: "proteos project clone --machine <id> [--wait] <owner/repo>",
		examples: []string{
			"proteos project clone --machine m-123 octocat/hello-world",
			"proteos project clone --machine m-123 --wait octocat/hello-world",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	wait := fs.Bool("wait", false, "poll until the repo appears in the workspace")
	timeout := fs.Duration("timeout", 5*time.Minute, "max time to wait for the clone")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if *machineID == "" || fs.NArg() < 1 {
		fs.Usage()
		return client.ExitUsage
	}
	fullName := fs.Arg(0)
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	return cloneAndMaybeWait(env, c, *machineID, fullName, *wait, *timeout, *asJSON)
}

func projectEnsure(env Env, args []string) int {
	fs := cmdFlags(env, "project ensure", cmdHelp{
		summary: "Clone owner/repo into a machine only if it is not already present.",
		long: "The agent-friendly step before 'task run': it lists the machine's projects,\n" +
			"and if the repo is already there it returns immediately; otherwise it clones\n" +
			"and waits until the repo appears. Idempotent — safe to call every time.",
		usage: "proteos project ensure --machine <id> <owner/repo>",
		examples: []string{
			"proteos project ensure --machine m-123 octocat/hello-world",
			`proteos project ensure --machine m-123 octocat/hello-world && \` + "\n" +
				`    proteos task run --machine m-123 --project hello-world "fix the build"`,
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	machineID := fs.String("machine", "", "machine id (required)")
	timeout := fs.Duration("timeout", 5*time.Minute, "max time to wait for the clone")
	asJSON := fs.Bool("json", false, "emit raw JSON of the project once present")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if *machineID == "" || fs.NArg() < 1 {
		fs.Usage()
		return client.ExitUsage
	}
	fullName := fs.Arg(0)
	name := repoDir(fullName)
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	projects, err := c.ListProjects(ctx(), *machineID)
	if err != nil {
		return fail(env, err)
	}
	if p, found := findProject(projects, name); found {
		if *asJSON {
			if err := printJSON(env.Stdout, p); err != nil {
				return fail(env, err)
			}
		} else {
			fmt.Fprintf(env.Stdout, "project %s already present\n", name)
		}
		return client.ExitOK
	}
	return cloneAndMaybeWait(env, c, *machineID, fullName, true, *timeout, *asJSON)
}

// cloneAndMaybeWait dispatches a clone and, when wait is set, polls the project
// list until the repo appears. With --json (and a wait) it prints the resulting
// project; otherwise it prints progress lines.
func cloneAndMaybeWait(env Env, c *client.Client, machineID, fullName string, wait bool, timeout time.Duration, asJSON bool) int {
	opID, err := c.Clone(ctx(), machineID, fullName)
	if err != nil {
		return fail(env, err)
	}
	name := repoDir(fullName)
	if !wait {
		fmt.Fprintf(env.Stdout, "clone of %s dispatched (op %s)\n", fullName, opID)
		return client.ExitOK
	}
	p, code := waitForProject(env, c, machineID, name, timeout)
	if code != client.ExitOK {
		return code
	}
	if asJSON {
		if err := printJSON(env.Stdout, p); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "cloned %s into project %s\n", fullName, name)
	return client.ExitOK
}

// waitForProject polls the machine's project list until a project named name
// appears or the timeout fires. Clone failures surface as a timeout (a failed
// clone never adds the directory); the message names the repo so it is actionable.
func waitForProject(env Env, c *client.Client, machineID, name string, timeout time.Duration) (client.Project, int) {
	deadline := time.Now().Add(timeout)
	interval := time.Second
	const maxInterval = 5 * time.Second
	for {
		projects, err := c.ListProjects(ctx(), machineID)
		if err != nil {
			return client.Project{}, fail(env, err)
		}
		if p, found := findProject(projects, name); found {
			return p, client.ExitOK
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(env.Stderr, "proteos: timed out waiting for %s to clone (it may still be in progress or have failed)\n", name)
			return client.Project{}, client.ExitError
		}
		time.Sleep(interval)
		if interval *= 2; interval > maxInterval {
			interval = maxInterval
		}
	}
}

// findProject returns the project with the given directory name, if present.
func findProject(projects []client.Project, name string) (client.Project, bool) {
	for _, p := range projects {
		if p.Name == name {
			return p, true
		}
	}
	return client.Project{}, false
}

// repoDir is the workspace directory name for a repo full-name — the part after
// the last '/' (mirrors the control plane's clone destination).
func repoDir(fullName string) string {
	if i := strings.LastIndex(fullName, "/"); i >= 0 {
		return fullName[i+1:]
	}
	return fullName
}
