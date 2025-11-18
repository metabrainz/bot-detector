package cluster

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"bot-detector/internal/logging"
)

// LogFunc is the function signature for logging.
type LogFunc func(level logging.LogLevel, tag string, format string, v ...interface{})

// ConfigPoller polls the leader node for configuration updates.
type ConfigPoller struct {
	leaderAddress     string
	configPath        string
	pollInterval      time.Duration
	httpClient        *http.Client
	configReloadCh    chan<- struct{}
	shutdownCh        <-chan os.Signal
	logFunc           LogFunc
	lastModTime       time.Time
	lastConfigContent []byte
}

// ConfigPollerOptions contains configuration for the config poller.
type ConfigPollerOptions struct {
	LeaderAddress  string           // Leader HTTP address (e.g., "http://leader:8080")
	ConfigPath     string           // Local config file path
	PollInterval   time.Duration    // How often to poll for updates
	ConfigReloadCh chan<- struct{}  // Channel to signal config reload
	ShutdownCh     <-chan os.Signal // Shutdown signal channel
	LogFunc        LogFunc          // Logging function
	HTTPTimeout    time.Duration    // HTTP request timeout
}

// NewConfigPoller creates a new configuration poller for follower nodes.
func NewConfigPoller(opts ConfigPollerOptions) *ConfigPoller {
	if opts.HTTPTimeout == 0 {
		opts.HTTPTimeout = 10 * time.Second
	}

	return &ConfigPoller{
		leaderAddress:  opts.LeaderAddress,
		configPath:     opts.ConfigPath,
		pollInterval:   opts.PollInterval,
		configReloadCh: opts.ConfigReloadCh,
		shutdownCh:     opts.ShutdownCh,
		logFunc:        opts.LogFunc,
		httpClient: &http.Client{
			Timeout: opts.HTTPTimeout,
		},
	}
}

// Start begins polling the leader for configuration updates.
// This runs in a goroutine and should only be called once.
func (cp *ConfigPoller) Start() {
	cp.logFunc(logging.LevelInfo, "CLUSTER", "Starting config poller for leader at %s (interval: %s)", cp.leaderAddress, cp.pollInterval)

	// Initialize with current local config modification time
	if stat, err := os.Stat(cp.configPath); err == nil {
		cp.lastModTime = stat.ModTime()
		cp.logFunc(logging.LevelDebug, "CLUSTER", "Initial local config mod time: %s", cp.lastModTime.Format(time.RFC1123))
	}

	ticker := time.NewTicker(cp.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cp.poll()
		case <-cp.shutdownCh:
			cp.logFunc(logging.LevelInfo, "CLUSTER", "Config poller shutting down")
			return
		}
	}
}

// poll fetches the configuration from the leader and updates local config if needed.
func (cp *ConfigPoller) poll() {
	configURL := fmt.Sprintf("%s/config", cp.leaderAddress)

	// Create request
	req, err := http.NewRequest("GET", configURL, nil)
	if err != nil {
		cp.logFunc(logging.LevelError, "CLUSTER", "Failed to create config request: %v", err)
		return
	}

	// Add If-Modified-Since header to avoid downloading unchanged config
	if !cp.lastModTime.IsZero() {
		req.Header.Set("If-Modified-Since", cp.lastModTime.UTC().Format(http.TimeFormat))
	}

	// Make request
	resp, err := cp.httpClient.Do(req)
	if err != nil {
		cp.logFunc(logging.LevelError, "CLUSTER", "Failed to fetch config from leader: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Check response status
	if resp.StatusCode == http.StatusNotModified {
		cp.logFunc(logging.LevelDebug, "CLUSTER", "Config not modified on leader")
		return
	}

	if resp.StatusCode != http.StatusOK {
		cp.logFunc(logging.LevelError, "CLUSTER", "Leader returned error status: %d", resp.StatusCode)
		return
	}

	// Get Last-Modified header
	lastModifiedStr := resp.Header.Get("Last-Modified")
	if lastModifiedStr == "" {
		cp.logFunc(logging.LevelWarning, "CLUSTER", "Leader config response missing Last-Modified header")
		return
	}

	lastModified, err := http.ParseTime(lastModifiedStr)
	if err != nil {
		cp.logFunc(logging.LevelError, "CLUSTER", "Failed to parse Last-Modified header: %v", err)
		return
	}

	// Read config content
	configContent, err := io.ReadAll(resp.Body)
	if err != nil {
		cp.logFunc(logging.LevelError, "CLUSTER", "Failed to read config from leader: %v", err)
		return
	}

	// Check if content has actually changed (not just mod time)
	if string(configContent) == string(cp.lastConfigContent) {
		cp.logFunc(logging.LevelDebug, "CLUSTER", "Config content unchanged despite different mod time")
		cp.lastModTime = lastModified
		return
	}

	// Write new config to temporary file first
	tempPath := cp.configPath + ".tmp"
	if err := os.WriteFile(tempPath, configContent, 0600); err != nil {
		cp.logFunc(logging.LevelError, "CLUSTER", "Failed to write temporary config file: %v", err)
		return
	}

	// NOTE: We skip validation here to avoid import cycles.
	// The config will be validated when it's loaded by the main application.
	// If it's invalid, the reload will fail and the old config will remain in use.

	// Backup current config
	backupPath := cp.configPath + ".backup"
	if err := copyFile(cp.configPath, backupPath); err != nil {
		cp.logFunc(logging.LevelWarning, "CLUSTER", "Failed to create config backup: %v", err)
	}

	// Replace config file atomically
	if err := os.Rename(tempPath, cp.configPath); err != nil {
		cp.logFunc(logging.LevelError, "CLUSTER", "Failed to replace config file: %v", err)
		_ = os.Remove(tempPath)
		return
	}

	// Update tracking variables
	cp.lastModTime = lastModified
	cp.lastConfigContent = configContent

	cp.logFunc(logging.LevelInfo, "CLUSTER", "Updated config from leader (mod time: %s)", lastModified.Format(time.RFC1123))

	// Signal config reload
	select {
	case cp.configReloadCh <- struct{}{}:
		cp.logFunc(logging.LevelInfo, "CLUSTER", "Signaled config reload")
	default:
		cp.logFunc(logging.LevelWarning, "CLUSTER", "Config reload channel full, reload signal dropped")
	}
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = sourceFile.Close() }()

	// Ensure destination directory exists
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = destFile.Close() }()

	if _, err := io.Copy(destFile, sourceFile); err != nil {
		return err
	}

	return destFile.Sync()
}

// BootstrapOptions contains options for bootstrapping a follower node.
type BootstrapOptions struct {
	LeaderAddress string        // Leader HTTP address (e.g., "http://leader:8080")
	ConfigPath    string        // Local config file path to create
	LogFunc       LogFunc       // Logging function
	HTTPTimeout   time.Duration // HTTP request timeout
	ForceUpdate   bool          // Force update even if local config exists
}

// Bootstrap fetches the initial configuration from the leader and saves it locally.
// This should be called before starting the application to ensure the follower has
// a valid configuration file.
//
// If the config file already exists and ForceUpdate is false, this function will
// skip the bootstrap and return nil (allowing the existing config to be used).
//
// Returns an error if the bootstrap fails.
func Bootstrap(opts BootstrapOptions) error {
	if opts.HTTPTimeout == 0 {
		opts.HTTPTimeout = 10 * time.Second
	}

	// Check if config already exists (unless force update)
	if !opts.ForceUpdate {
		if _, err := os.Stat(opts.ConfigPath); err == nil {
			opts.LogFunc(logging.LevelInfo, "CLUSTER", "Local config exists, skipping bootstrap")
			return nil
		}
	}

	opts.LogFunc(logging.LevelInfo, "CLUSTER", "Bootstrapping config from leader at %s", opts.LeaderAddress)

	// Create HTTP client
	client := &http.Client{
		Timeout: opts.HTTPTimeout,
	}

	// Fetch config from leader
	configURL := fmt.Sprintf("%s/config", opts.LeaderAddress)
	resp, err := client.Get(configURL)
	if err != nil {
		return fmt.Errorf("failed to fetch config from leader: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("leader returned error status %d", resp.StatusCode)
	}

	// Read config content
	configContent, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read config from leader: %w", err)
	}

	// Ensure config directory exists
	configDir := filepath.Dir(opts.ConfigPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Write config to file
	if err := os.WriteFile(opts.ConfigPath, configContent, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	opts.LogFunc(logging.LevelInfo, "CLUSTER", "Successfully bootstrapped config from leader (%d bytes)", len(configContent))
	return nil
}
