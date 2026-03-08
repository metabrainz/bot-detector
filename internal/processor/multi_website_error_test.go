package processor

import (
	"bot-detector/internal/app"
	"bot-detector/internal/blocker"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/metrics"
	"bot-detector/internal/store"
	"fmt"
	"os"
	"regexp"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMultiWebsite_MissingLogFile tests behavior when a log file doesn't exist
func TestMultiWebsite_MissingLogFile(t *testing.T) {
	tempDir := t.TempDir()

	// Create only one log file, leave the other missing
	log1 := filepath.Join(tempDir, "exists.log")
	log2 := filepath.Join(tempDir, "missing.log")

	if err := os.WriteFile(log1, []byte("test.com 10.0.0.1 - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot\"\n"), 0644); err != nil {
		t.Fatalf("Failed to create log1: %v", err)
	}
	// Don't create log2

	var errorLogged int32

	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		LogPathMutex:  sync.Mutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        []config.BehavioralChain{},
		LogRegex:      regexp.MustCompile(`^(?P<VHost>\S+) (?P<IP>\S+) \S+ \S+ \[(?P<Timestamp>[^\]]+)\] "(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)" (?P<StatusCode>\d+) (?P<Size>\d+) "(?P<Referrer>[^"]*)" "(?P<UserAgent>[^"]*)"`),
		Config: &config.AppConfig{
			Application: config.ApplicationConfig{
				EOFPollingDelay: 10 * time.Millisecond,
			},
			Parser: config.ParserConfig{
				LineEnding: "lf",
			},
			FileOpener: func(name string) (config.FileHandle, error) { return os.Open(name) },
			StatFunc:   os.Stat,
		},
		DryRun:   true,
		NowFunc:  time.Now,
		SignalCh: make(chan os.Signal, 1),
		Websites: []config.WebsiteConfig{
			{Name: "site1", VHosts: []string{"site1.com"}, LogPath: log1},
			{Name: "site2", VHosts: []string{"site2.com"}, LogPath: log2},
		},
		VHostToWebsite: map[string]string{
			"site1.com": "site1",
			"site2.com": "site2",
		},
		UnknownVHosts: make(map[string]bool),
		ExitOnEOF:     true,
		UnknownVHostsMux: sync.Mutex{},
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)
	p.ProcessLogLine = func(line string) {}
	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {
		msg := fmt.Sprintf(format, v...)
		if strings.Contains(msg, "Failed to open") || strings.Contains(msg, "no such file") {
			atomic.AddInt32(&errorLogged, 1)
		}
		t.Logf("[%s] %s", tag, msg)
	}

	// Start multi-log tailer
	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, p.SignalCh)
		close(done)
	}()

	// Give the manager time to start listening for signals
	time.Sleep(50 * time.Millisecond)

	// Wait for error to be logged, then shutdown
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if atomic.LoadInt32(&errorLogged) > 0 {
				// Wait a bit more to ensure both tailers have started
				time.Sleep(100 * time.Millisecond)
				// Error logged, send shutdown signal
				p.SignalCh <- os.Interrupt
				goto waitDone
			}
		case <-timeout:
			t.Fatal("Timeout waiting for error to be logged")
		}
	}

waitDone:
	select {
	case <-done:
		// Should complete - one tailer fails, one succeeds
		if atomic.LoadInt32(&errorLogged) == 0 {
			t.Error("Expected error to be logged for missing file")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for shutdown")
	}
}

// TestMultiWebsite_UnknownVHost tests handling of log entries with unknown vhosts
func TestMultiWebsite_UnknownVHost(t *testing.T) {
	tempDir := t.TempDir()

	logPath := filepath.Join(tempDir, "test.log")
	content := `known.com 10.0.0.1 - - [01/Jan/2026:12:00:00 +0000] "GET /test HTTP/1.1" 200 100 "-" "Bot"
unknown.com 10.0.0.2 - - [01/Jan/2026:12:00:01 +0000] "GET /test HTTP/1.1" 200 100 "-" "Bot"
another-unknown.com 10.0.0.3 - - [01/Jan/2026:12:00:02 +0000] "GET /test HTTP/1.1" 200 100 "-" "Bot"
known.com 10.0.0.4 - - [01/Jan/2026:12:00:03 +0000] "GET /test HTTP/1.1" 200 100 "-" "Bot"
`
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create log: %v", err)
	}

	var unknownVHostWarnings int32

	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		LogPathMutex:  sync.Mutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        []config.BehavioralChain{},
		LogRegex:      regexp.MustCompile(`^(?P<VHost>\S+) (?P<IP>\S+) \S+ \S+ \[(?P<Timestamp>[^\]]+)\] "(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)" (?P<StatusCode>\d+) (?P<Size>\d+) "(?P<Referrer>[^"]*)" "(?P<UserAgent>[^"]*)"`),
		Config: &config.AppConfig{
			Application: config.ApplicationConfig{
				EOFPollingDelay: 10 * time.Millisecond,
			},
			Parser: config.ParserConfig{
				LineEnding: "lf",
			},
			FileOpener: func(name string) (config.FileHandle, error) { return os.Open(name) },
			StatFunc:   os.Stat,
		},
		DryRun:   true,
		NowFunc:  time.Now,
		SignalCh: make(chan os.Signal, 1),
		Websites: []config.WebsiteConfig{
			{Name: "site1", VHosts: []string{"known.com"}, LogPath: logPath},
		},
		VHostToWebsite: map[string]string{
			"known.com": "site1",
		},
		UnknownVHosts: make(map[string]bool),
		ExitOnEOF:     true,
		UnknownVHostsMux: sync.Mutex{},
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)

	// Mock CheckChainsFunc to detect unknown vhosts (simulates real checker behavior)
	var entriesProcessed int32
	p.CheckChainsFunc = func(entry *app.LogEntry) {
		atomic.AddInt32(&entriesProcessed, 1)
		if entry.VHost != "" {
			if _, known := p.VHostToWebsite[entry.VHost]; !known {
				p.UnknownVHostsMux.Lock()
				if !p.UnknownVHosts[entry.VHost] {
					p.UnknownVHosts[entry.VHost] = true
					atomic.AddInt32(&unknownVHostWarnings, 1)
				}
				p.UnknownVHostsMux.Unlock()
			}
		}
	}

	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {}

	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, p.SignalCh)
		close(done)
	}()

	select {
	case <-done:
		warnings := atomic.LoadInt32(&unknownVHostWarnings)
		entries := atomic.LoadInt32(&entriesProcessed)
		
		t.Logf("Processed %d entries, %d unknown vhost warnings", entries, warnings)
		
		if warnings != 2 {
			t.Errorf("Expected 2 unknown vhost warnings, got %d", warnings)
		}

		// Verify UnknownVHosts map
		p.UnknownVHostsMux.Lock()
		if !p.UnknownVHosts["unknown.com"] {
			t.Error("Expected 'unknown.com' in UnknownVHosts map")
		}
		if !p.UnknownVHosts["another-unknown.com"] {
			t.Error("Expected 'another-unknown.com' in UnknownVHosts map")
		}
		if p.UnknownVHosts["known.com"] {
			t.Error("'known.com' should not be in UnknownVHosts map")
		}
		p.UnknownVHostsMux.Unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout")
	}
}

// TestMultiWebsite_EmptyLogFiles tests handling of empty log files
func TestMultiWebsite_EmptyLogFiles(t *testing.T) {
	tempDir := t.TempDir()

	log1 := filepath.Join(tempDir, "empty1.log")
	log2 := filepath.Join(tempDir, "empty2.log")

	// Create empty files
	for _, logPath := range []string{log1, log2} {
		if err := os.WriteFile(logPath, []byte{}, 0644); err != nil {
			t.Fatalf("Failed to create log: %v", err)
		}
	}

	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		LogPathMutex:  sync.Mutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        []config.BehavioralChain{},
		LogRegex:      regexp.MustCompile(`^(?P<VHost>\S+) (?P<IP>\S+) \S+ \S+ \[(?P<Timestamp>[^\]]+)\] "(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)" (?P<StatusCode>\d+) (?P<Size>\d+) "(?P<Referrer>[^"]*)" "(?P<UserAgent>[^"]*)"`),
		Config: &config.AppConfig{
			Application: config.ApplicationConfig{
				EOFPollingDelay: 10 * time.Millisecond,
			},
			Parser: config.ParserConfig{
				LineEnding: "lf",
			},
			FileOpener: func(name string) (config.FileHandle, error) { return os.Open(name) },
			StatFunc:   os.Stat,
		},
		DryRun:   true,
		NowFunc:  time.Now,
		SignalCh: make(chan os.Signal, 1),
		Websites: []config.WebsiteConfig{
			{Name: "site1", VHosts: []string{"site1.com"}, LogPath: log1},
			{Name: "site2", VHosts: []string{"site2.com"}, LogPath: log2},
		},
		VHostToWebsite: map[string]string{
			"site1.com": "site1",
			"site2.com": "site2",
		},
		UnknownVHosts: make(map[string]bool),
		ExitOnEOF:     true,
		UnknownVHostsMux: sync.Mutex{},
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)
	p.ProcessLogLine = func(line string) {
		t.Errorf("ProcessLogLine should not be called for empty files, got: %s", line)
	}
	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {}

	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, p.SignalCh)
		close(done)
	}()

	select {
	case <-done:
		// Success - should handle empty files gracefully
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout")
	}
}

// TestMultiWebsite_MalformedLogLines tests handling of unparseable log lines
func TestMultiWebsite_MalformedLogLines(t *testing.T) {
	tempDir := t.TempDir()

	logPath := filepath.Join(tempDir, "malformed.log")
	content := `site.com 10.0.0.1 - - [01/Jan/2026:12:00:00 +0000] "GET /test HTTP/1.1" 200 100 "-" "Bot"
this is not a valid log line
another malformed line without proper format
site.com 10.0.0.2 - - [01/Jan/2026:12:00:01 +0000] "GET /test HTTP/1.1" 200 100 "-" "Bot"
`
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create log: %v", err)
	}

	var linesProcessed int32

	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		LogPathMutex:  sync.Mutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        []config.BehavioralChain{},
		LogRegex:      regexp.MustCompile(`^(?P<VHost>\S+) (?P<IP>\S+) \S+ \S+ \[(?P<Timestamp>[^\]]+)\] "(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)" (?P<StatusCode>\d+) (?P<Size>\d+) "(?P<Referrer>[^"]*)" "(?P<UserAgent>[^"]*)"`),
		Config: &config.AppConfig{
			Application: config.ApplicationConfig{
				EOFPollingDelay: 10 * time.Millisecond,
			},
			Parser: config.ParserConfig{
				LineEnding: "lf",
			},
			FileOpener: func(name string) (config.FileHandle, error) { return os.Open(name) },
			StatFunc:   os.Stat,
		},
		DryRun:   true,
		NowFunc:  time.Now,
		SignalCh: make(chan os.Signal, 1),
		Websites: []config.WebsiteConfig{
			{Name: "site1", VHosts: []string{"site.com"}, LogPath: logPath},
		},
		VHostToWebsite: map[string]string{
			"site.com": "site1",
		},
		UnknownVHosts: make(map[string]bool),
		ExitOnEOF:     true,
		UnknownVHostsMux: sync.Mutex{},
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)
	p.ProcessLogLine = func(line string) {
		// All lines are passed to ProcessLogLine, even malformed ones
		atomic.AddInt32(&linesProcessed, 1)
	}
	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {}

	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, p.SignalCh)
		close(done)
	}()

	select {
	case <-done:
		// Should process all 4 lines (including malformed ones)
		total := atomic.LoadInt32(&linesProcessed)
		if total != 4 {
			t.Errorf("Expected 4 lines processed, got %d", total)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout")
	}
}
