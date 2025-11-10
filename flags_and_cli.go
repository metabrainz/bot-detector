package main

import (
	"flag"
	"fmt"
	"os"
)

// --- CONFIGURATION GLOBAL VARS (Set by CLI flags) ---
var (
	LogFilePath    string
	DryRun         bool
	ShowVersion    bool
	ReloadOnSignal string
)

// RegisterCLIFlags registers the command-line flags with the global flag set.
// This function is called by init() and can be called by tests after resetting flag.CommandLine.
func RegisterCLIFlags(fs *flag.FlagSet) *string {
	fs.StringVar(&LogFilePath, "log-path", "", "Required. Path to the access log file to tail (or to read in dry-run mode).")
	yamlPath := fs.String("yaml-path", "", "Required. Path to the YAML configuration file.")
	fs.BoolVar(&DryRun, "dry-run", false, "Optional. If true, runs in test mode, ignoring HAProxy and live logging.")
	fs.BoolVar(&ShowVersion, "version", false, "Optional. Print the application version and exit.")
	fs.StringVar(&ReloadOnSignal, "reload-on-signal", "", "Optional. If set to a signal name (e.g., HUP, USR1), disables file watcher and reloads config on signal.")
	return yamlPath
}

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "A behavioral bot detection tool that monitors logs and blocks malicious IPs via the configured blocking backend.\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nMemory and CPU are optimized by pre-compiling regexes and using the cleanup routine.\n")
	}
}
