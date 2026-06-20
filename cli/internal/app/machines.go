package app

import (
	"fmt"
	"text/tabwriter"

	"github.com/tavon/proteos/cli/internal/client"
)

func runMachines(env Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "usage: proteos machines <ls|get> [flags]")
		return client.ExitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return machinesList(env, rest)
	case "get", "show":
		return machinesGet(env, rest)
	default:
		fmt.Fprintf(env.Stderr, "proteos: unknown machines subcommand %q\n", sub)
		return client.ExitUsage
	}
}

func machinesList(env Env, args []string) int {
	fs := flagSet(env, "machines ls")
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
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE")
	for _, m := range machines {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", m.ID, m.Name, m.State)
	}
	tw.Flush()
	return client.ExitOK
}

func machinesGet(env Env, args []string) int {
	fs := flagSet(env, "machines get")
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(env.Stderr, "usage: proteos machines get <id> [--json]")
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
	if m.GuestIP != nil && *m.GuestIP != "" {
		fmt.Fprintf(env.Stdout, "IP:    %s\n", *m.GuestIP)
	}
	if m.CreatedAt != "" {
		fmt.Fprintf(env.Stdout, "Created: %s\n", m.CreatedAt)
	}
	return client.ExitOK
}
