package main

import (
	"flag"
	"fmt"
	"os"
)

// CLIFlagValues holds pointers to the variables where command-line flag values will be stored.
// This struct is returned by RegisterCLIFlags to provide access to the parsed flag values.
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
}

// RegisterCLIFlags registers the command line flags.
func RegisterCLIFlags(fs *flag.FlagSet) *CLIFlagValues {
	flags := &CLIFlagValues{
		ConfigPath:   fs.String("config", "config.yaml", "Path to the configuration file."),
		LogPath:      fs.String("log-path", "", "Path to the log file to monitor."),
		DryRun:       fs.Bool("dry-run", false, "Enable dry-run mode. Processes a static log file and exits."),
		ShowVersion:  fs.Bool("version", false, "Show the application version and exit."),
		Check:        fs.Bool("check", false, "Check the configuration file for validity and exit."),
		ReloadOn:     fs.String("reload-on", "", "Trigger a configuration reload on a specific signal (hup, usr1, usr2) or 'watcher'. By default, both are enabled."),
		TopN:         fs.Int("top-n", 0, "Number of top actors to display in the metrics summary."),
		HTTPServer:   fs.String("http-server", "", "Enable the HTTP server for metrics on the given address (e.g., :8080)."),
		DumpBackends: fs.Bool("dump-backends", false, "List currently blocked IPs and exit."),
		StateDir:     fs.String("state-dir", "", "Path to the state directory. Enables persistence if set."),
	}
	return flags
}

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "A behavioral bot detection tool that monitors logs and blocks malicious IPs via the configured blocking backend.\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nMemory and CPU are optimized by pre-compiling regexes and using the cleanup routine.\n")
	}
}
