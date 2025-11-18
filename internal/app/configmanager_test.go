package app_test

import (
	"bot-detector/internal/app"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/metrics"
	"bot-detector/internal/store"
	"bot-detector/internal/testutil"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSignalReloader_Reload(t *testing.T) {
	// --- Setup ---
	testutil.ResetGlobalState()
	t.Cleanup(testutil.ResetGlobalState)

	// Isolate the log level for this test.
	originalLogLevel := logging.GetLogLevel()
	t.Cleanup(func() { logging.SetLogLevel(originalLogLevel.String()) })

	// 1. Create a temporary YAML file with initial content.
	initialYAMLContent := `
version: "1.0"
application:
  log_level: "info"
chains:
  - name: "InitialChain"
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {Path: "/initial"}}]
`
	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(tempFile, []byte(initialYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write initial temp yaml file: %v", err)
	}

	// Enable signal-based reloading for this test.
	// This is now set on the processor directly.

	// 2. Load the initial configuration.
	initialLoadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigPath: tempFile})
	if err != nil {
		t.Fatalf("Initial config.LoadConfigFromYAML() failed: %v", err)
	}

	// 3. Create the processor with the initial config.
	processor := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		Metrics:       metrics.NewMetrics(),
		Chains:        initialLoadedCfg.Chains,
		Config:        &config.AppConfig{},
		SignalCh:      make(chan os.Signal, 1), // Initialize the signal channel
		LogFunc:       func(level logging.LogLevel, tag string, format string, args ...interface{}) {},
		TestSignals: &app.TestSignals{
			// This signal is used by the test to wait for the reload to complete.
			ReloadDoneSignal: make(chan struct{}, 1),
		},
		ConfigPath: tempFile,
		ReloadOn:   "HUP", // Set for this test
	}

	// 4. Start the app.SignalReloader.
	stopWatcher := make(chan struct{})
	t.Cleanup(func() { close(stopWatcher) })
	go app.SignalReloader(processor, stopWatcher, processor.SignalCh)

	// --- Act ---
	// 5. Modify the YAML file on disk.
	modifiedYAMLContent := `
version: "1.0"
application:
  log_level: "debug" # Changed log level
chains:
  - name: "ReloadedChain" # Changed chain name
    match_key: "ip"
    action: "log"
    steps: [{field_matches: {Path: "/reloaded"}}]
`
	if err := os.WriteFile(tempFile, []byte(modifiedYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write modified temp yaml file: %v", err)
	}

	// 6. Send the SIGHUP signal to the current process to trigger the reload.
	processor.SignalCh <- syscall.SIGHUP

	// 7. Wait for the reload signal from the reloader.
	select {
	case <-processor.TestSignals.ReloadDoneSignal:
		// Reload completed successfully.
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for signal-based configuration reload.")
	}

	// --- Assert ---
	// 8. Check if the processor's state has been updated.
	processor.ConfigMutex.RLock()
	reloadedChains := processor.Chains
	processor.ConfigMutex.RUnlock()

	if len(reloadedChains) != 1 || reloadedChains[0].Name != "ReloadedChain" {
		t.Errorf("Expected chain to be 'ReloadedChain', but got: %+v", reloadedChains)
	}
	if logging.GetLogLevel() != logging.LevelDebug {
		t.Errorf("Expected log level to be updated to 'debug', but it was not.")
	}
}

// TestCompaction verifies that the state snapshot and journal truncation work correctly.
