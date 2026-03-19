package processor

import (
	"bot-detector/internal/app"
	"bot-detector/internal/blocker"
	"bot-detector/internal/checker"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/metrics"
	"bot-detector/internal/store"
	"bot-detector/internal/types"
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
		LogRegex:      regexp.MustCompile(`^(?P<VHost>\S+) (?P<IP>\S+) \S+ \S+ \[(?P<Timestamp>[^\]]+)\] "(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)" (?P<StatusCode>\d+) (?P<Size>\d+) "(?P<Referrer>[^"]*)" "(?P<UserAgent>[^"]*)"`),
		Config: &config.AppConfig{
			Application: config.ApplicationConfig{
				EOFPollingDelay: 10 * time.Millisecond,
			},
			Parser: config.ParserConfig{
				TimestampFormat: "02/Jan/2006:15:04:05 -0700",
				LineEnding:      "lf",
			},
			Checker: config.CheckerConfig{
				MaxTimeSinceLastHit: 10 * time.Second,
			},
			FileOpener: func(name string) (config.FileHandle, error) { return os.Open(name) },
			StatFunc:   os.Stat,
		},
		DryRun:    true,
		NowFunc:   time.Now,
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
	}

	// Define chains: 1 global + 1 per website
	p.Chains = []config.BehavioralChain{
		// Global chain: 2 requests to /api
		{
			Name:          "Global-API-Abuse",
			MatchKey:      "ip",
			Action:        "log",
			BlockDuration: 30 * time.Minute,
			Websites:      []string{}, // Empty = global
			StepsYAML: []config.StepDefYAML{
				{FieldMatches: map[string]interface{}{"Path": "/api"}},
				{FieldMatches: map[string]interface{}{"Path": "/api"}},
			},
		},
		// Site1-specific: 2 requests to /admin
		{
			Name:          "Site1-Admin-Access",
			MatchKey:      "ip",
			Action:        "log",
			BlockDuration: 30 * time.Minute,
			Websites:      []string{"site1"},
			StepsYAML: []config.StepDefYAML{
				{FieldMatches: map[string]interface{}{"Path": "/admin"}},
				{FieldMatches: map[string]interface{}{"Path": "/admin"}},
			},
		},
		// Site2-specific: 2 requests to /login
		{
			Name:          "Site2-Login-Abuse",
			MatchKey:      "ip",
			Action:        "log",
			BlockDuration: 30 * time.Minute,
			Websites:      []string{"site2"},
			StepsYAML: []config.StepDefYAML{
				{FieldMatches: map[string]interface{}{"Path": "/login"}},
				{FieldMatches: map[string]interface{}{"Path": "/login"}},
			},
		},
		// Site3-specific: 2 requests to /data
		{
			Name:          "Site3-Data-Access",
			MatchKey:      "ip",
			Action:        "log",
			BlockDuration: 30 * time.Minute,
			Websites:      []string{"site3"},
			StepsYAML: []config.StepDefYAML{
				{FieldMatches: map[string]interface{}{"Path": "/data"}},
				{FieldMatches: map[string]interface{}{"Path": "/data"}},
			},
		},
	}

	// Initialize chains (compile matchers)
	testFileDependencies := make(map[string]*types.FileDependency)
	for i := range p.Chains {
		chain := &p.Chains[i]
		// Initialize metrics
		chain.MetricsHitsCounter = new(atomic.Int64)
		chain.MetricsResetCounter = new(atomic.Int64)
		chain.MetricsCounter = new(atomic.Int64)
		chain.FieldMatchCounts = &sync.Map{}

		for j, stepYAML := range chain.StepsYAML {
			matchers, err := config.CompileMatchers(chain.Name, j, stepYAML.FieldMatches, testFileDependencies, "")
			if err != nil {
				t.Fatalf("Failed to compile matchers for chain '%s': %v", chain.Name, err)
			}
			chain.Steps = append(chain.Steps, config.StepDef{
				Order:    j + 1,
				Matchers: matchers,
			})
		}
	}

	// Categorize chains into website-specific and global
	p.WebsiteChains, p.GlobalChains = app.CategorizeChains(p.Chains)

	// Set up chain checking with event tracking
	p.CheckChainsFunc = func(entry *app.LogEntry) {
		// Track parsed entries per website
		key := fmt.Sprintf("%s:parsed", entry.Website)
		val, _ := events.LoadOrStore(key, new(atomic.Int64))
		val.(*atomic.Int64).Add(1)

		// Track specific IPs per website
		ipKey := fmt.Sprintf("%s:ip:%s", entry.Website, entry.IPInfo.Address)
		ipVal, _ := events.LoadOrStore(ipKey, new(atomic.Int64))
		ipVal.(*atomic.Int64).Add(1)

		// Run actual chain checking
		checker.CheckChains(p, entry)
	}

	// Set up logging
	p.LogFunc = func(level logging.LogLevel, tag string, format string, v ...interface{}) {
		logMutex.Lock()
		defer logMutex.Unlock()
		msg := fmt.Sprintf("[%s] %s", tag, fmt.Sprintf(format, v...))
		logMessages = append(logMessages, msg)
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)

	// Create EMPTY log files first
	for _, path := range []string{site1Log, site2Log, site3Log} {
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("Failed to create empty log %s: %v", path, err)
		}
		_ = f.Close()
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
		// Trigger global chain
		`site1.com 10.0.1.1 - - [01/Jan/2026:12:00:00 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot1"`,
		`site1.com 10.0.1.1 - - [01/Jan/2026:12:00:01 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot1"`,
		// Trigger site1-specific chain
		`site1.com 10.0.1.2 - - [01/Jan/2026:12:00:02 +0000] "GET /admin HTTP/1.1" 200 100 "-" "Bot2"`,
		`site1.com 10.0.1.2 - - [01/Jan/2026:12:00:03 +0000] "GET /admin HTTP/1.1" 200 100 "-" "Bot2"`,
		// Try to trigger site2 chain (should NOT work from site1)
		`site1.com 10.0.1.3 - - [01/Jan/2026:12:00:04 +0000] "GET /login HTTP/1.1" 200 100 "-" "Bot3"`,
		`site1.com 10.0.1.3 - - [01/Jan/2026:12:00:05 +0000] "GET /login HTTP/1.1" 200 100 "-" "Bot3"`,
	})

	writeLog(t, site2Log, []string{
		// Trigger global chain
		`site2.com 10.0.2.1 - - [01/Jan/2026:12:00:00 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot4"`,
		`site2.com 10.0.2.1 - - [01/Jan/2026:12:00:01 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot4"`,
		// Trigger site2-specific chain
		`site2.com 10.0.2.2 - - [01/Jan/2026:12:00:02 +0000] "GET /login HTTP/1.1" 200 100 "-" "Bot5"`,
		`site2.com 10.0.2.2 - - [01/Jan/2026:12:00:03 +0000] "GET /login HTTP/1.1" 200 100 "-" "Bot5"`,
		// Try to trigger site3 chain (should NOT work from site2)
		`site2.com 10.0.2.3 - - [01/Jan/2026:12:00:04 +0000] "GET /data HTTP/1.1" 200 100 "-" "Bot6"`,
		`site2.com 10.0.2.3 - - [01/Jan/2026:12:00:05 +0000] "GET /data HTTP/1.1" 200 100 "-" "Bot6"`,
	})

	writeLog(t, site3Log, []string{
		// Trigger global chain
		`site3.com 10.0.3.1 - - [01/Jan/2026:12:00:00 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot7"`,
		`site3.com 10.0.3.1 - - [01/Jan/2026:12:00:01 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot7"`,
		// Trigger site3-specific chain
		`site3.com 10.0.3.2 - - [01/Jan/2026:12:00:02 +0000] "GET /data HTTP/1.1" 200 100 "-" "Bot8"`,
		`site3.com 10.0.3.2 - - [01/Jan/2026:12:00:03 +0000] "GET /data HTTP/1.1" 200 100 "-" "Bot8"`,
		// Try to trigger site1 chain (should NOT work from site3)
		`site3.com 10.0.3.3 - - [01/Jan/2026:12:00:04 +0000] "GET /admin HTTP/1.1" 200 100 "-" "Bot9"`,
		`site3.com 10.0.3.3 - - [01/Jan/2026:12:00:05 +0000] "GET /admin HTTP/1.1" 200 100 "-" "Bot9"`,
	})

	// Wait for initial processing
	time.Sleep(300 * time.Millisecond)

	// Flush entry buffer to ensure all entries are processed
	checker.FlushEntryBuffer(p)
	time.Sleep(100 * time.Millisecond)

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
	assertEventCount(t, &events, "site1:parsed", 6)
	assertEventCount(t, &events, "site2:parsed", 6)
	assertEventCount(t, &events, "site3:parsed", 6)

	// Verify IP tracking per website
	assertEventCount(t, &events, "site1:ip:10.0.1.1", 2)
	assertEventCount(t, &events, "site1:ip:10.0.1.2", 2)
	assertEventCount(t, &events, "site1:ip:10.0.1.3", 2)
	assertEventCount(t, &events, "site2:ip:10.0.2.1", 2)
	assertEventCount(t, &events, "site2:ip:10.0.2.2", 2)
	assertEventCount(t, &events, "site2:ip:10.0.2.3", 2)
	assertEventCount(t, &events, "site3:ip:10.0.3.1", 2)
	assertEventCount(t, &events, "site3:ip:10.0.3.2", 2)
	assertEventCount(t, &events, "site3:ip:10.0.3.3", 2)

	// Verify chain completions from log messages
	t.Log("=== Verifying chain completions ===")

	logMutex.Lock()
	globalCompletions := 0
	site1Completions := 0
	site2Completions := 0
	site3Completions := 0
	site1WrongChain := 0 // site1 trying to trigger site2 chain
	site2WrongChain := 0 // site2 trying to trigger site3 chain
	site3WrongChain := 0 // site3 trying to trigger site1 chain

	for _, msg := range logMessages {
		if contains(msg, "Global-API-Abuse") && contains(msg, "completed by IP") {
			globalCompletions++
		}
		if contains(msg, "Site1-Admin-Access") && contains(msg, "completed by IP") {
			site1Completions++
		}
		if contains(msg, "Site2-Login-Abuse") && contains(msg, "completed by IP") {
			if contains(msg, "10.0.2.2") {
				site2Completions++ // Correct
			} else if contains(msg, "10.0.1.3") {
				site1WrongChain++ // Wrong - site1 triggered site2 chain
			}
		}
		if contains(msg, "Site3-Data-Access") && contains(msg, "completed by IP") {
			if contains(msg, "10.0.3.2") {
				site3Completions++ // Correct
			} else if contains(msg, "10.0.2.3") {
				site2WrongChain++ // Wrong - site2 triggered site3 chain
			}
		}
		if contains(msg, "Site1-Admin-Access") && contains(msg, "10.0.3.3") {
			site3WrongChain++ // Wrong - site3 triggered site1 chain
		}
	}
	logMutex.Unlock()

	// Global chain should complete 3 times (once per website)
	if globalCompletions != 3 {
		t.Errorf("Expected 3 global chain completions, got %d", globalCompletions)
	}

	// Each site-specific chain should complete exactly once
	if site1Completions != 1 {
		t.Errorf("Expected 1 site1 chain completion, got %d", site1Completions)
	}
	if site2Completions != 1 {
		t.Errorf("Expected 1 site2 chain completion, got %d", site2Completions)
	}
	if site3Completions != 1 {
		t.Errorf("Expected 1 site3 chain completion, got %d", site3Completions)
	}

	// Verify isolation: wrong chains should NOT trigger
	if site1WrongChain > 0 {
		t.Errorf("Site1 incorrectly triggered Site2 chain %d times", site1WrongChain)
	}
	if site2WrongChain > 0 {
		t.Errorf("Site2 incorrectly triggered Site3 chain %d times", site2WrongChain)
	}
	if site3WrongChain > 0 {
		t.Errorf("Site3 incorrectly triggered Site1 chain %d times", site3WrongChain)
	}

	// Test log rotation: rename old logs and create new ones
	t.Log("=== Phase 2: Log rotation ===")
	_ = os.Rename(site1Log, site1Log+".old")
	_ = os.Rename(site2Log, site2Log+".old")
	_ = os.Rename(site3Log, site3Log+".old")

	// Write new content to rotated logs
	// Use Create since these are new files after rotation
	for path, lines := range map[string][]string{
		site1Log: {
			`site1.com 10.0.1.10 - - [01/Jan/2026:12:00:10 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot10"`,
			`site1.com 10.0.1.10 - - [01/Jan/2026:12:00:11 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot10"`,
		},
		site2Log: {
			`site2.com 10.0.2.10 - - [01/Jan/2026:12:00:10 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot11"`,
			`site2.com 10.0.2.10 - - [01/Jan/2026:12:00:11 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot11"`,
		},
		site3Log: {
			`site3.com 10.0.3.10 - - [01/Jan/2026:12:00:10 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot12"`,
			`site3.com 10.0.3.10 - - [01/Jan/2026:12:00:11 +0000] "GET /api HTTP/1.1" 200 100 "-" "Bot12"`,
		},
	} {
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("Failed to create rotated log %s: %v", path, err)
		}
		for _, line := range lines {
			_, _ = fmt.Fprintln(f, line)
		}
		_ = f.Close()
	}

	// Wait for rotation detection and new content processing
	time.Sleep(2 * time.Second)

	// Verify post-rotation parsing
	t.Log("=== Phase 3: Post-rotation verification ===")
	assertEventCount(t, &events, "site1:parsed", 8) // 6 + 2 after rotation
	assertEventCount(t, &events, "site2:parsed", 8)
	assertEventCount(t, &events, "site3:parsed", 8)

	// Verify new IPs were parsed correctly after rotation
	assertEventCount(t, &events, "site1:ip:10.0.1.10", 2)
	assertEventCount(t, &events, "site2:ip:10.0.2.10", 2)
	assertEventCount(t, &events, "site3:ip:10.0.3.10", 2)

	// Verify global chain completed again after rotation (3 more times)
	logMutex.Lock()
	postRotationGlobal := 0
	for _, msg := range logMessages {
		if contains(msg, "Global-API-Abuse") && contains(msg, "completed by IP") &&
			(contains(msg, "10.0.1.10") || contains(msg, "10.0.2.10") || contains(msg, "10.0.3.10")) {
			postRotationGlobal++
		}
	}
	logMutex.Unlock()

	if postRotationGlobal != 3 {
		t.Errorf("Expected 3 global chain completions after rotation, got %d", postRotationGlobal)
	}

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

	t.Logf("✓ Test complete: 3 websites, 18 initial lines + 6 after rotation = 24 total lines parsed")
	t.Logf("✓ Log rotation handled successfully on all 3 websites")
	t.Logf("✓ Website isolation verified")
	t.Logf("✓ Global chain completed 6 times (3 before + 3 after rotation)")
	t.Logf("✓ Site-specific chains completed correctly (1 per site)")
	t.Logf("✓ Cross-site chain triggering prevented (isolation verified)")
}

func writeLog(t *testing.T, path string, lines []string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("Failed to open log %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	for _, line := range lines {
		_, _ = fmt.Fprintln(f, line)
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
