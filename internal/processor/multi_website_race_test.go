package processor

import (
	"bot-detector/internal/app"
	"bot-detector/internal/blocker"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/metrics"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

// TestMultiWebsite_ConcurrentAccess tests concurrent log processing with -race flag
// Run with: go test -race -run TestMultiWebsite_ConcurrentAccess
func TestMultiWebsite_ConcurrentAccess(t *testing.T) {
	tempDir := t.TempDir()

	// Create 3 log files
	logs := []string{
		filepath.Join(tempDir, "site1.log"),
		filepath.Join(tempDir, "site2.log"),
		filepath.Join(tempDir, "site3.log"),
	}

	for _, logPath := range logs {
		content := `site.com 10.0.0.1 - - [01/Jan/2026:12:00:00 +0000] "GET /test HTTP/1.1" 200 100 "-" "Bot"
site.com 10.0.0.2 - - [01/Jan/2026:12:00:01 +0000] "GET /test HTTP/1.1" 200 100 "-" "Bot"
`
		if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
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
				TimestampFormat: "02/Jan/2006:15:04:05 -0700",
				LineEnding:      "lf",
			},
			FileOpener: func(name string) (config.FileHandle, error) { return os.Open(name) },
			StatFunc:   os.Stat,
		},
		DryRun:   true,
		NowFunc:  time.Now,
		SignalCh: make(chan os.Signal, 1),
		Websites: []config.WebsiteConfig{
			{Name: "site1", VHosts: []string{"site1.com"}, LogPath: logs[0]},
			{Name: "site2", VHosts: []string{"site2.com"}, LogPath: logs[1]},
			{Name: "site3", VHosts: []string{"site3.com"}, LogPath: logs[2]},
		},
		VHostToWebsite: map[string]string{
			"site1.com": "site1",
			"site2.com": "site2",
			"site3.com": "site3",
		},
		UnknownVHosts:    make(map[string]bool),
		ExitOnEOF:        true,
		UnknownVHostsMux: sync.Mutex{},
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)
	p.CheckChainsFunc = func(entry *app.LogEntry) {}

	// ProcessLogLine accesses shared state - this will trigger race detector if unsafe
	p.ProcessLogLine = func(line string) {
		// Access ActivityStore (shared map)
		p.ActivityMutex.Lock()
		actor := store.Actor{
			IPInfo: utils.IPInfo{Address: "10.0.0.1", Version: utils.VersionIPv4},
			UA:     "Bot",
		}
		if _, exists := p.ActivityStore[actor]; !exists {
			p.ActivityStore[actor] = &store.ActorActivity{
				LastRequestTime: time.Now(),
			}
		}
		p.ActivityMutex.Unlock()

		// Access Metrics (shared)
		p.Metrics.LinesProcessed.Add(1)

		// Access UnknownVHosts (shared map)
		p.UnknownVHostsMux.Lock()
		p.UnknownVHosts["test.com"] = true
		p.UnknownVHostsMux.Unlock()
	}

	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {}

	// Start multi-log tailer - will process concurrently
	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, p.SignalCh)
		close(done)
	}()

	select {
	case <-done:
		// Success - no race conditions detected
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for processing")
	}
}

// TestMultiWebsite_LogPathMutex tests that LogPathMutex prevents races
func TestMultiWebsite_LogPathMutex(t *testing.T) {
	tempDir := t.TempDir()

	log1 := filepath.Join(tempDir, "log1.log")
	log2 := filepath.Join(tempDir, "log2.log")

	// Create logs with content
	content := "test.com 10.0.0.1 - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot\"\n"
	for _, logPath := range []string{log1, log2} {
		if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
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
				TimestampFormat: "02/Jan/2006:15:04:05 -0700",
				LineEnding:      "lf",
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
		UnknownVHosts:    make(map[string]bool),
		ExitOnEOF:        true,
		UnknownVHostsMux: sync.Mutex{},
		LogPath:          "initial.log", // Will be overwritten by each tailer
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)
	p.CheckChainsFunc = func(entry *app.LogEntry) {}
	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {}

	// The test is that this doesn't trigger race detector
	// LogPathMutex protects concurrent access to p.LogPath
	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, p.SignalCh)
		close(done)
	}()

	select {
	case <-done:
		// Success - no race detected
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout")
	}
}

// TestMultiWebsite_UnknownVHostsConcurrency tests UnknownVHosts map concurrent access
func TestMultiWebsite_UnknownVHostsConcurrency(t *testing.T) {
	tempDir := t.TempDir()

	// Create log files with unknown vhosts
	logs := make([]string, 5)
	for i := 0; i < 5; i++ {
		logPath := filepath.Join(tempDir, fmt.Sprintf("site%d.log", i))
		content := fmt.Sprintf("unknown%d.com 10.0.0.1 - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot\"\n", i)
		if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to create log: %v", err)
		}
		logs[i] = logPath
	}

	websites := make([]config.WebsiteConfig, 5)
	vhostMap := make(map[string]string)
	for i := 0; i < 5; i++ {
		websites[i] = config.WebsiteConfig{
			Name:    fmt.Sprintf("site%d", i),
			VHosts:  []string{fmt.Sprintf("site%d.com", i)},
			LogPath: logs[i],
		}
		vhostMap[fmt.Sprintf("site%d.com", i)] = fmt.Sprintf("site%d", i)
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
				TimestampFormat: "02/Jan/2006:15:04:05 -0700",
				LineEnding:      "lf",
			},
			FileOpener: func(name string) (config.FileHandle, error) { return os.Open(name) },
			StatFunc:   os.Stat,
		},
		DryRun:           true,
		NowFunc:          time.Now,
		SignalCh:         make(chan os.Signal, 1),
		Websites:         websites,
		VHostToWebsite:   vhostMap,
		CatchAllWebsite:  "", // No catch-all website
		UnknownVHosts:    make(map[string]bool),
		ExitOnEOF:        true,
		UnknownVHostsMux: sync.Mutex{},
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)

	// Simulate checking for unknown vhosts (this happens in real processing)
	p.CheckChainsFunc = func(entry *app.LogEntry) {
		// Check if vhost is known
		if entry.VHost != "" {
			if _, known := p.VHostToWebsite[entry.VHost]; !known {
				// Access UnknownVHosts map - must be thread-safe
				p.UnknownVHostsMux.Lock()
				if !p.UnknownVHosts[entry.VHost] {
					p.UnknownVHosts[entry.VHost] = true
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
		// Verify unknown vhosts were tracked
		p.UnknownVHostsMux.Lock()
		unknownCount := len(p.UnknownVHosts)
		p.UnknownVHostsMux.Unlock()

		if unknownCount < 1 || unknownCount > 5 {
			t.Errorf("Expected 1-5 unknown vhosts, got %d", unknownCount)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout")
	}
}
