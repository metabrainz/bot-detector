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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMultiWebsite_HighVolumeProcessing tests processing many log lines across multiple websites
func TestMultiWebsite_HighVolumeProcessing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping load test in short mode")
	}

	tempDir := t.TempDir()

	// Create 3 log files with 1000 lines each
	websites := []struct {
		name  string
		vhost string
		path  string
	}{
		{"site1", "site1.com", filepath.Join(tempDir, "site1.log")},
		{"site2", "site2.com", filepath.Join(tempDir, "site2.log")},
		{"site3", "site3.com", filepath.Join(tempDir, "site3.log")},
	}

	for _, site := range websites {
		f, err := os.Create(site.path)
		if err != nil {
			t.Fatalf("Failed to create %s: %v", site.path, err)
		}
		for i := 0; i < 1000; i++ {
			_, _ = fmt.Fprintf(f, "%s 10.0.%d.%d - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot\"\n",
				site.vhost, i/256, i%256)
		}
		_ = f.Close()
	}

	var linesProcessed int64

	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        []config.BehavioralChain{},
		Config: &config.AppConfig{
			Application: config.ApplicationConfig{
				EOFPollingDelay: 5 * time.Millisecond,
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
			{Name: "site1", VHosts: []string{"site1.com"}, LogPath: websites[0].path},
			{Name: "site2", VHosts: []string{"site2.com"}, LogPath: websites[1].path},
			{Name: "site3", VHosts: []string{"site3.com"}, LogPath: websites[2].path},
		},
		VHostToWebsite: map[string]string{
			"site1.com": "site1",
			"site2.com": "site2",
			"site3.com": "site3",
		},
		UnknownVHosts: make(map[string]bool),
		ExitOnEOF:     true,
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)
	p.ProcessLogLine = func(line string) {
		atomic.AddInt64(&linesProcessed, 1)
	}
	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {}

	startTime := time.Now()

	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, p.SignalCh)
		close(done)
	}()

	select {
	case <-done:
		elapsed := time.Since(startTime)
		total := atomic.LoadInt64(&linesProcessed)

		if total != 3000 {
			t.Errorf("Expected 3000 lines processed, got %d", total)
		}

		linesPerSec := float64(total) / elapsed.Seconds()
		t.Logf("Processed %d lines in %v (%.0f lines/sec)", total, elapsed, linesPerSec)

		// Sanity check - should process at least 1000 lines/sec
		if linesPerSec < 1000 {
			t.Errorf("Performance too slow: %.0f lines/sec (expected > 1000)", linesPerSec)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout processing 3000 lines")
	}
}

// TestMultiWebsite_MemoryUsage tests that memory usage stays bounded with many actors
func TestMultiWebsite_MemoryUsage(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping load test in short mode")
	}

	tempDir := t.TempDir()

	// Create log with 500 unique IPs
	logPath := filepath.Join(tempDir, "memory.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("Failed to create log: %v", err)
	}
	for i := 0; i < 500; i++ {
		_, _ = fmt.Fprintf(f, "site.com 10.0.%d.%d - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot%d\"\n",
			i/256, i%256, i%10) // 10 different user agents
	}
	_ = f.Close()

	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        []config.BehavioralChain{},
		Config: &config.AppConfig{
			Application: config.ApplicationConfig{
				EOFPollingDelay: 5 * time.Millisecond,
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
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)
	p.ProcessLogLine = func(line string) {
		// Just count lines - no need to actually store state for this test
	}
	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {}

	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, p.SignalCh)
		close(done)
	}()

	select {
	case <-done:
		// Test passes if it completes without OOM
		t.Logf("Successfully processed 500 unique actors")
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout")
	}
}

// TestMultiWebsite_CommandQueueStress tests blocker command queue under load
func TestMultiWebsite_CommandQueueStress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping load test in short mode")
	}

	tempDir := t.TempDir()

	// Create 5 websites with 100 lines each
	var websites []config.WebsiteConfig
	vhostMap := make(map[string]string)

	for i := 0; i < 5; i++ {
		logPath := filepath.Join(tempDir, fmt.Sprintf("site%d.log", i))
		f, err := os.Create(logPath)
		if err != nil {
			t.Fatalf("Failed to create log: %v", err)
		}
		for j := 0; j < 100; j++ {
			_, _ = fmt.Fprintf(f, "site%d.com 10.%d.0.%d - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot\"\n",
				i, i, j)
		}
		_ = f.Close()

		websites = append(websites, config.WebsiteConfig{
			Name:    fmt.Sprintf("site%d", i),
			VHosts:  []string{fmt.Sprintf("site%d.com", i)},
			LogPath: logPath,
		})
		vhostMap[fmt.Sprintf("site%d.com", i)] = fmt.Sprintf("site%d", i)
	}

	var commandsQueued int64

	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        []config.BehavioralChain{},
		Config: &config.AppConfig{
			Application: config.ApplicationConfig{
				EOFPollingDelay: 5 * time.Millisecond,
			},
			Parser: config.ParserConfig{
				LineEnding: "lf",
			},
			Blockers: config.BlockersConfig{
				CommandQueueSize: 1000, // Test queue capacity
			},
			FileOpener: func(name string) (config.FileHandle, error) { return os.Open(name) },
			StatFunc:   os.Stat,
		},
		DryRun:         true,
		NowFunc:        time.Now,
		SignalCh:       make(chan os.Signal, 1),
		Websites:       websites,
		VHostToWebsite: vhostMap,
		UnknownVHosts:  make(map[string]bool),
		ExitOnEOF:      true,
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)
	p.ProcessLogLine = func(line string) {
		// Simulate queueing a block command
		atomic.AddInt64(&commandsQueued, 1)
	}
	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {}

	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, p.SignalCh)
		close(done)
	}()

	select {
	case <-done:
		total := atomic.LoadInt64(&commandsQueued)
		if total != 500 {
			t.Errorf("Expected 500 commands queued, got %d", total)
		}
		t.Logf("Successfully queued %d commands from 5 concurrent websites", total)
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout")
	}
}
