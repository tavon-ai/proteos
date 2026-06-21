package app

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/tavon/proteos/cli/internal/client"
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
	default:
		fmt.Fprintf(env.Stderr, "proteos: unknown machines subcommand %q\n\n", sub)
		machinesGroupUsage(env.Stderr)
		return client.ExitUsage
	}
}

// machinesGroupUsage explains the machines command family.
func machinesGroupUsage(w io.Writer) {
	fmt.Fprint(w, `proteos machines — inspect your machines

A task runs inside one of your machines, so most task commands need its id; list
your machines here to find it.

Commands:
  ls               List your machines (id, name, state)
  get <id>         Show one machine

Reads accept --json. Run 'proteos machines <command> -h' for flags.
`)
}

func machinesList(env Env, args []string) int {
	fs := cmdFlags(env, "machines ls", cmdHelp{
		summary:  "List your machines (id, name, state).",
		usage:    "proteos machines ls [--json]",
		examples: []string{"proteos machines ls", "proteos machines ls --json"},
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
	machines, err := c.ListMachines(ctx())
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, machines); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	if len(machines) == 0 {
		fmt.Fprintln(env.Stdout, "No machines.")
		return client.ExitOK
	}
	labels := templateLabels(c)
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tTEMPLATE")
	for _, m := range machines {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.ID, m.Name, m.State, templateName(m.TemplateID, labels))
	}
	tw.Flush()
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
	if ok, code := parse(fs, args); !ok {
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
