package app

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/tavon/proteos/cli/internal/client"
)

func runTemplates(env Env, args []string) int {
	if groupHelp(args) {
		templatesGroupUsage(env.Stdout)
		return client.ExitOK
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return templatesList(env, rest)
	default:
		fmt.Fprintf(env.Stderr, "proteos: unknown templates subcommand %q\n\n", sub)
		templatesGroupUsage(env.Stderr)
		return client.ExitUsage
	}
}

// templatesGroupUsage explains the templates command family.
func templatesGroupUsage(w io.Writer) {
	fmt.Fprint(w, `proteos templates — list the machine templates (types) you can create

A template is the "type" of a machine — full-stack, go, … — bundling the image
and default resources. 'machines ls' shows each machine's template; this lists
the catalog of templates you can pick from.

Commands:
  ls               List the templates (id, label, description)

Reads accept --json. Run 'proteos templates <command> -h' for flags.
`)
}

func templatesList(env Env, args []string) int {
	fs := cmdFlags(env, "templates ls", cmdHelp{
		summary:  "List the machine templates you can create.",
		usage:    "proteos templates ls [--json]",
		examples: []string{"proteos templates ls", "proteos templates ls --json"},
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
	ts, err := c.ListTemplates(ctx())
	if err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, ts); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	if len(ts) == 0 {
		fmt.Fprintln(env.Stdout, "No templates (single-image deployment).")
		return client.ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLABEL\tDESCRIPTION")
	for _, t := range ts {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", t.ID, t.Label, t.Description)
	}
	tw.Flush()
	return client.ExitOK
}
