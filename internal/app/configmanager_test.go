package app_test

import (
	"bot-detector/internal/app"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/metrics"
	"bot-detector/internal/store"
	"bot-detector/internal/testutil"
	"github.com/stretchr/testify/assert"
	"os"
	"path/filepath"
	"reflect"
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
	initialLoadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigFilePath: tempFile})
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
		ConfigFilePath: tempFile,
		ReloadOn:       "HUP", // Set for this test
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

func TestReloadConfiguration_PreservesFileOpener(t *testing.T) {
	// --- Setup ---
	testutil.ResetGlobalState()
	t.Cleanup(testutil.ResetGlobalState)

	// 1. Create a temporary YAML file.
	initialYAMLContent := `
version: "1.0"
application:
  log_level: "info"
`
	tempDir := t.TempDir()
	configFilePath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(configFilePath, []byte(initialYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write initial temp yaml file: %v", err)
	}
	fileInfo, err := os.Stat(configFilePath)
	if err != nil {
		t.Fatalf("Failed to stat temp yaml file: %v", err)
	}

	// 2. Create a mock FileOpener to check for pointer preservation.
	var fileOpenerCalled bool
	mockFileOpener := func(name string) (config.FileHandle, error) {
		fileOpenerCalled = true
		return os.Open(name)
	}

	// 3. Load initial config and create a processor with the mock function.
	loadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigFilePath: configFilePath})
	if err != nil {
		t.Fatalf("Initial config.LoadConfigFromYAML() failed: %v", err)
	}

	appConfig := &config.AppConfig{
		Application: loadedCfg.Application,
		FileOpener:  mockFileOpener,
		StatFunc:    func(name string) (os.FileInfo, error) { return os.Stat(name) },
		LastModTime: fileInfo.ModTime(),
	}

	processor := &app.Processor{
		ActivityMutex:  &sync.RWMutex{},
		ActivityStore:  make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:    &sync.RWMutex{},
		Metrics:        metrics.NewMetrics(),
		Config:         appConfig,
		Chains:         loadedCfg.Chains,
		LogFunc:        func(level logging.LogLevel, tag string, format string, args ...interface{}) {},
		ConfigFilePath: configFilePath,
	}

	// Make a copy of the old config for the reload function, as the real caller would.
	processor.ConfigMutex.RLock()
	oldConfigForComparison := processor.Config.Clone()
	processor.ConfigMutex.RUnlock()

	// --- Act ---
	// 4. Modify the config file on disk to trigger a change.
	modifiedYAMLContent := `
version: "1.0"
application:
  log_level: "debug" # Change log level
`
	if err := os.WriteFile(configFilePath, []byte(modifiedYAMLContent), 0644); err != nil {
		t.Fatalf("Failed to write modified temp yaml file: %v", err)
	}

	// 5. Trigger the configuration reload directly.
	app.ReloadConfiguration(processor, true, &oldConfigForComparison)

	// --- Assert ---
	// 6. Check if FileOpener is preserved after the reload.
	processor.ConfigMutex.RLock()
	reloadedFileOpener := processor.Config.FileOpener
	processor.ConfigMutex.RUnlock()

	assert.NotNil(t, reloadedFileOpener, "FileOpener should not be nil after reload")

	// 7. Call the reloaded FileOpener to ensure it's our original mock function.
	// This confirms the function pointer was preserved.
	_, err = reloadedFileOpener(configFilePath)
	assert.NoError(t, err, "FileOpener should be callable")
	assert.True(t, fileOpenerCalled, "The mocked FileOpener function should have been called")
}

func TestAppConfig_HasAllLoadedConfigFields(t *testing.T) {
	// This test ensures that when new fields are added to LoadedConfig,
	// they are also added to AppConfig (and thus copied in main.go and
	// configmanager.go). Fields that are intentionally excluded are listed below.
	excluded := map[string]bool{
		"Websites":       true, // handled separately in Processor
		"Chains":         true, // handled separately in Processor
		"LogFormatRegex": true, // handled separately in Processor
		"StatFunc":       true, // set independently
	}

	loadedType := reflect.TypeOf(config.LoadedConfig{})
	appType := reflect.TypeOf(config.AppConfig{})

	appFields := make(map[string]bool)
	for i := 0; i < appType.NumField(); i++ {
		appFields[appType.Field(i).Name] = true
	}

	for i := 0; i < loadedType.NumField(); i++ {
		field := loadedType.Field(i).Name
		if excluded[field] {
			continue
		}
		if !appFields[field] {
			t.Errorf("LoadedConfig has field %q that is missing from AppConfig (add it or exclude it in this test)", field)
		}
	}
}
