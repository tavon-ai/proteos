package app

import (
	"fmt"
	"io"
	"strings"

	"github.com/tavon-ai/proteos/cli/internal/client"
)

func runSecrets(env Env, args []string) int {
	if groupHelp(args) {
		secretsGroupUsage(env.Stdout)
		return client.ExitOK
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "set":
		return secretsSet(env, rest)
	case "unset", "delete", "rm":
		return secretsUnset(env, rest)
	default:
		return unknownSubcommand(env, "secrets subcommand", sub, secretsGroupUsage)
	}
}

// secretsGroupUsage explains the secrets command family.
func secretsGroupUsage(w io.Writer) {
	fmt.Fprint(w, `proteos secrets — set or remove a provider's API key

Keys are stored encrypted and injected into your machines at runtime. A value
is write-only: 'secrets set' never echoes it back, and 'proteos providers ls'
only ever shows whether a key is set (key_set), not its contents.

Commands:
  set <provider>     Set (or replace) a provider's key
  unset <provider>   Remove a provider's stored key

Run 'proteos providers get <key>' to see the field names a provider declares,
and 'proteos secrets <command> -h' for flags.
`)
}

// fieldFlag collects repeated --field name=value flags.
type fieldFlag []string

func (f *fieldFlag) String() string { return strings.Join(*f, ",") }
func (f *fieldFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func secretsSet(env Env, args []string) int {
	fs := cmdFlags(env, "secrets set", cmdHelp{
		summary: "Set (or replace) a provider's API key.",
		long: "Fields come from --field name=value (repeatable) — run 'proteos providers\n" +
			"get <provider>' to see the field names it declares. Providers with exactly\n" +
			"one field also accept --key or --stdin as shorthand, so the value need not\n" +
			"appear in shell history or process args.",
		usage: "proteos secrets set [--field name=value ...] [--key <value>] [--stdin] <provider>",
		examples: []string{
			"proteos secrets set --key sk-ant-... claude",
			"echo $ANTHROPIC_API_KEY | proteos secrets set --stdin claude",
			"proteos secrets set --field api_key=sk-... openai",
		},
	})
	url := fs.String("url", "", "control-plane base URL (or PROTEOS_URL)")
	var fields fieldFlag
	fs.Var(&fields, "field", "a name=value pair for a declared secret field (repeatable)")
	key := fs.String("key", "", "shorthand: the value for a provider's sole secret field")
	stdin := fs.Bool("stdin", false, "shorthand: read a provider's sole secret field from stdin")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if ok, code := parse(env, fs, args); !ok {
		return code
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return client.ExitUsage
	}
	providerKey := fs.Arg(0)

	values := map[string]string{}
	for _, f := range fields {
		name, val, ok := strings.Cut(f, "=")
		if !ok {
			writeErrorMsg(env, "bad_field", fmt.Sprintf("--field must be name=value, got %q", f))
			return client.ExitUsage
		}
		values[name] = val
	}
	if *key != "" && *stdin {
		writeErrorMsg(env, "bad_flags", "--key and --stdin are mutually exclusive")
		return client.ExitUsage
	}

	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}

	if *key != "" || *stdin {
		pr, code, ok := findProvider(env, c, providerKey)
		if !ok {
			return code
		}
		if len(pr.SecretFields) != 1 {
			writeErrorMsg(env, "ambiguous_field", fmt.Sprintf("%s declares %d fields — use --field name=value for each", providerKey, len(pr.SecretFields)))
			return client.ExitUsage
		}
		field := pr.SecretFields[0].Name
		if *stdin {
			if env.Stdin == nil {
				writeErrorMsg(env, "no_stdin", "--stdin given but no input is available")
				return client.ExitUsage
			}
			b, err := io.ReadAll(env.Stdin)
			if err != nil {
				return fail(env, err)
			}
			values[field] = strings.TrimSpace(string(b))
		} else {
			values[field] = *key
		}
	}

	if len(values) == 0 {
		writeErrorMsg(env, "no_fields", "no fields given — pass --field name=value, --key, or --stdin")
		return client.ExitUsage
	}

	if err := c.SetProviderKey(ctx(), providerKey, values); err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, providerKeyResult{Provider: providerKey, KeySet: true}); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "%s key set\n", providerKey)
	return client.ExitOK
}

// providerKeyResult is the --json confirmation for secrets set/unset. The
// mutation endpoints themselves respond 204 with no body (values are never
// echoed), so this is a CLI-side summary, not a passthrough of a server reply.
type providerKeyResult struct {
	Provider string `json:"provider"`
	KeySet   bool   `json:"key_set"`
}

func secretsUnset(env Env, args []string) int {
	fs := cmdFlags(env, "secrets unset", cmdHelp{
		summary:  "Remove a provider's stored key.",
		usage:    "proteos secrets unset [--json] <provider>",
		examples: []string{"proteos secrets unset claude", "proteos secrets unset --json claude"},
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
	providerKey := fs.Arg(0)
	c, _, code, ok := newClient(env, *url)
	if !ok {
		return code
	}
	if err := c.DeleteProviderKey(ctx(), providerKey); err != nil {
		return fail(env, err)
	}
	if *asJSON {
		if err := printJSON(env.Stdout, providerKeyResult{Provider: providerKey, KeySet: false}); err != nil {
			return fail(env, err)
		}
		return client.ExitOK
	}
	fmt.Fprintf(env.Stdout, "%s key removed\n", providerKey)
	return client.ExitOK
}
