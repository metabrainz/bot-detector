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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMultiWebsiteTailerManager_DynamicAdd tests adding a website at runtime
func TestMultiWebsiteTailerManager_DynamicAdd(t *testing.T) {
	tempDir := t.TempDir()

	// Create initial log files
	log1 := filepath.Join(tempDir, "site1.log")
	log2 := filepath.Join(tempDir, "site2.log")

	for _, logPath := range []string{log1, log2} {
		f, _ := os.Create(logPath)
		_, _ = fmt.Fprintf(f, "test.com 10.0.0.1 - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot\"\n")
		_ = f.Close()
	}

	var linesProcessed int32
	var logMessages []string
	var logMutex sync.Mutex

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
			{Name: "site1", VHosts: []string{"site1.com"}, LogPath: log1},
		},
		VHostToWebsite: map[string]string{
			"site1.com": "site1",
		},
		UnknownVHosts: make(map[string]bool),
		ExitOnEOF:     true,
		UnknownVHostsMux: sync.Mutex{},
		ProcessLogLine: func(line string) {
			atomic.AddInt32(&linesProcessed, 1)
		},
		LogFunc: func(level logging.LogLevel, tag string, format string, v ...interface{}) {
			logMutex.Lock()
			defer logMutex.Unlock()
			logMessages = append(logMessages, fmt.Sprintf(format, v...))
		},
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)
	p.CheckChainsFunc = func(entry *app.LogEntry) {}

	signalCh := make(chan os.Signal, 1)
	manager := NewMultiWebsiteTailerManager(p, signalCh)
	p.WebsiteTailerMgr = manager

	// Start with one website
	manager.Start()

	// Wait for initial processing
	time.Sleep(100 * time.Millisecond)

	// Add a second website
	newWebsites := []config.WebsiteConfig{
		{Name: "site1", VHosts: []string{"site1.com"}, LogPath: log1},
		{Name: "site2", VHosts: []string{"site2.com"}, LogPath: log2},
	}
	p.Websites = newWebsites
	p.VHostToWebsite["site2.com"] = "site2"
	manager.UpdateWebsites(newWebsites)

	// Wait for new tailer to process
	time.Sleep(100 * time.Millisecond)

	// Shutdown
	close(signalCh)
	manager.Wait()

	// Verify both sites were processed
	total := atomic.LoadInt32(&linesProcessed)
	if total != 2 {
		t.Errorf("Expected 2 lines processed (1 per website), got %d", total)
	}

	// Verify log messages show both tailers started
	logMutex.Lock()
	defer logMutex.Unlock()

	foundSite1Start := false
	foundSite2Start := false
	for _, msg := range logMessages {
		if msg == "Starting tailer for website 'site1' on "+log1 {
			foundSite1Start = true
		}
		if msg == "Starting tailer for new website 'site2'" {
			foundSite2Start = true
		}
	}

	if !foundSite1Start {
		t.Error("Did not find log message for site1 tailer start")
	}
	if !foundSite2Start {
		t.Error("Did not find log message for site2 being added")
	}
}

// TestMultiWebsiteTailerManager_DynamicRemove tests removing a website at runtime
func TestMultiWebsiteTailerManager_DynamicRemove(t *testing.T) {
	tempDir := t.TempDir()

	log1 := filepath.Join(tempDir, "site1.log")
	log2 := filepath.Join(tempDir, "site2.log")

	for _, logPath := range []string{log1, log2} {
		f, _ := os.Create(logPath)
		_, _ = fmt.Fprintf(f, "test.com 10.0.0.1 - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot\"\n")
		_ = f.Close()
	}

	var logMessages []string
	var logMutex sync.Mutex

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
			{Name: "site1", VHosts: []string{"site1.com"}, LogPath: log1},
			{Name: "site2", VHosts: []string{"site2.com"}, LogPath: log2},
		},
		VHostToWebsite: map[string]string{
			"site1.com": "site1",
			"site2.com": "site2",
		},
		UnknownVHosts:  make(map[string]bool),
		ExitOnEOF:      true,
		UnknownVHostsMux: sync.Mutex{},
		ProcessLogLine: func(line string) {},
		LogFunc: func(level logging.LogLevel, tag string, format string, v ...interface{}) {
			logMutex.Lock()
			defer logMutex.Unlock()
			logMessages = append(logMessages, fmt.Sprintf(format, v...))
		},
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)
	p.CheckChainsFunc = func(entry *app.LogEntry) {}

	signalCh := make(chan os.Signal, 1)
	manager := NewMultiWebsiteTailerManager(p, signalCh)
	p.WebsiteTailerMgr = manager

	// Start with two websites
	manager.Start()

	// Wait for initial processing
	time.Sleep(100 * time.Millisecond)

	// Remove site2
	newWebsites := []config.WebsiteConfig{
		{Name: "site1", VHosts: []string{"site1.com"}, LogPath: log1},
	}
	p.Websites = newWebsites
	delete(p.VHostToWebsite, "site2.com")
	manager.UpdateWebsites(newWebsites)

	// Wait a bit
	time.Sleep(100 * time.Millisecond)

	// Shutdown
	close(signalCh)
	manager.Wait()

	// Verify log messages show site2 was stopped
	logMutex.Lock()
	defer logMutex.Unlock()

	foundSite2Stop := false
	for _, msg := range logMessages {
		if msg == "Stopping tailer for removed website 'site2'" {
			foundSite2Stop = true
		}
	}

	if !foundSite2Stop {
		t.Error("Did not find log message for site2 being removed")
	}
}

// TestMultiWebsiteTailerManager_LogPathChange tests changing a website's log path
func TestMultiWebsiteTailerManager_LogPathChange(t *testing.T) {
	tempDir := t.TempDir()

	log1 := filepath.Join(tempDir, "site1_old.log")
	log2 := filepath.Join(tempDir, "site1_new.log")

	for _, logPath := range []string{log1, log2} {
		f, _ := os.Create(logPath)
		_, _ = fmt.Fprintf(f, "test.com 10.0.0.1 - - [01/Jan/2026:12:00:00 +0000] \"GET /test HTTP/1.1\" 200 100 \"-\" \"Bot\"\n")
		_ = f.Close()
	}

	var logMessages []string
	var logMutex sync.Mutex

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
			{Name: "site1", VHosts: []string{"site1.com"}, LogPath: log1},
		},
		VHostToWebsite: map[string]string{
			"site1.com": "site1",
		},
		UnknownVHosts:  make(map[string]bool),
		ExitOnEOF:      true,
		UnknownVHostsMux: sync.Mutex{},
		ProcessLogLine: func(line string) {},
		LogFunc: func(level logging.LogLevel, tag string, format string, v ...interface{}) {
			logMutex.Lock()
			defer logMutex.Unlock()
			logMessages = append(logMessages, fmt.Sprintf(format, v...))
		},
	}

	p.Blocker = blocker.NewHAProxyBlocker(p, true)
	p.CheckChainsFunc = func(entry *app.LogEntry) {}

	signalCh := make(chan os.Signal, 1)
	manager := NewMultiWebsiteTailerManager(p, signalCh)
	p.WebsiteTailerMgr = manager

	// Start with old log path
	manager.Start()

	// Wait for initial processing
	time.Sleep(100 * time.Millisecond)

	// Change log path
	newWebsites := []config.WebsiteConfig{
		{Name: "site1", VHosts: []string{"site1.com"}, LogPath: log2},
	}
	p.Websites = newWebsites
	manager.UpdateWebsites(newWebsites)

	// Wait a bit
	time.Sleep(100 * time.Millisecond)

	// Shutdown
	close(signalCh)
	manager.Wait()

	// Verify log messages show restart
	logMutex.Lock()
	defer logMutex.Unlock()

	foundRestart := false
	for _, msg := range logMessages {
		if msg == "Log path changed for website 'site1', restarting tailer" {
			foundRestart = true
		}
	}

	if !foundRestart {
		t.Error("Did not find log message for log path change")
	}
}
