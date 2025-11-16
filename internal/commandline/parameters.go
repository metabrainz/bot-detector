package commandline

// AppParameters holds the fully parsed and validated configuration
// from command-line flags, ready for execution.
type AppParameters struct {
	ConfigPath   string
	LogPath      string
	StateDir     string
	DryRun       bool
	ExitOnEOF    bool
	ShowVersion  bool
	Check        bool
	DumpBackends bool
	ReloadOn     string
	TopN         int
	HTTPServer   string
}
