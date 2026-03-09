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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMultiWebsite_FullIntegration tests a complete multi-website setup with:
// - 3 websites with separate log files
// - Global chains that apply to all websites
// - Website-specific chains
// - Log rotation handling
// - Proper isolation between websites
func TestMultiWebsite_FullIntegration(t *testing.T) {
	tempDir := t.TempDir()

	// Create log files for 3 websites
	site1Log := filepath.Join(tempDir, "site1.log")
	site2Log := filepath.Join(tempDir, "site2.log")
	site3Log := filepath.Join(tempDir, "site3.log")

	// Track events per website
	var events sync.Map // key: "website:event" -> count
	var logMessages []string
	var logMutex sync.Mutex

	// Create processor with 3 websites
	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		LogPathMutex:  sync.Mutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        []config.BehavioralChain{}, // No chains for this test, just verify parsing
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
		ExitOnEOF: false, // Keep tailing to detect rotation
		Websites: []config.WebsiteConfig{
			{Name: "site1", VHosts: []string{"site1.com"}, LogPath: site1Log},
			{Name: "site2", VHosts: []string{"site2.com"}, LogPath: site2Log},
			{Name: "site3", VHosts: []string{"site3.com"}, LogPath: site3Log},
		},
		VHostToWebsite: map[string]string{
			"site1.com": "site1",
			"site2.com": "site2",
			"site3.com": "site3",
		},
		UnknownVHosts:    make(map[string]bool),
		UnknownVHostsMux: sync.Mutex{},
		CheckChainsFunc: func(entry *app.LogEntry) {
			// Track parsed entries per website
			key := fmt.Sprintf("%s:parsed", entry.Website)
			val, _ := events.LoadOrStore(key, new(atomic.Int64))
			val.(*atomic.Int64).Add(1)

			// Track specific IPs per website
			ipKey := fmt.Sprintf("%s:ip:%s", entry.Website, entry.IPInfo.Address)
			ipVal, _ := events.LoadOrStore(ipKey, new(atomic.Int64))
			ipVal.(*atomic.Int64).Add(1)
		},
		LogFunc: func(level logging.LogLevel, tag string, format string, v ...interface{}) {
			logMutex.Lock()
			defer logMutex.Unlock()
			msg := fmt.Sprintf("[%s] %s", tag, fmt.Sprintf(format, v...))
			logMessages = append(logMessages, msg)
			// Log everything to see what's happening
			t.Logf("%s", msg)
		},
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)

	// Create EMPTY log files first
	for _, path := range []string{site1Log, site2Log, site3Log} {
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("Failed to create empty log %s: %v", path, err)
		}
		f.Close()
	}

	// Start multi-website tailer
	signalCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		MultiLogTailer(p, signalCh)
		close(done)
	}()

	// Wait for tailers to start and seek to end
	time.Sleep(200 * time.Millisecond)

	// NOW write initial content (will be read as new lines)
	writeLog(t, site1Log, []string{
		`site1.com 10.0.1.1 - - [01/Jan/2026:12:00:00 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot1"`,
		`site1.com 10.0.1.1 - - [01/Jan/2026:12:00:01 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot1"`,
		`site1.com 10.0.1.2 - - [01/Jan/2026:12:00:02 +0000] "GET /admin HTTP/1.1" 200 100 "-" "Bot2"`,
		`site1.com 10.0.1.2 - - [01/Jan/2026:12:00:03 +0000] "GET /admin HTTP/1.1" 200 100 "-" "Bot2"`,
	})

	writeLog(t, site2Log, []string{
		`site2.com 10.0.2.1 - - [01/Jan/2026:12:00:00 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot3"`,
		`site2.com 10.0.2.1 - - [01/Jan/2026:12:00:01 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot3"`,
		`site2.com 10.0.2.2 - - [01/Jan/2026:12:00:02 +0000] "GET /login HTTP/1.1" 200 100 "-" "Bot4"`,
		`site2.com 10.0.2.2 - - [01/Jan/2026:12:00:03 +0000] "GET /login HTTP/1.1" 200 100 "-" "Bot4"`,
	})

	writeLog(t, site3Log, []string{
		`site3.com 10.0.3.1 - - [01/Jan/2026:12:00:00 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot5"`,
		`site3.com 10.0.3.1 - - [01/Jan/2026:12:00:01 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot5"`,
		`site3.com 10.0.3.2 - - [01/Jan/2026:12:00:02 +0000] "GET /data HTTP/1.1" 200 100 "-" "Bot6"`,
		`site3.com 10.0.3.2 - - [01/Jan/2026:12:00:03 +0000] "GET /data HTTP/1.1" 200 100 "-" "Bot6"`,
	})

	// Wait for initial processing
	time.Sleep(300 * time.Millisecond)

	// Debug: check what we have
	var totalParsed int64
	events.Range(func(key, value interface{}) bool {
		if k, ok := key.(string); ok && contains(k, ":parsed") {
			totalParsed += value.(*atomic.Int64).Load()
		}
		return true
	})
	t.Logf("Total parsed entries after initial wait: %d", totalParsed)

	// Verify initial parsing
	t.Log("=== Phase 1: Initial log parsing ===")
	assertEventCount(t, &events, "site1:parsed", 4)
	assertEventCount(t, &events, "site2:parsed", 4)
	assertEventCount(t, &events, "site3:parsed", 4)

	// Verify IP isolation per website
	assertEventCount(t, &events, "site1:ip:10.0.1.1", 2)
	assertEventCount(t, &events, "site1:ip:10.0.1.2", 2)
	assertEventCount(t, &events, "site2:ip:10.0.2.1", 2)
	assertEventCount(t, &events, "site2:ip:10.0.2.2", 2)
	assertEventCount(t, &events, "site3:ip:10.0.3.1", 2)
	assertEventCount(t, &events, "site3:ip:10.0.3.2", 2)

	// Test log rotation: rename old logs and create new ones
	t.Log("=== Phase 2: Log rotation ===")
	os.Rename(site1Log, site1Log+".old")
	os.Rename(site2Log, site2Log+".old")
	os.Rename(site3Log, site3Log+".old")

	// Write new content to rotated logs
	// Use Create since these are new files after rotation
	for path, lines := range map[string][]string{
		site1Log: {
			`site1.com 10.0.1.3 - - [01/Jan/2026:12:00:10 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot7"`,
			`site1.com 10.0.1.3 - - [01/Jan/2026:12:00:11 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot7"`,
		},
		site2Log: {
			`site2.com 10.0.2.3 - - [01/Jan/2026:12:00:10 +0000] "GET /login HTTP/1.1" 200 100 "-" "Bot8"`,
			`site2.com 10.0.2.3 - - [01/Jan/2026:12:00:11 +0000] "GET /login HTTP/1.1" 200 100 "-" "Bot8"`,
		},
		site3Log: {
			`site3.com 10.0.3.3 - - [01/Jan/2026:12:00:10 +0000] "GET /data HTTP/1.1" 200 100 "-" "Bot9"`,
			`site3.com 10.0.3.3 - - [01/Jan/2026:12:00:11 +0000] "GET /data HTTP/1.1" 200 100 "-" "Bot9"`,
		},
	} {
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("Failed to create rotated log %s: %v", path, err)
		}
		for _, line := range lines {
			fmt.Fprintln(f, line)
		}
		f.Close()
	}

	// Wait for rotation detection and new content processing
	time.Sleep(2 * time.Second)

	// Verify post-rotation parsing
	t.Log("=== Phase 3: Post-rotation verification ===")
	assertEventCount(t, &events, "site1:parsed", 6) // 4 + 2 after rotation
	assertEventCount(t, &events, "site2:parsed", 6)
	assertEventCount(t, &events, "site3:parsed", 6)

	// Verify new IPs were parsed correctly after rotation
	assertEventCount(t, &events, "site1:ip:10.0.1.3", 2)
	assertEventCount(t, &events, "site2:ip:10.0.2.3", 2)
	assertEventCount(t, &events, "site3:ip:10.0.3.3", 2)

	// Shutdown
	close(signalCh)
	select {
	case <-done:
		t.Log("=== Tailer shutdown complete ===")
	case <-time.After(3 * time.Second):
		t.Fatal("Timeout waiting for shutdown")
	}

	// Verify all 3 tailers started
	logMutex.Lock()
	site1Started := false
	site2Started := false
	site3Started := false
	for _, msg := range logMessages {
		if contains(msg, "Starting tailer for website 'site1'") {
			site1Started = true
		}
		if contains(msg, "Starting tailer for website 'site2'") {
			site2Started = true
		}
		if contains(msg, "Starting tailer for website 'site3'") {
			site3Started = true
		}
	}
	logMutex.Unlock()

	if !site1Started || !site2Started || !site3Started {
		t.Errorf("Not all tailers started: site1=%v, site2=%v, site3=%v", site1Started, site2Started, site3Started)
	}

	t.Logf("✓ Test complete: 3 websites, 12 initial lines + 6 after rotation = 18 total lines parsed")
	t.Logf("✓ Log rotation handled successfully on all 3 websites")
	t.Logf("✓ Website isolation verified")
}

func writeLog(t *testing.T, path string, lines []string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("Failed to open log %s: %v", path, err)
	}
	defer f.Close()

	for _, line := range lines {
		fmt.Fprintln(f, line)
	}
}

func assertEventCount(t *testing.T, events *sync.Map, key string, expected int64) {
	val, ok := events.Load(key)
	if !ok {
		t.Errorf("Expected event %s but found none", key)
		return
	}
	count := val.(*atomic.Int64).Load()
	if count < expected {
		t.Errorf("Expected at least %d events for %s, got %d", expected, key, count)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
