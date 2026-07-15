package app

import (
	"flag"
	"io"
	"reflect"
	"strings"

	"github.com/tavon-ai/proteos/cli/internal/client"
)

// This file implements 'proteos --help-json': a machine-readable dump of the
// whole command tree — every group, leaf, top-level command, and flag — emitted offline (no server
// contact). It is built for non-human consumers (tooling, agents) that want to
// enumerate the CLI without scraping -h text or guessing.
//
// Single source of truth: rather than re-declaring flags, it runs each leaf in
// "describe mode" (Env.describe set). In that mode cmdFlags records the leaf's
// help metadata and parse() captures the fully-registered *flag.FlagSet and
// returns before any newClient() call. So the JSON can never drift from -h —
// both read the exact same flag definitions.

// helpFlag describes one flag of a leaf command.
type helpFlag struct {
	Name    string `json:"name"`              // flag name without dashes, e.g. "machine"
	Type    string `json:"type"`              // "string", "bool", "duration", …
	Default string `json:"default,omitempty"` // default value as printed by -h
	Usage   string `json:"usage"`             // one-line flag description
}

// helpCommand describes one leaf command (e.g. "task run").
type helpCommand struct {
	Path     string     `json:"path"`               // full invocation path, e.g. "task run"
	Group    string     `json:"group,omitempty"`    // owning group, e.g. "task"; empty for top-level commands
	Name     string     `json:"name"`               // leaf name, e.g. "run"
	Aliases  []string   `json:"aliases,omitempty"`  // alternative leaf names, e.g. "list" for "ls"
	Summary  string     `json:"summary,omitempty"`  // one-line description
	Long     string     `json:"long,omitempty"`     // optional detail paragraph(s)
	Usage    string     `json:"usage,omitempty"`    // the invocation line shown by -h
	Examples []string   `json:"examples,omitempty"` // example invocations
	Flags    []helpFlag `json:"flags"`              // flags, in the order -h prints them
}

// helpGroup describes a command group (e.g. "task") and its leaves.
type helpGroup struct {
	Name     string        `json:"name"`
	Aliases  []string      `json:"aliases,omitempty"`
	Summary  string        `json:"summary,omitempty"`
	Commands []helpCommand `json:"commands"`
}

// helpTree is the top-level document emitted by --help-json.
type helpTree struct {
	Program  string        `json:"program"`
	Version  string        `json:"version"`
	Commit   string        `json:"commit,omitempty"`
	Date     string        `json:"date,omitempty"`
	Summary  string        `json:"summary"`
	Groups   []helpGroup   `json:"groups"`
	Commands []helpCommand `json:"commands"` // group-less commands: version, help, help-json
}

// describer captures one leaf command's flag/help metadata while it runs in
// describe mode. cmdFlags stashes (pendingName, pendingHelp); parse() then pairs
// them with the registered flag set into captured.
type describer struct {
	pendingName string
	pendingHelp cmdHelp
	captured    *helpCommand
}

// capture snapshots fs (already populated with the leaf's flags) together with
// the help metadata cmdFlags stashed, producing the helpCommand for this leaf.
func (d *describer) capture(fs *flag.FlagSet) {
	d.captured = &helpCommand{
		Path:     d.pendingName,
		Summary:  d.pendingHelp.summary,
		Long:     d.pendingHelp.long,
		Usage:    d.pendingHelp.usage,
		Examples: d.pendingHelp.examples,
		Flags:    flagInfos(fs),
	}
}

// flagInfos renders fs's flags in the same lexical order -h prints them.
func flagInfos(fs *flag.FlagSet) []helpFlag {
	out := []helpFlag{}
	fs.VisitAll(func(f *flag.Flag) {
		out = append(out, helpFlag{
			Name:    f.Name,
			Type:    flagType(f),
			Default: f.DefValue,
			Usage:   f.Usage,
		})
	})
	return out
}

// flagType reports a flag's value type ("bool", "string", "duration", …). It
// keys off the same signals the stdlib flag package uses: IsBoolFlag for bools,
// and the concrete value type's name (e.g. *flag.durationValue → "duration").
func flagType(f *flag.Flag) string {
	if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
		return "bool"
	}
	t := reflect.TypeOf(f.Value)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return "string"
	}
	return strings.TrimSuffix(t.Name(), "Value")
}

// leaf names a single command and the function that builds/runs it. run is
// invoked in describe mode to introspect its flags and help text.
type leaf struct {
	name    string
	aliases []string
	run     func(Env, []string) int
}

// group names a command family, its aliases, and the group-usage function whose
// header line supplies the group summary.
type group struct {
	name    string
	aliases []string
	usage   func(io.Writer)
	leaves  []leaf
}

// commandRegistry mirrors the dispatch tree in app.go and the run* dispatchers.
// It is the one place that lists which commands exist; flags/help for each leaf
// come from the leaf itself via describe mode, so only this membership list has
// to track new commands. (TestHelpJSONOffline and TestHelpJSONMatchesCommandHelp
// guard against drift.)
func commandRegistry() []group {
	return []group{
		{name: "auth", usage: authGroupUsage, leaves: []leaf{
			{name: "login", run: authLogin},
			{name: "status", run: authStatus},
			{name: "logout", run: authLogout},
		}},
		{name: "machines", aliases: []string{"machine"}, usage: machinesGroupUsage, leaves: []leaf{
			{name: "ls", aliases: []string{"list"}, run: machinesList},
			{name: "get", aliases: []string{"show"}, run: machinesGet},
			{name: "create", aliases: []string{"new"}, run: machinesCreate},
			{name: "start", run: machinesStart},
			{name: "stop", run: machinesStop},
		}},
		{name: "templates", aliases: []string{"template"}, usage: templatesGroupUsage, leaves: []leaf{
			{name: "ls", aliases: []string{"list"}, run: templatesList},
		}},
		{name: "repo", aliases: []string{"repos"}, usage: reposGroupUsage, leaves: []leaf{
			{name: "ls", aliases: []string{"list"}, run: reposList},
		}},
		{name: "project", aliases: []string{"projects"}, usage: projectsGroupUsage, leaves: []leaf{
			{name: "ls", aliases: []string{"list"}, run: projectsList},
			{name: "clone", run: projectClone},
			{name: "ensure", run: projectEnsure},
		}},
		{name: "git", usage: gitGroupUsage, leaves: []leaf{
			{name: "status", run: gitStatus},
			{name: "diff", run: gitDiff},
			{name: "branch", run: gitBranch},
			{name: "commit", run: gitCommit},
			{name: "push", run: gitPush},
			{name: "pr", run: gitPR},
		}},
		{name: "pr", usage: prGroupUsage, leaves: []leaf{
			{name: "view", run: prView},
			{name: "files", run: prFiles},
			{name: "checks", run: prChecks},
			{name: "merge", run: prMerge},
			{name: "comment", run: prComment},
		}},
		{name: "task", aliases: []string{"tasks"}, usage: taskGroupUsage, leaves: []leaf{
			{name: "run", run: taskRun},
			{name: "ls", aliases: []string{"list"}, run: taskList},
			{name: "get", aliases: []string{"show"}, run: taskGet},
			{name: "watch", run: taskWatch},
			{name: "cancel", run: taskCancel},
			{name: "send", aliases: []string{"message"}, run: taskSend},
		}},
		{name: "providers", aliases: []string{"provider"}, usage: providersGroupUsage, leaves: []leaf{
			{name: "ls", aliases: []string{"list"}, run: providersList},
			{name: "get", aliases: []string{"show"}, run: providersGet},
		}},
		{name: "secrets", aliases: []string{"secret"}, usage: secretsGroupUsage, leaves: []leaf{
			{name: "set", run: secretsSet},
			{name: "unset", aliases: []string{"delete", "rm"}, run: secretsUnset},
		}},
	}
}

// topLevelCommands lists the group-less commands Run dispatches directly.
// Unlike group leaves they take no flags and register no FlagSet, so their
// entries are authored here; the aliases mirror Run's switch cases exactly.
func topLevelCommands() []helpCommand {
	return []helpCommand{
		{
			Path:    "version",
			Name:    "version",
			Aliases: []string{"--version", "-v"},
			Summary: "Print the CLI version, commit, and build date.",
			Usage:   "proteos version",
			Flags:   []helpFlag{},
		},
		{
			Path:    "help",
			Name:    "help",
			Aliases: []string{"--help", "-h"},
			Summary: "Show the command overview.",
			Usage:   "proteos help",
			Flags:   []helpFlag{},
		},
		{
			Path:    "help-json",
			Name:    "help-json",
			Aliases: []string{"--help-json"},
			Summary: "Emit the full command tree (flags included) as JSON, for tools and agents.",
			Usage:   "proteos --help-json",
			Flags:   []helpFlag{},
		},
	}
}

// helpTreeOf builds the full command tree by introspecting every leaf offline.
func helpTreeOf(env Env) helpTree {
	t := helpTree{
		Program: "proteos",
		Version: env.Version,
		Commit:  env.Commit,
		Date:    env.Date,
		Summary: "drive the ProteOS Agent Task lane from the command line",
	}
	for _, g := range commandRegistry() {
		hg := helpGroup{
			Name:    g.name,
			Aliases: g.aliases,
			Summary: summaryFromUsage(g.usage),
		}
		for _, l := range g.leaves {
			hg.Commands = append(hg.Commands, describeLeaf(env, g, l))
		}
		t.Groups = append(t.Groups, hg)
	}
	t.Commands = topLevelCommands()
	return t
}

// describeLeaf produces the helpCommand for one leaf: it runs the leaf in
// describe mode (which captures its flags before any server call) and fills in
// the group/name/aliases the leaf itself doesn't know.
func describeLeaf(baseEnv Env, g group, l leaf) helpCommand {
	d := &describer{}
	env := baseEnv
	env.describe = d
	l.run(env, nil)
	var cmd helpCommand
	if d.captured != nil {
		cmd = *d.captured
	}
	if cmd.Path == "" {
		cmd.Path = g.name + " " + l.name
	}
	cmd.Group = g.name
	cmd.Name = l.name
	cmd.Aliases = l.aliases
	if cmd.Flags == nil {
		cmd.Flags = []helpFlag{}
	}
	return cmd
}

// summaryFromUsage pulls a group's one-line summary from its usage header, whose
// first line reads "proteos <group> — <summary>".
func summaryFromUsage(fn func(io.Writer)) string {
	if fn == nil {
		return ""
	}
	var b strings.Builder
	fn(&b)
	line := b.String()
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if i := strings.Index(line, "— "); i >= 0 {
		return strings.TrimSpace(line[i+len("— "):])
	}
	return strings.TrimSpace(line)
}

// emitHelpJSON writes the full command tree as indented JSON to stdout.
func emitHelpJSON(env Env) int {
	if err := printJSON(env.Stdout, helpTreeOf(env)); err != nil {
		return fail(env, err)
	}
	return client.ExitOK
}
