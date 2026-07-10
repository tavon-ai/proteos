package app

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/tavon-ai/proteos/cli/internal/client"
)

func runMachines(env Env, args []string) int {
	if groupHelp(args) {
		machinesGroupUsage(env.Stdout)
		return client.ExitOK
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return machinesList(env, rest)
	case "get", "show":
		return machinesGet(env, rest)
	case "create", "new":
		return machinesCreate(env, rest)
	case "start":
		return machinesStart(env, rest)
	case "stop":
		return machinesStop(env, rest)
	default:
		return unknownSubcommand(env, "machines subcommand", sub, machinesGroupUsage)
	}
}

// machinesGroupUsage explains the machines command family.
func machinesGroupUsage(w io.Writer) {
	fmt.Fprint(w, `proteos machines — manage your machines

A task runs inside one of your machines, so most task commands need its id; list
your machines here to find it, or create/start/stop them.

Commands:
  ls               List your machines (id, name, state)
  get <id>         Show one machine
  create           Create a new machine from a template
  start <id>       Start a stopped machine
  stop <id>        Stop a running machine

Reads accept --json. Run 'proteos machines <command> -h' for flags.
`)
}

func machinesList(env Env, args []string) int {
	fs := cmdFlags(env, "machines ls", cmdHelp{
		summary:  "List your machines (id, name, state).",
		usage:    "proteos machines ls [--json] [--limit N] [--offset N]",
		examples: []string{"proteos machines ls", "proteos machines ls --json", "proteos machines ls --limit 20 --offset 20"},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	limit, offset := paginationFlags(fs)
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	machines, err := c.ListMachines(ctx())
	if err != nil {
		return fail(env, err)
	}
	p := paginate(machines, *offset, *limit)
	if *asJSON {
		if err := printPageJSON(env.Stdout, p); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	if len(p.Items) == 0 {
		fmt.Fprintln(env.Stdout, "No machines.")
		return client.ExitOK
	}
	labels := templateLabels(c)
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tTEMPLATE")
	for _, m := range p.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.ID, m.Name, m.State, templateName(m.TemplateID, labels))
	}
	tw.Flush()
	printPageFooter(env.Stdout, p, "machines")
	return client.ExitOK
}

// templateLabels fetches the template catalog and maps id → label so machine
// listings can show a friendly type (full-stack, go, …). On any error it returns
// nil; callers then fall back to the raw template id.
func templateLabels(c *client.Client) map[string]string {
	ts, err := c.ListTemplates(ctx())
	if err != nil {
		return nil
	}
	m := make(map[string]string, len(ts))
	for _, t := range ts {
		m[t.ID] = t.Label
	}
	return m
}

// templateName renders a machine's template for display: the catalog label when
// known, else the raw id, else "-" when the machine has no template.
func templateName(id *string, labels map[string]string) string {
	if id == nil || *id == "" {
		return "-"
	}
	if label, ok := labels[*id]; ok && label != "" {
		return label
	}
	return *id
}

func machinesGet(env Env, args []string) int {
	fs := cmdFlags(env, "machines get", cmdHelp{
		summary:  "Show one machine by id.",
		usage:    "proteos machines get <id> [--json]",
		examples: []string{"proteos machines get m-123", "proteos machines get m-123 --json"},
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
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	m, err := c.GetMachine(ctx(), fs.Arg(0))
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, m); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "ID:    %s\n", m.ID)
	fmt.Fprintf(env.Stdout, "Name:  %s\n", m.Name)
	fmt.Fprintf(env.Stdout, "State: %s\n", m.State)
	if m.TemplateID != nil && *m.TemplateID != "" {
		fmt.Fprintf(env.Stdout, "Template: %s\n", templateName(m.TemplateID, templateLabels(c)))
	}
	if m.GuestIP != nil && *m.GuestIP != "" {
		fmt.Fprintf(env.Stdout, "IP:    %s\n", *m.GuestIP)
	}
	if m.CreatedAt != "" {
		fmt.Fprintf(env.Stdout, "Created: %s\n", m.CreatedAt)
	}
	return client.ExitOK
}

func machinesCreate(env Env, args []string) int {
	fs := cmdFlags(env, "machines create", cmdHelp{
		summary: "Create a new machine from a template.",
		long: "The template picks the machine type (image + default resources); list the\n" +
			"catalog with 'proteos templates ls'. The server boots the machine\n" +
			"asynchronously, so its state is usually still provisioning on return.",
		usage: "proteos machines create [--name <name>] [--template <id>] [flags]",
		examples: []string{
			"proteos machines create --template go --name my-box",
			"proteos machines create --template full-stack",
			"proteos machines create --template go --vcpus 4 --mem-mib 8192 --json",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	name := fs.String("name", "", "display name for the machine")
	template := fs.String("template", "", "template id to create from (see 'proteos templates ls')")
	vcpus := fs.Int("vcpus", 0, "override vCPU count (0 = template default)")
	memMiB := fs.Int("mem-mib", 0, "override memory in MiB (0 = template default)")
	diskMiB := fs.Int("disk-mib", 0, "override disk size in MiB (0 = template default)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	req := client.CreateMachineRequest{Name: *name, TemplateID: *template}
	if *vcpus > 0 {
		req.Vcpus = vcpus
	}
	if *memMiB > 0 {
		req.MemMiB = memMiB
	}
	if *diskMiB > 0 {
		req.DiskMiB = diskMiB
	}
	m, err := c.CreateMachine(ctx(), req)
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, m); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "machine %s created (%s)\n", m.ID, m.State)
	return client.ExitOK
}

func machinesStart(env Env, args []string) int {
	return machineStateChange(env, args, "start", "Start a stopped machine by id.", "started",
		func(c *client.Client, id string) (client.Machine, error) { return c.StartMachine(ctx(), id) })
}

func machinesStop(env Env, args []string) int {
	return machineStateChange(env, args, "stop", "Stop a running machine by id.", "stopped",
		func(c *client.Client, id string) (client.Machine, error) { return c.StopMachine(ctx(), id) })
}

// machineStateChange factors the shared shape of start/stop: parse the required
// <id> argument, invoke op, and print the updated machine (or --json). verb names
// the subcommand, done is the past-tense word for the success line.
func machineStateChange(env Env, args []string, verb, summary, done string, op func(*client.Client, string) (client.Machine, error)) int {
	fs := cmdFlags(env, "machines "+verb, cmdHelp{
		summary:  summary,
		usage:    "proteos machines " + verb + " <id> [--json]",
		examples: []string{"proteos machines " + verb + " m-123"},
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
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	m, err := op(c, fs.Arg(0))
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, m); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "machine %s %s (%s)\n", m.ID, done, m.State)
	return client.ExitOK
}
