package commandline

import (
	"flag"
	"fmt"
	"os"
)

// ParseParameters defines and parses all command-line flags, validates them,
// and returns a populated AppParameters struct.
func ParseParameters(args []string) (*AppParameters, error) {
	flagSet := flag.NewFlagSet(args[0], flag.ContinueOnError)

	cliFlags := registerCLIFlags(flagSet)

	flagSet.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", args[0])
		fmt.Fprintf(os.Stderr, "A behavioral bot detection tool that monitors logs and blocks malicious IPs via the configured blocking backend.\n\n")
		flagSet.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nMemory and CPU are optimized by pre-compiling regexes and using the cleanup routine.\n")
	}

	if err := flagSet.Parse(args[1:]); err != nil {
		return nil, err
	}

	// Perform initial validation that doesn't require loading the config.
	if *cliFlags.Check && *cliFlags.ConfigPath == "" {
		return nil, fmt.Errorf("--config flag is required for --check")
	}
	if *cliFlags.ConfigPath == "" || (*cliFlags.LogPath == "" && !*cliFlags.DryRun && !*cliFlags.DumpBackends) {
		flagSet.Usage()
		return nil, fmt.Errorf("missing required flags")
	}

	params := &AppParameters{
		ConfigPath:   *cliFlags.ConfigPath,
		LogPath:      *cliFlags.LogPath,
		StateDir:     *cliFlags.StateDir,
		DryRun:       *cliFlags.DryRun,
		ExitOnEOF:    *cliFlags.ExitOnEOF,
		ShowVersion:  *cliFlags.ShowVersion,
		Check:        *cliFlags.Check,
		DumpBackends: *cliFlags.DumpBackends,
		ReloadOn:     *cliFlags.ReloadOn,
		TopN:         *cliFlags.TopN,
		HTTPServer:   *cliFlags.HTTPServer,
	}

	return params, nil
}

// CLIFlagValues holds pointers to the variables where command-line flag values will be stored.
type CLIFlagValues struct {
	LogPath      *string
	ConfigPath   *string
	DryRun       *bool
	ShowVersion  *bool
	ReloadOn     *string
	TopN         *int
	HTTPServer   *string
	DumpBackends *bool
	StateDir     *string
	Check        *bool
	ExitOnEOF    *bool
}

// registerCLIFlags registers the command line flags.
func registerCLIFlags(fs *flag.FlagSet) *CLIFlagValues {
	flags := &CLIFlagValues{
		ConfigPath:   fs.String("config", "config.yaml", "Path to the configuration file."),
		LogPath:      fs.String("log-path", "", "Path to the log file to monitor."),
		DryRun:       fs.Bool("dry-run", false, "Enable dry-run mode. Processes a static log file and exits."),
		ShowVersion:  fs.Bool("version", false, "Show the application version and exit."),
		Check:        fs.Bool("check", false, "Check the configuration file for validity and exit."),
		ExitOnEOF:    fs.Bool("exit-on-eof", false, "Exit after processing the existing log file instead of tailing."),
		ReloadOn:     fs.String("reload-on", "", "Trigger a configuration reload on a specific signal (hup, usr1, usr2) or 'watcher'. By default, both are enabled."),
		TopN:         fs.Int("top-n", 0, "Number of top actors to display in the metrics summary."),
		HTTPServer:   fs.String("http-server", "", "Enable the HTTP server for metrics on the given address (e.g., :8080)."),
		DumpBackends: fs.Bool("dump-backends", false, "List currently blocked IPs and exit."),
		StateDir:     fs.String("state-dir", "", "Path to the state directory. Enables persistence if set."),
	}
	return flags
}
