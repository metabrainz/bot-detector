package commandline

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AppParameters holds the fully parsed and validated configuration
// from command-line flags, ready for execution.
type AppParameters struct {
	Check           bool
	ConfigDir       string // Directory containing config.yaml
	DryRun          bool
	DumpBackends    bool
	ExitOnEOF       bool
	HTTPServer      string
	LogPath         string
	ReloadOn        string
	ShowVersion     bool
	StateDir        string
	TopN            int
	ClusterNodeName string
}

// String implements the fmt.Stringer interface for AppParameters.
func (p AppParameters) String() string {
	var sb strings.Builder

	const labelFormat = "  %-20s: "
	writeField := func(label string, format string, value interface{}) {
		sb.WriteString(fmt.Sprintf(labelFormat+format+"\n", label, value))
	}

	sb.WriteString("--- App Parameters ---\n")

	writeField("Check", "%v", p.Check)
	writeField("ConfigDir", "%q", p.ConfigDir)
	writeField("DryRun", "%v", p.DryRun)
	writeField("DumpBackends", "%v", p.DumpBackends)
	writeField("ExitOnEOF", "%v", p.ExitOnEOF)
	writeField("HTTPServer", "%q", p.HTTPServer)
	writeField("LogPath", "%q", p.LogPath)
	writeField("ReloadOn", "%q", p.ReloadOn)
	writeField("ShowVersion", "%v", p.ShowVersion)
	writeField("StateDir", "%q", p.StateDir)
	writeField("TopN", "%v", p.TopN)
	writeField("ClusterNodeName", "%q", p.ClusterNodeName)

	sb.WriteString("----------------------\n")
	return sb.String()
}

// ParseParameters defines and parses all command-line flags, validates them,
// and returns a populated AppParameters struct.
func ParseParameters(args []string) (*AppParameters, error) {
	flagSet := flag.NewFlagSet(args[0], flag.ContinueOnError)
	cliFlags := registerCLIFlags(flagSet)

	flagSet.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", args[0])
		fmt.Fprintf(os.Stderr, "A behavioral bot detection tool that monitors logs and blocks malicious IPs via the configured blocking backend.\n\n")
		flagSet.PrintDefaults()
	}

	if err := flagSet.Parse(args[1:]); err != nil {
		return nil, err
	}
	if flagSet.NFlag() == 0 {
		// Display usage if no parameters
		flagSet.Usage()
		return nil, fmt.Errorf("no flag: help requested")
	}

	params := &AppParameters{
		Check:           *cliFlags.Check,
		ConfigDir:       *cliFlags.ConfigDir,
		DryRun:          *cliFlags.DryRun,
		DumpBackends:    *cliFlags.DumpBackends,
		ExitOnEOF:       *cliFlags.ExitOnEOF,
		HTTPServer:      *cliFlags.HTTPServer,
		LogPath:         *cliFlags.LogPath,
		ReloadOn:        *cliFlags.ReloadOn,
		ShowVersion:     *cliFlags.ShowVersion,
		StateDir:        *cliFlags.StateDir,
		TopN:            *cliFlags.TopN,
		ClusterNodeName: *cliFlags.ClusterNodeName,
	}

	// --version is a special case, returns ASAP
	if params.ShowVersion {
		return params, nil
	}

	hasConfigDir := false
	hasLogPath := false

	// Resolve absolute paths immediately.
	if params.ConfigDir != "" {
		absPath, err := filepath.Abs(params.ConfigDir)
		if err != nil {
			return nil, fmt.Errorf("could not determine absolute path for config dir: %v", err)
		}
		params.ConfigDir = absPath
		hasConfigDir = true
	}

	if params.LogPath != "" {
		absPath, err := filepath.Abs(params.LogPath)
		if err != nil {
			return nil, fmt.Errorf("could not determine absolute path for log file: %v", err)
		}
		params.LogPath = absPath
		hasLogPath = true
	}

	if params.StateDir != "" {
		absPath, err := filepath.Abs(params.StateDir)
		if err != nil {
			return nil, fmt.Errorf("could not determine absolute path for state directory: %v", err)
		}
		params.StateDir = absPath
	}

	// Dry run don't require a log path
	requireLogPath := true
	requireConfigDir := true

	if params.Check || params.DumpBackends || params.DryRun {
		requireLogPath = false
	}

	if requireConfigDir && !hasConfigDir {
		return nil, fmt.Errorf("--config-dir <path> is required")
	}

	if requireLogPath && !hasLogPath {
		return nil, fmt.Errorf("--log-path <path> is required")
	}

	return params, nil
}

// CLIFlagValues holds pointers to the variables where command-line flag values will be stored.
type CLIFlagValues struct {
	Check           *bool
	ConfigDir       *string
	DryRun          *bool
	DumpBackends    *bool
	ExitOnEOF       *bool
	HTTPServer      *string
	LogPath         *string
	ReloadOn        *string
	ShowVersion     *bool
	StateDir        *string
	TopN            *int
	ClusterNodeName *string
}

// registerCLIFlags registers the command line flags.
func registerCLIFlags(fs *flag.FlagSet) *CLIFlagValues {
	flags := &CLIFlagValues{
		Check:           fs.Bool("check", false, "Check the configuration file for validity and exit."),
		ConfigDir:       fs.String("config-dir", "", "Path to the configuration file."),
		DryRun:          fs.Bool("dry-run", false, "Enable dry-run mode. Processes a static log file and exits."),
		DumpBackends:    fs.Bool("dump-backends", false, "List currently blocked IPs and exit."),
		ExitOnEOF:       fs.Bool("exit-on-eof", false, "Exit after processing the existing log file instead of tailing."),
		HTTPServer:      fs.String("http-server", "", "Enable the HTTP server for metrics on the given address (e.g., :8080)."),
		LogPath:         fs.String("log-path", "", "Path to the log file to monitor."),
		ReloadOn:        fs.String("reload-on", "", "Trigger a configuration reload on a specific signal (hup, usr1, usr2) or 'watcher'. By default, both are enabled."),
		ShowVersion:     fs.Bool("version", false, "Show the application version and exit."),
		StateDir:        fs.String("state-dir", "", "Path to the state directory. Enables persistence if set."),
		TopN:            fs.Int("top-n", 0, "Number of top actors to display in the metrics summary."),
		ClusterNodeName: fs.String("cluster-node-name", "", "Node name for cluster identification. Overrides port-based matching."),
	}
	return flags
}
