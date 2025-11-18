package server

import (
	"os"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/types"
)

// Provider defines the interface required by the HTTP server to access application data.
// This interface decouples the server from the main application implementation,
// allowing the server to request metrics, configuration, and lifecycle information
// without direct dependencies on internal application structures.
type Provider interface {
	// GetListenAddr returns the HTTP server listen address (e.g., "127.0.0.1:8080").
	// Returns empty string if the HTTP server is disabled.
	GetListenAddr() string

	// GetShutdownChannel returns a channel that signals when the application is shutting down.
	GetShutdownChannel() chan os.Signal

	// Log writes a log message with the specified level, tag, and format.
	Log(level logging.LogLevel, tag string, format string, v ...interface{})

	// GetConfigForArchive retrieves the main configuration and all file dependencies
	// for creating a configuration archive.
	GetConfigForArchive() (mainConfig []byte, modTime time.Time, deps map[string]*types.FileDependency, configPath string, err error)

	// GenerateHTMLMetricsReport generates an HTML-formatted metrics report.
	GenerateHTMLMetricsReport() string

	// GenerateStepsMetricsReport generates a plain-text step execution metrics report.
	GenerateStepsMetricsReport() string

	// GetMarshalledConfig retrieves the raw YAML configuration bytes and its modification time.
	GetMarshalledConfig() ([]byte, time.Time, error)
}
