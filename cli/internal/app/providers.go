package app

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/tavon-ai/proteos/cli/internal/client"
)

func runProviders(env Env, args []string) int {
	if groupHelp(args) {
		providersGroupUsage(env.Stdout)
		return client.ExitOK
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return providersList(env, rest)
	case "get", "show":
		return providersGet(env, rest)
	default:
		return unknownSubcommand(env, "providers subcommand", sub, providersGroupUsage)
	}
}

// providersGroupUsage explains the providers command family.
func providersGroupUsage(w io.Writer) {
	fmt.Fprint(w, `proteos providers — list the AI providers a machine can run

A provider is a coding-agent CLI (Claude Code, Codex, Gemini, …) a task can
launch; each needs an API key before it can run. This group is read-only and
never exposes key material — only whether one is set (key_set). Use 'proteos
secrets set/unset' to manage the key itself.

Commands:
  ls               List providers (key, name, enabled, key_set, fields)
  get <key>        Show one provider

Reads accept --json. Run 'proteos providers <command> -h' for flags.
`)
}

func providersList(env Env, args []string) int {
	fs := cmdFlags(env, "providers ls", cmdHelp{
		summary:  "List the AI providers and whether you have a key set for each.",
		usage:    "proteos providers ls [--json] [--limit N] [--offset N]",
		examples: []string{"proteos providers ls", "proteos providers ls --json"},
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
	ps, err := c.ListProviders(ctx())
	if err != nil {
		return fail(env, err)
	}
	p := paginate(ps, *offset, *limit)
	if *asJSON {
		if err := printPageJSON(env.Stdout, p); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	if len(p.Items) == 0 {
		fmt.Fprintln(env.Stdout, "No providers registered.")
		return client.ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tNAME\tENABLED\tKEY SET\tFIELDS")
	for _, pr := range p.Items {
		fmt.Fprintf(tw, "%s\t%s\t%t\t%t\t%s\n", pr.Key, pr.DisplayName, pr.Enabled, pr.KeySet, fieldNames(pr.SecretFields))
	}
	tw.Flush()
	printPageFooter(env.Stdout, p, "providers")
	return client.ExitOK
}

// fieldNames renders a provider's declared secret field names for the ls
// table, e.g. "api_key" — the same names 'secrets set --field' expects.
func fieldNames(fields []client.SecretField) string {
	if len(fields) == 0 {
		return "-"
	}
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = f.Name
	}
	return strings.Join(names, ",")
}

func providersGet(env Env, args []string) int {
	fs := cmdFlags(env, "providers get", cmdHelp{
		summary:  "Show one provider by key, including its declared secret fields.",
		usage:    "proteos providers get [--json] <key>",
		examples: []string{"proteos providers get claude", "proteos providers get --json claude"},
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
	pr, code, ok := findProvider(env, c, fs.Arg(0))
	if !ok {
		return code
	}
	if *asJSON {
		if err := printJSON(env.Stdout, pr); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "Key:     %s\n", pr.Key)
	fmt.Fprintf(env.Stdout, "Name:    %s\n", pr.DisplayName)
	fmt.Fprintf(env.Stdout, "Enabled: %t\n", pr.Enabled)
	fmt.Fprintf(env.Stdout, "Key set: %t\n", pr.KeySet)
	for _, f := range pr.SecretFields {
		fmt.Fprintf(env.Stdout, "Field:   %s (%s)\n", f.Name, f.Label)
	}
	return client.ExitOK
}

// findProvider fetches the provider catalog (there is no single-provider read
// endpoint) and returns the entry matching key, or a not-found failure already
// reported via fail().
func findProvider(env Env, c *client.Client, key string) (client.Provider, int, bool) {
	ps, err := c.ListProviders(ctx())
	if err != nil {
		return client.Provider{}, fail(env, err), false
	}
	for _, pr := range ps {
		if pr.Key == key {
			return pr, client.ExitOK, true
		}
	}
	return client.Provider{}, fail(env, &client.APIError{Status: 404, Code: "unknown_provider"}), false
}
