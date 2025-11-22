package cluster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"bot-detector/internal/logging"
)

// schemeRegex is used to check if a URL has a scheme.
var schemeRegex = regexp.MustCompile(`^[^:]+://`)

// ensureURIScheme ensures that a given address has an HTTP scheme.
// If no scheme is present, it prepends "http://".
func ensureURIScheme(address, protocol string) string {
	if !schemeRegex.MatchString(address) {
		return protocol + "://" + address
	}
	return address
}

// LogFunc is the function signature for logging.
type LogFunc func(level logging.LogLevel, tag string, format string, v ...interface{})

// ConfigPoller polls the leader node for configuration updates.
type ConfigPoller struct {
	leaderAddress  string
	configFilePath string
	pollInterval   time.Duration
	httpClient     *http.Client
	configReloadCh chan<- struct{}
	shutdownCh     <-chan os.Signal
	logFunc        LogFunc
	lastModTime    time.Time
	lastETag       string
}

// ConfigPollerOptions contains configuration for the config poller.
type ConfigPollerOptions struct {
	LeaderAddress  string           // Leader HTTP address (e.g., "http://leader:8080")
	ConfigFilePath string           // Local config file path
	PollInterval   time.Duration    // How often to poll for updates
	ConfigReloadCh chan<- struct{}  // Channel to signal config reload
	ShutdownCh     <-chan os.Signal // Shutdown signal channel
	LogFunc        LogFunc          // Logging function
	HTTPTimeout    time.Duration    // HTTP request timeout
	Protocol       string           // Protocol for cluster communication (e.g., "http", "https")
}

// NewConfigPoller creates a new configuration poller for follower nodes.
func NewConfigPoller(opts ConfigPollerOptions) *ConfigPoller {
	if opts.HTTPTimeout == 0 {
		opts.HTTPTimeout = 10 * time.Second
	}

	return &ConfigPoller{
		leaderAddress:  ensureURIScheme(opts.LeaderAddress, opts.Protocol),
		configFilePath: opts.ConfigFilePath,
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
	if stat, err := os.Stat(cp.configFilePath); err == nil {
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

// poll fetches the configuration archive from the leader and updates local config if needed.
func (cp *ConfigPoller) poll() {
	archiveURL := fmt.Sprintf("%s/config/archive", cp.leaderAddress)

	// Create request
	req, err := http.NewRequest("GET", archiveURL, nil)
	if err != nil {
		cp.logFunc(logging.LevelError, "CLUSTER", "Failed to create archive request: %v", err)
		return
	}

	// Add If-Modified-Since header to avoid downloading unchanged config
	if !cp.lastModTime.IsZero() {
		req.Header.Set("If-Modified-Since", cp.lastModTime.UTC().Format(http.TimeFormat))
	}

	// Make request
	resp, err := cp.httpClient.Do(req)
	if err != nil {
		cp.logFunc(logging.LevelError, "CLUSTER", "Failed to fetch archive from leader: %v", err)
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
		cp.logFunc(logging.LevelWarning, "CLUSTER", "Leader archive response missing Last-Modified header")
		return
	}

	lastModified, err := http.ParseTime(lastModifiedStr)
	if err != nil {
		cp.logFunc(logging.LevelError, "CLUSTER", "Failed to parse Last-Modified header: %v", err)
		return
	}

	// Get ETag for change detection
	etag := resp.Header.Get("ETag")
	if etag != "" && etag == cp.lastETag {
		cp.logFunc(logging.LevelDebug, "CLUSTER", "ETag unchanged, skipping update")
		return
	}

	// Read archive content
	archiveData, err := io.ReadAll(resp.Body)
	if err != nil {
		cp.logFunc(logging.LevelError, "CLUSTER", "Failed to read archive from leader: %v", err)
		return
	}

	// Backup current config and dependencies
	configDir := filepath.Dir(cp.configFilePath)
	backupDir := configDir + ".backup"
	if err := os.RemoveAll(backupDir); err != nil && !os.IsNotExist(err) {
		cp.logFunc(logging.LevelWarning, "CLUSTER", "Failed to remove old backup: %v", err)
	}
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		cp.logFunc(logging.LevelWarning, "CLUSTER", "Failed to create backup directory: %v", err)
	} else {
		// Copy config directory contents to backup (simple approach - copy only config.yaml)
		if err := copyFile(cp.configFilePath, filepath.Join(backupDir, "config.yaml")); err != nil {
			cp.logFunc(logging.LevelWarning, "CLUSTER", "Failed to create config backup: %v", err)
		}
	}

	// Extract archive to config directory
	// This will overwrite existing files but preserve FOLLOW file
	if err := extractTarGz(archiveData, configDir, etag, cp.logFunc); err != nil {
		cp.logFunc(logging.LevelError, "CLUSTER", "Failed to extract config archive: %v", err)
		return
	}

	// Update tracking variables
	cp.lastModTime = lastModified
	cp.lastETag = etag

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
	LeaderAddress  string        // Leader HTTP address (e.g., "http://leader:8080")
	ConfigFilePath string        // Local config file path to create
	LogFunc        LogFunc       // Logging function
	HTTPTimeout    time.Duration // HTTP request timeout
	ForceUpdate    bool          // Force update even if local config exists
	Protocol       string        // Protocol for cluster communication (e.g., "http", "https")
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
		if _, err := os.Stat(opts.ConfigFilePath); err == nil {
			opts.LogFunc(logging.LevelInfo, "CLUSTER", "Local config exists, skipping bootstrap")
			return nil
		}
	}

	opts.LogFunc(logging.LevelInfo, "CLUSTER", "Bootstrapping config from leader at %s", opts.LeaderAddress)

	// Create HTTP client
	client := &http.Client{
		Timeout: opts.HTTPTimeout,
	}

	// Fetch config archive from leader
	leaderAddress := ensureURIScheme(opts.LeaderAddress, opts.Protocol)
	archiveURL := fmt.Sprintf("%s/config/archive", leaderAddress)
	resp, err := client.Get(archiveURL)
	if err != nil {
		return fmt.Errorf("failed to fetch config archive from leader: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("leader returned error status %d", resp.StatusCode)
	}

	// Read archive content
	archiveData, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read config archive from leader: %w", err)
	}

	// Get ETag for checksum verification
	etag := resp.Header.Get("ETag")

	// Ensure config directory exists
	configDir := filepath.Dir(opts.ConfigFilePath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Extract archive to config directory
	if err := extractTarGz(archiveData, configDir, etag, opts.LogFunc); err != nil {
		return fmt.Errorf("failed to extract config archive: %w", err)
	}

	opts.LogFunc(logging.LevelInfo, "CLUSTER", "Successfully bootstrapped config from leader (%d bytes)", len(archiveData))
	return nil
}

// extractTarGz extracts a tar.gz archive to the specified directory.
// It excludes the FOLLOW file for safety (to prevent overwriting role determination).
// Verifies the archive checksum against the provided ETag (SHA256).
func extractTarGz(archiveData []byte, targetDir string, etag string, logFunc LogFunc) error {
	// Verify checksum if ETag is provided
	if etag != "" {
		// Remove quotes from ETag if present
		etag = strings.Trim(etag, "\"")

		// Compute SHA256 of archive data
		hash := sha256.Sum256(archiveData)
		computedHash := fmt.Sprintf("%x", hash)

		if computedHash != etag {
			return fmt.Errorf("archive checksum verification failed (expected: %s, got: %s)", etag, computedHash)
		}
		logFunc(logging.LevelDebug, "CLUSTER", "Archive checksum verified: %s", etag)
	}

	// Create gzip reader
	gzReader, err := gzip.NewReader(bytes.NewReader(archiveData))
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	// Create tar reader
	tarReader := tar.NewReader(gzReader)

	// Extract files
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar header: %w", err)
		}

		// Skip FOLLOW file for safety
		if header.Name == "FOLLOW" || strings.HasSuffix(header.Name, "/FOLLOW") {
			logFunc(logging.LevelDebug, "CLUSTER", "Skipping FOLLOW file in archive")
			continue
		}

		// Only process regular files
		if header.Typeflag != tar.TypeReg {
			continue
		}

		// Build target path
		targetPath := filepath.Join(targetDir, header.Name)

		// Ensure target directory exists
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", targetPath, err)
		}

		// Create target file
		outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
		if err != nil {
			return fmt.Errorf("failed to create file %s: %w", targetPath, err)
		}

		// Copy file contents
		if _, err := io.Copy(outFile, tarReader); err != nil {
			_ = outFile.Close()
			return fmt.Errorf("failed to write file %s: %w", targetPath, err)
		}

		_ = outFile.Close()
		logFunc(logging.LevelDebug, "CLUSTER", "Extracted: %s", header.Name)
	}

	return nil
}
