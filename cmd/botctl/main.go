// Command botctl is a small CLI for interacting with a running bot-detector
// instance over its HTTP API.
//
// The target instance is selected via --url, the BOT_DETECTOR_URL environment
// variable, or the default http://localhost:8090 (in that order of priority).
//
// Commands are grouped by resource (ip, blocks, bad-actors, config, metrics,
// cluster) and each maps to exactly one API endpoint. Destructive commands
// (unblock, clear, remove) prompt for confirmation unless --yes is given.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Exit codes. Beyond success/error, `ip check` uses distinct codes so scripts
// can branch on block status without parsing output.
const (
	exitOK       = 0 // success; for `ip check`: not blocked
	exitError    = 1 // usage error, network error, or server error
	exitBlocked  = 2 // `ip check`: the IP is blocked
	exitNotFound = 3 // `ip check`: the IP is unknown to the instance
)

// globalOpts holds flags shared by every command.
type globalOpts struct {
	url     string
	json    bool
	yes     bool
	timeout time.Duration
}

// context bundles the resolved client and options passed to each command.
type cmdContext struct {
	c    *client
	opts globalOpts
	args []string // positional args after the group+command
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// run parses global flags, resolves the target, and dispatches to a command.
// It returns the process exit code.
func run(argv []string) int {
	opts := globalOpts{timeout: 10 * time.Second}

	// Manual global-flag parsing so flags may appear before the group/command
	// without the group/command being consumed by the flag package.
	var rest []string
	i := 0
	for i < len(argv) {
		arg := argv[i]
		switch {
		case arg == "--":
			rest = append(rest, argv[i+1:]...)
			i = len(argv)
		case arg == "-h" || arg == "--help" || arg == "help":
			printUsage(os.Stdout)
			return exitOK
		case arg == "--json":
			opts.json = true
		case arg == "-y" || arg == "--yes":
			opts.yes = true
		case arg == "--url":
			v, ok := nextValue(argv, &i)
			if !ok {
				return usageErr("--url requires a value")
			}
			opts.url = v
		case strings.HasPrefix(arg, "--url="):
			opts.url = strings.TrimPrefix(arg, "--url=")
		case arg == "--timeout":
			v, ok := nextValue(argv, &i)
			if !ok {
				return usageErr("--timeout requires a value")
			}
			d, err := time.ParseDuration(v)
			if err != nil {
				return usageErr(fmt.Sprintf("invalid --timeout %q: %v", v, err))
			}
			opts.timeout = d
		case strings.HasPrefix(arg, "--timeout="):
			d, err := time.ParseDuration(strings.TrimPrefix(arg, "--timeout="))
			if err != nil {
				return usageErr(fmt.Sprintf("invalid --timeout: %v", err))
			}
			opts.timeout = d
		case strings.HasPrefix(arg, "-"):
			// Unknown global flag before the command.
			return usageErr(fmt.Sprintf("unknown flag %q", arg))
		default:
			// First non-flag token: the rest is group + command + args.
			rest = argv[i:]
			i = len(argv)
		}
		i++
	}

	if len(rest) == 0 {
		printUsage(os.Stderr)
		return exitError
	}

	group := rest[0]
	var command string
	var args []string
	if len(rest) > 1 {
		command = rest[1]
		args = rest[2:]
	}

	ctx := &cmdContext{
		c:    newClient(resolveBaseURL(opts.url), opts.timeout),
		opts: opts,
		args: args,
	}

	handler, ok := lookupCommand(group, command)
	if !ok {
		return usageErr(fmt.Sprintf("unknown command: %s %s", group, command))
	}
	return handler(ctx)
}

// nextValue returns the argument following the current index, advancing the
// index. It reports false if there is no following value.
func nextValue(argv []string, i *int) (string, bool) {
	if *i+1 >= len(argv) {
		return "", false
	}
	*i++
	return argv[*i], true
}

// usageErr prints a message plus a usage hint and returns the error exit code.
func usageErr(msg string) int {
	fmt.Fprintln(os.Stderr, "error: "+msg)
	fmt.Fprintln(os.Stderr, "run 'botctl help' for usage")
	return exitError
}

// fail prints an error to stderr and returns the error exit code.
func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error: "+err.Error())
	return exitError
}

// eprintln writes a line to stderr, discarding the error (best-effort output).
func eprintln(msg string) {
	_, _ = fmt.Fprintln(os.Stderr, msg)
}

// printJSON writes indented JSON of v to stdout.
func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// printRaw writes a raw response body to stdout, ensuring a trailing newline.
func printRaw(body []byte) {
	os.Stdout.Write(body) //nolint:errcheck
	if len(body) > 0 && body[len(body)-1] != '\n' {
		fmt.Println()
	}
}

// validateIP canonicalizes and validates an IP argument.
func validateIP(arg string) (string, error) {
	ip := net.ParseIP(strings.TrimSpace(arg))
	if ip == nil {
		return "", fmt.Errorf("invalid IP address: %q", arg)
	}
	return ip.String(), nil
}

// confirm prompts the user to approve a destructive action. It returns true if
// --yes was set or the user answered yes. On a non-interactive stdin with no
// --yes, it returns false.
func confirm(opts globalOpts, prompt string) bool {
	if opts.yes {
		return true
	}
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "y" || answer == "yes"
}
