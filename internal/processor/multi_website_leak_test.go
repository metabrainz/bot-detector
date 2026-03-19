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
	"regexp"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestMultiWebsite_NoGoroutineLeaks verifies that tailers don't leak goroutines
// when they exit naturally (e.g., due to ExitOnEOF).
func TestMultiWebsite_NoGoroutineLeaks(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "test.log")

	// Create log file with one line
	f, _ := os.Create(logPath)
	_, _ = fmt.Fprintf(f, "test.com 10.0.0.1 - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot\"\n")
	_ = f.Close()

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
		DryRun:  true,
		NowFunc: time.Now,
		Websites: []config.WebsiteConfig{
			{Name: "site1", VHosts: []string{"site1.com"}, LogPath: logPath},
		},
		VHostToWebsite: map[string]string{
			"site1.com": "site1",
		},
		UnknownVHosts:    make(map[string]bool),
		ExitOnEOF:        true, // Causes tailer to exit after reading
		UnknownVHostsMux: sync.Mutex{},
		CheckChainsFunc:  func(entry *app.LogEntry) {},
		LogFunc: func(level logging.LogLevel, tag string, format string, v ...interface{}) {
			// Silent
		},
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)

	// Force garbage collection and get baseline goroutine count
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baselineGoroutines := runtime.NumGoroutine()

	signalCh := make(chan os.Signal, 1)
	manager := NewMultiWebsiteTailerManager(p, signalCh)
	p.WebsiteTailerMgr = manager

	// Start tailer - it will read the file and exit due to ExitOnEOF
	manager.Start()

	// Wait for tailer to complete (liveLogTailerWithPath returns)
	time.Sleep(200 * time.Millisecond)

	// Check goroutine count BEFORE shutdown
	// The bug: signal forwarder goroutine is still blocked in select{}
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	midGoroutines := runtime.NumGoroutine()
	leakedBeforeShutdown := midGoroutines - baselineGoroutines

	t.Logf("Goroutine count before shutdown: baseline=%d, current=%d, leaked=%d",
		baselineGoroutines, midGoroutines, leakedBeforeShutdown)

	// With the bug, we have 2 leaked goroutines:
	// 1. The signal forwarder stuck in select{}
	// 2. The outer tailer goroutine waiting for liveLogTailerWithPath (which has returned)
	// Allow tolerance of 1 for timing issues
	if leakedBeforeShutdown > 1 {
		t.Errorf("Goroutine leak detected BEFORE shutdown: baseline=%d, current=%d, leaked=%d (expected ≤1)",
			baselineGoroutines, midGoroutines, leakedBeforeShutdown)

		buf := make([]byte, 1<<20)
		stackSize := runtime.Stack(buf, true)
		t.Logf("Goroutine stack traces:\n%s", buf[:stackSize])
	}

	// Shutdown
	close(signalCh)
	manager.Wait()

	// Check final count
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	finalGoroutines := runtime.NumGoroutine()

	t.Logf("Goroutine count after shutdown: baseline=%d, final=%d",
		baselineGoroutines, finalGoroutines)
}

// TestMultiWebsite_LongRunningNoLeak tests that a long-running tailer
// doesn't accumulate goroutines over time.
func TestMultiWebsite_LongRunningNoLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping long-running test in short mode")
	}

	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "test.log")

	// Create initial log file
	f, _ := os.Create(logPath)
	_ = f.Close()

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
		DryRun:  true,
		NowFunc: time.Now,
		Websites: []config.WebsiteConfig{
			{Name: "site1", VHosts: []string{"site1.com"}, LogPath: logPath},
		},
		VHostToWebsite: map[string]string{
			"site1.com": "site1",
		},
		UnknownVHosts:    make(map[string]bool),
		ExitOnEOF:        false, // Keep tailing
		UnknownVHostsMux: sync.Mutex{},
		CheckChainsFunc:  func(entry *app.LogEntry) {},
		LogFunc: func(level logging.LogLevel, tag string, format string, v ...interface{}) {
			// Silent
		},
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baselineGoroutines := runtime.NumGoroutine()

	signalCh := make(chan os.Signal, 1)
	manager := NewMultiWebsiteTailerManager(p, signalCh)
	p.WebsiteTailerMgr = manager

	manager.Start()
	time.Sleep(100 * time.Millisecond)

	// Simulate continuous operation with periodic writes
	for i := 0; i < 10; i++ {
		f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
		_, _ = fmt.Fprintf(f, "test.com 10.0.0.%d - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot\"\n", i+1)
		_ = f.Close()
		time.Sleep(50 * time.Millisecond)
	}

	close(signalCh)
	manager.Wait()

	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	finalGoroutines := runtime.NumGoroutine()
	leakedGoroutines := finalGoroutines - baselineGoroutines

	if leakedGoroutines > 3 {
		t.Errorf("Goroutine leak in long-running test: baseline=%d, final=%d, leaked=%d",
			baselineGoroutines, finalGoroutines, leakedGoroutines)
	}
}
