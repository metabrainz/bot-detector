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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// setupTestProcessor creates a processor with minimal working configuration for multi-website tests
func setupTestProcessor(t *testing.T, websites []config.WebsiteConfig) *app.Processor {
	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        []config.BehavioralChain{},
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
		DryRun:        true,
		NowFunc:       time.Now,
		SignalCh:      make(chan os.Signal, 1),
		Websites:      websites,
		UnknownVHosts: make(map[string]bool),
		ExitOnEOF:     true,
	}

	// Build vhost map
	vhostMap := make(map[string]string)
	for _, ws := range websites {
		for _, vhost := range ws.VHosts {
			vhostMap[vhost] = ws.Name
		}
	}
	p.VHostToWebsite = vhostMap

	// Initialize blocker
	p.Blocker = blocker.NewHAProxyBlocker(p, true)

	// Set up a no-op CheckChainsFunc
	p.CheckChainsFunc = func(entry *app.LogEntry) {}

	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {
		t.Logf("[%s] %s", tag, fmt.Sprintf(format, v...))
	}

	return p
}

// TestMultiLogTailer_Integration tests concurrent tailing of multiple websites with signal shutdown
func TestMultiLogTailer_Integration(t *testing.T) {
	tempDir := t.TempDir()

	// Create 3 log files for 3 websites
	mainLog := filepath.Join(tempDir, "main.log")
	apiLog := filepath.Join(tempDir, "api.log")
	adminLog := filepath.Join(tempDir, "admin.log")

	// Create initial log files with content
	mainContent := `www.example.com 10.0.0.1 - - [01/Jan/2026:12:00:00 +0000] "GET /main HTTP/1.1" 200 100 "-" "MainBot"
www.example.com 10.0.0.2 - - [01/Jan/2026:12:00:01 +0000] "GET /main HTTP/1.1" 200 100 "-" "MainBot"
`
	apiContent := `api.example.com 10.0.1.1 - - [01/Jan/2026:12:00:00 +0000] "GET /api HTTP/1.1" 200 100 "-" "APIBot"
api.example.com 10.0.1.2 - - [01/Jan/2026:12:00:01 +0000] "GET /api HTTP/1.1" 200 100 "-" "APIBot"
`
	adminContent := `admin.example.com 10.0.2.1 - - [01/Jan/2026:12:00:00 +0000] "GET /admin HTTP/1.1" 200 100 "-" "AdminBot"
admin.example.com 10.0.2.2 - - [01/Jan/2026:12:00:01 +0000] "GET /admin HTTP/1.1" 200 100 "-" "AdminBot"
`

	if err := os.WriteFile(mainLog, []byte(mainContent), 0644); err != nil {
		t.Fatalf("Failed to create main log: %v", err)
	}
	if err := os.WriteFile(apiLog, []byte(apiContent), 0644); err != nil {
		t.Fatalf("Failed to create api log: %v", err)
	}
	if err := os.WriteFile(adminLog, []byte(adminContent), 0644); err != nil {
		t.Fatalf("Failed to create admin log: %v", err)
	}

	// Create processor with multi-website configuration
	p := setupTestProcessor(t, []config.WebsiteConfig{
		{Name: "main", VHosts: []string{"www.example.com"}, LogPath: mainLog},
		{Name: "api", VHosts: []string{"api.example.com"}, LogPath: apiLog},
		{Name: "admin", VHosts: []string{"admin.example.com"}, LogPath: adminLog},
	})

	// Start multi-log tailer
	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, p.SignalCh)
		close(done)
	}()

	// Wait for processing to complete (ExitOnEOF=true means it will exit after reading)
	select {
	case <-done:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for multi-log tailer to complete")
	}

	// Verify all lines were processed
	totalLines := p.Metrics.LinesProcessed.Load()
	if totalLines != 6 {
		t.Errorf("Expected 6 lines processed (2 per website), got %d", totalLines)
	}
}

// TestMultiLogTailer_SignalShutdown tests graceful shutdown with SIGTERM
func TestMultiLogTailer_SignalShutdown(t *testing.T) {
	tempDir := t.TempDir()

	// Create 2 log files
	log1 := filepath.Join(tempDir, "site1.log")
	log2 := filepath.Join(tempDir, "site2.log")

	// Create empty log files
	if err := os.WriteFile(log1, []byte{}, 0644); err != nil {
		t.Fatalf("Failed to create log1: %v", err)
	}
	if err := os.WriteFile(log2, []byte{}, 0644); err != nil {
		t.Fatalf("Failed to create log2: %v", err)
	}

	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        []config.BehavioralChain{},
		Config: &config.AppConfig{
			Application: config.ApplicationConfig{
				EOFPollingDelay: 50 * time.Millisecond,
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
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)
	p.ProcessLogLine = func(line string) {}
	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {
		t.Logf("[%s] %s", tag, fmt.Sprintf(format, v...))
	}

	// Start multi-log tailer
	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, p.SignalCh)
		close(done)
	}()

	// Give tailers time to start
	time.Sleep(100 * time.Millisecond)

	// Send shutdown signal by closing the channel (broadcasts to all goroutines)
	close(p.SignalCh)

	// Wait for graceful shutdown
	select {
	case <-done:
		// Success - all tailers shut down gracefully
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for graceful shutdown")
	}
}

// TestMultiLogTailer_ConcurrentWrites tests that concurrent log writes are processed correctly
func TestMultiLogTailer_ConcurrentWrites(t *testing.T) {
	tempDir := t.TempDir()

	log1 := filepath.Join(tempDir, "concurrent1.log")
	log2 := filepath.Join(tempDir, "concurrent2.log")

	// Create empty log files
	f1, err := os.Create(log1)
	if err != nil {
		t.Fatalf("Failed to create log1: %v", err)
	}
	_ = f1.Close()

	f2, err := os.Create(log2)
	if err != nil {
		t.Fatalf("Failed to create log2: %v", err)
	}
	_ = f2.Close()

	var linesProcessed int32

	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        []config.BehavioralChain{},
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
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)

	p.ProcessLogLine = func(line string) {
		// Simple check - just count non-empty lines
		if len(strings.TrimSpace(line)) > 0 && !strings.HasPrefix(line, "#") {
			atomic.AddInt32(&linesProcessed, 1)
		}
	}

	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {}

	// Start multi-log tailer
	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, p.SignalCh)
		close(done)
	}()

	// Give tailers time to start
	time.Sleep(100 * time.Millisecond)

	// Write lines concurrently to both log files
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		f, _ := os.OpenFile(log1, os.O_APPEND|os.O_WRONLY, 0644)
		defer func() { _ = f.Close() }()
		for i := 0; i < 5; i++ {
			_, _ = fmt.Fprintf(f, "site1.com 10.0.0.%d - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot\"\n", i)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	go func() {
		defer wg.Done()
		f, _ := os.OpenFile(log2, os.O_APPEND|os.O_WRONLY, 0644)
		defer func() { _ = f.Close() }()
		for i := 0; i < 5; i++ {
			_, _ = fmt.Fprintf(f, "site2.com 10.0.1.%d - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot\"\n", i)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	wg.Wait()

	// Wait a bit for processing
	time.Sleep(200 * time.Millisecond)

	// Shutdown by closing channel
	close(p.SignalCh)

	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for shutdown")
	}

	// Verify all lines were processed
	total := atomic.LoadInt32(&linesProcessed)
	if total != 10 {
		t.Errorf("Expected 10 lines processed, got %d", total)
	}
}
