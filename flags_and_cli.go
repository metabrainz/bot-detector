package main

import (
	"flag"
	"fmt"
	"os"
)

// --- CONFIGURATION GLOBAL VARS (Set by CLI flags) ---
var (
	LogFilePath  string
	YAMLFilePath string
	DryRun       bool
	TestLogPath  string
)

// RegisterCLIFlags registers the command-line flags with the global flag set.
// This function is called by init() and can be called by tests after resetting flag.CommandLine.
func RegisterCLIFlags(fs *flag.FlagSet) {
	fs.StringVar(&LogFilePath, "log-path", "/var/log/http/access.log", "Path to the live access log file to tail (ignored in dry-run).")
	fs.StringVar(&YAMLFilePath, "yaml-path", "chains.yaml", "Path to the YAML configuration file defining behavioral chains.")
	fs.BoolVar(&DryRun, "dry-run", false, "If true, runs in test mode: skips HAProxy/live logging, ignores cleanup/polling, and uses --test-log.")
	fs.StringVar(&TestLogPath, "test-log", "test_access.log", "Path to a static file containing log lines for dry-run testing.")
}

func init() {
	RegisterCLIFlags(flag.CommandLine) // Register flags on the default global flag set.
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "A behavioral bot detection tool that monitors logs and blocks malicious IPs via the HAProxy Runtime API.\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nMemory and CPU are optimized by pre-compiling regexes and using the cleanup routine.\n")
	}
}
