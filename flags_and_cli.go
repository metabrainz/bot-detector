package main

import (
	"flag"
	"fmt"
	"os"
)

// CLIFlagValues holds pointers to the variables where command-line flag values will be stored.
// This struct is returned by RegisterCLIFlags to provide access to the parsed flag values.
type CLIFlagValues struct {
	LogPath        *string
	ConfigPath     *string
	DryRun         *bool
	ShowVersion    *bool
	ReloadOn       *string
	TopN           *int
	HTTPListenAddr *string
}

// RegisterCLIFlags registers the command-line flags with the global flag set.
func RegisterCLIFlags(fs *flag.FlagSet) *CLIFlagValues {
	flags := &CLIFlagValues{}
	flags.LogPath = fs.String("log-path", "", "Path to the access log file. Required for live mode. If omitted in dry-run mode, reads from stdin.")
	flags.ConfigPath = fs.String("config", "", "Required. Path to the YAML configuration file.")
	flags.DryRun = fs.Bool("dry-run", false, "Optional. If true, runs in test mode, ignoring HAProxy and live logging.")
	flags.ShowVersion = fs.Bool("version", false, "Optional. Print the application version and exit.")
	flags.ReloadOn = fs.String("reload-on", "", "Optional. Controls config reloading. Use `watcher` for file-watching only, or `hup`, `usr1`, `usr2` for signal-based reloads only. If absent, both watcher and `SIGHUP` are enabled.")
	flags.TopN = fs.Int("top-n", 0, "Optional. In dry-run mode, show top N actors per chain. Default is 0 (disabled).")
	flags.HTTPListenAddr = fs.String("http-listen-addr", "", "Optional. If set (e.g., \"127.0.0.1:8080\"), starts a web server on this address. Disabled by default.")
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
