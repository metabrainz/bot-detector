package main

import (
	"fmt"
	"io"
)

// commandFunc handles a resolved command.
type commandFunc func(ctx *cmdContext) int

// commandSpec describes one CLI command for dispatch and help.
type commandSpec struct {
	group   string
	command string
	args    string // positional args summary for help (e.g. "<ip>")
	desc    string
	run     commandFunc
}

// registry lists every command. The order here is the order shown in help.
var registry = []commandSpec{
	{"ip", "check", "<ip>", "Show block status of an IP (exit 2 if blocked, 3 if unknown)", cmdIPCheck},
	{"ip", "unblock", "<ip>", "Unblock an IP (checks status first; prompts unless --yes)", cmdIPUnblock},
	{"ip", "clear", "<ip>", "Clear an IP from all state (prompts unless --yes)", cmdIPClear},

	{"blocks", "unblock", "--reason <r>", "Unblock all IPs blocked by a reason (prompts unless --yes)", cmdBlocksUnblock},

	{"bad-actors", "list", "[--reason <r>]", "List bad actors, optionally filtered by reason substring", cmdBadActorsList},
	{"bad-actors", "stats", "", "Show bad actor statistics (counts by reason and day)", cmdBadActorsStats},
	{"bad-actors", "export", "", "Export bad actor IPs, one per line", cmdBadActorsExport},
	{"bad-actors", "remove", "--reason <r> [--unblock]", "Remove bad actors by reason (prompts unless --yes)", cmdBadActorsRemove},

	{"config", "show", "", "Print the running YAML configuration", cmdConfigShow},
	{"config", "archive", "[-o <file>]", "Download config + dependencies as a .tar.gz", cmdConfigArchive},

	{"metrics", "show", "[--aggregate]", "Show node metrics (or cluster-wide with --aggregate)", cmdMetricsShow},

	{"cluster", "status", "", "Show this node's cluster status", cmdClusterStatus},
	{"cluster", "state", "[--reason <r>]", "Show merged cluster block state", cmdClusterState},

	{"endpoints", "", "", "List the instance's API endpoints", cmdEndpoints},
}

// lookupCommand finds a handler for a group+command pair. Commands with an
// empty command name (e.g. "endpoints") match on the group alone.
func lookupCommand(group, command string) (commandFunc, bool) {
	for _, s := range registry {
		if s.group == group && s.command == command {
			return s.run, true
		}
		if s.group == group && s.command == "" && command == "" {
			return s.run, true
		}
	}
	return nil, false
}

// printUsage writes the full help text.
func printUsage(w io.Writer) {
	fp := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

	fp(`botctl - command-line client for a bot-detector instance

Usage:
  botctl [global flags] <group> <command> [args]

Target selection (in priority order):
  --url <url>            explicit base URL
  BOT_DETECTOR_URL       environment variable
  %s   default

Global flags:
  --url <url>            base URL of the bot-detector API
  --json                 print the raw JSON/body from the server
  -y, --yes              skip confirmation for destructive actions
  --timeout <dur>        HTTP timeout (default 10s, e.g. 5s, 1m)
  -h, --help             show this help

Commands:
`, defaultBaseURL)
	group := ""
	for _, s := range registry {
		if s.group != group {
			fp("\n  %s\n", s.group)
			group = s.group
		}
		name := s.command
		if name == "" {
			name = s.group
		} else {
			name = s.group + " " + s.command
		}
		invocation := name
		if s.args != "" {
			invocation = name + " " + s.args
		}
		fp("    %-42s %s\n", invocation, s.desc)
	}
	fp(`
Examples:
  botctl ip check 1.2.3.4
  BOT_DETECTOR_URL=http://gw1:8090 botctl ip unblock 1.2.3.4
  botctl bad-actors list --reason abusers-444
  botctl bad-actors remove --reason abusers-444 --unblock --yes
  botctl --json cluster status
`)
}
