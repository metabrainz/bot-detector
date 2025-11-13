package main

import (
	"bot-detector/internal/logging"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestProcessorWithFileDeps creates a new Processor instance for testing,
// pre-populating it with a mock AppConfig that includes a FileDependencies map.
func newTestProcessorWithFileDeps(t *testing.T, config *AppConfig, logFunc func(level logging.LogLevel, tag string, format string, v ...interface{})) *Processor {
	t.Helper()
	if config == nil {
		config = &AppConfig{}
	}
	if config.FileDependencies == nil {
		config.FileDependencies = make(map[string]*FileDependency)
	}
	p := newTestProcessor(config, nil) // Use the existing newTestProcessor
	p.LogFunc = logFunc                // Set the log function here
	return p
}

// TestFileDependency_ContentChange verifies that ConfigWatcher detects a file content change
// and triggers a reload, and LoadConfigFromYAML re-reads the new content.
func TestFileDependency_ContentChange(t *testing.T) {
	resetGlobalState()

	tempDir := t.TempDir()
	testFilePath := filepath.Join(tempDir, "change_file.txt")
	configPath := filepath.Join(tempDir, "config.yaml")

	// Create initial file content
	initialContent := "old_line1\nold_line2"
	if err := os.WriteFile(testFilePath, []byte(initialContent), 0644); err != nil {
		t.Fatalf("Failed to write initial test file: %v", err)
	}

	// Create config.yaml that references the test file
	configContent := `
version: "1.0"
chains:
  - name: "test_chain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          Path: "file:` + testFilePath + `"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// --- Initial Load ---
	loadedCfg, err := LoadConfigFromYAML(configPath)
	if err != nil {
		t.Fatalf("Initial LoadConfigFromYAML failed: %v", err)
	}
	if loadedCfg == nil {
		t.Fatal("Loaded config is nil")
	}

	// Capture logs to check for reload messages
	var capturedLogs []string
	var logMutex sync.Mutex
	logCaptureFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		defer logMutex.Unlock()
		capturedLogs = append(capturedLogs, fmt.Sprintf("[%s] %s: %s", level, tag, fmt.Sprintf(format, args...)))
	}

	// Create a processor with the loaded config
	p := newTestProcessorWithFileDeps(t, &AppConfig{
		PollingInterval:  10 * time.Millisecond, // Short polling interval for test
		LastModTime:      time.Now(),            // Set a LastModTime for the config itself
		FileDependencies: loadedCfg.FileDependencies,
	}, logCaptureFunc)
	p.ConfigPath = configPath

	// Setup channels for ConfigWatcher
	stopCh := make(chan struct{})
	reloadDoneCh := make(chan struct{})
	p.TestSignals = &TestSignals{ReloadDoneSignal: reloadDoneCh}

	go ConfigWatcher(p, stopCh)
	defer close(stopCh)

	// --- Act 1: Modify the file content ---
	// Ensure enough time passes for ModTime to be different
	time.Sleep(20 * time.Millisecond)
	newContent := "new_line1\nnew_line2\nnew_line3"
	if err := os.WriteFile(testFilePath, []byte(newContent), 0644); err != nil {
		t.Fatalf("Failed to write new test file content: %v", err)
	}

	// --- Assert 1: Wait for reload to be detected and completed ---
	select {
	case <-reloadDoneCh:
		// Reload completed
	case <-time.After(2 * time.Second):
		t.Fatalf("Timed out waiting for config reload after file change. Logs:\n%s", strings.Join(capturedLogs, "\n"))
	}

	// --- Assert 2: Verify new content is loaded ---
	p.ConfigMutex.RLock()
	reloadedFileDep, ok := p.Config.FileDependencies[testFilePath]
	p.ConfigMutex.RUnlock()

	if !ok {
		t.Fatalf("File dependency '%s' not found in reloaded config", testFilePath)
	}
	expectedContent := []string{"new_line1", "new_line2", "new_line3"}
	if !reflect.DeepEqual(reloadedFileDep.Content, expectedContent) {
		t.Errorf("FileDependency Content mismatch after reload.\nGot:  %v\nWant: %v", reloadedFileDep.Content, expectedContent)
	}
	if reloadedFileDep.Status != FileStatusLoaded {
		t.Errorf("FileDependency Status mismatch after reload. Got '%s', want '%s'", reloadedFileDep.Status, FileStatusLoaded)
	}
	if reloadedFileDep.Error != nil {
		t.Errorf("FileDependency Error mismatch after reload. Got '%v', want nil", reloadedFileDep.Error)
	}

	// Verify log message for file change
	logMutex.Lock()
	foundLog := false
	for _, log := range capturedLogs {
		if strings.Contains(log, "Modified file dependencies: '"+testFilePath+"'") {
			foundLog = true
			break
		}
	}
	logMutex.Unlock()
	if !foundLog {
		t.Errorf("Expected log message for file dependency change not found.")
		fmt.Println("Captured Logs (TestFileDependency_ContentChange):")
		for _, log := range capturedLogs {
			fmt.Println(log)
		}
	}
}

// TestFileDependency_MissingFile verifies that a missing file dependency is correctly handled.
func TestFileDependency_MissingFile(t *testing.T) {
	resetGlobalState()

	tempDir := t.TempDir()
	testFilePath := filepath.Join(tempDir, "non_existent_file.txt") // This file will not be created
	configPath := filepath.Join(tempDir, "config.yaml")

	// Create config.yaml that references the non-existent file
	configContent := `
version: "1.0"
chains:
  - name: "test_chain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          Path: "file:` + testFilePath + `"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// Capture logs to check for warnings
	var capturedLogs []string
	var logMutex sync.Mutex
	originalLogOutput := logging.LogOutput
	logging.LogOutput = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		defer logMutex.Unlock()
		capturedLogs = append(capturedLogs, fmt.Sprintf("[%s] %s: %s", level, tag, fmt.Sprintf(format, args...)))
	}
	t.Cleanup(func() { logging.LogOutput = originalLogOutput })

	// --- Act ---
	loadedCfg, err := LoadConfigFromYAML(configPath)
	if err != nil {
		t.Fatalf("LoadConfigFromYAML failed: %v", err)
	}

	// --- Assert ---
	if loadedCfg == nil {
		t.Fatal("Loaded config is nil")
	}
	if len(loadedCfg.FileDependencies) != 1 {
		t.Fatalf("Expected 1 file dependency, got %d", len(loadedCfg.FileDependencies))
	}

	fileDep, ok := loadedCfg.FileDependencies[testFilePath]
	if !ok {
		t.Fatalf("Expected file dependency for '%s' not found", testFilePath)
	}

	if fileDep.Status != FileStatusMissing {
		t.Errorf("FileDependency Status mismatch. Got '%s', want '%s'", fileDep.Status, FileStatusMissing)
	}
	if fileDep.Error == nil {
		t.Error("FileDependency Error is nil, expected an error for missing file")
	}
	if !strings.Contains(fileDep.Error.Error(), "no such file or directory") {
		t.Errorf("Expected error to contain 'no such file or directory', got '%v'", fileDep.Error)
	}
	if len(fileDep.Content) != 0 {
		t.Errorf("FileDependency Content mismatch. Got %v, want empty", fileDep.Content)
	}

	// Verify warning was logged by compileStringMatcher
	logMutex.Lock()
	foundWarning := false
	for _, log := range capturedLogs {
		if strings.Contains(log, "file matcher '"+testFilePath+"' does not exist or is inaccessible") {
			foundWarning = true
			break
		}
	}
	logMutex.Unlock()
	if !foundWarning {
		t.Error("Expected a warning log about missing file, but none was found.")
		fmt.Println("Captured Logs (TestFileDependency_MissingFile):")
		for _, log := range capturedLogs {
			fmt.Println(log)
		}
	}
}

// TestFileDependency_FileReappears verifies that if a missing file reappears,
// ConfigWatcher detects it and triggers a reload, and LoadConfigFromYAML successfully loads it.
func TestFileDependency_FileReappears(t *testing.T) {
	resetGlobalState()

	tempDir := t.TempDir()
	testFilePath := filepath.Join(tempDir, "reappear_file.txt")
	configPath := filepath.Join(tempDir, "config.yaml")

	// Create config.yaml that references the file (initially missing)
	configContent := `
version: "1.0"
chains:
  - name: "test_chain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          Path: "file:` + testFilePath + `"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// --- Initial Load (file is missing) ---
	loadedCfg, err := LoadConfigFromYAML(configPath)
	if err != nil {
		t.Fatalf("Initial LoadConfigFromYAML failed: %v", err)
	}
	if loadedCfg == nil {
		t.Fatal("Loaded config is nil")
	}
	initialFileDep, ok := loadedCfg.FileDependencies[testFilePath]
	if !ok || initialFileDep.Status != FileStatusMissing {
		t.Fatalf("Expected file dependency to be initially missing, got status '%s'", initialFileDep.Status)
	}

	// Capture logs to check for reload messages
	var capturedLogs []string
	var logMutex sync.Mutex
	logCaptureFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		defer logMutex.Unlock()
		capturedLogs = append(capturedLogs, fmt.Sprintf("[%s] %s: %s", level, tag, fmt.Sprintf(format, args...)))
	}

	// Create a processor with the loaded config
	p := newTestProcessorWithFileDeps(t, &AppConfig{
		PollingInterval:  10 * time.Millisecond, // Short polling interval for test
		LastModTime:      time.Now(),            // Set a LastModTime for the config itself
		FileDependencies: loadedCfg.FileDependencies,
	}, logCaptureFunc)
	p.ConfigPath = configPath

	// Setup channels for ConfigWatcher
	stopCh := make(chan struct{})
	reloadDoneCh := make(chan struct{})
	p.TestSignals = &TestSignals{ReloadDoneSignal: reloadDoneCh}

	go ConfigWatcher(p, stopCh)
	defer close(stopCh)

	// --- Act 1: Create the missing file ---
	time.Sleep(20 * time.Millisecond) // Ensure enough time passes for ModTime to be different
	reappearedContent := "reappeared_line"
	if err := os.WriteFile(testFilePath, []byte(reappearedContent), 0644); err != nil {
		t.Fatalf("Failed to create reappeared test file: %v", err)
	}

	// --- Assert 1: Wait for reload to be detected and completed ---
	select {
	case <-reloadDoneCh:
		// Reload completed
	case <-time.After(2 * time.Second):
		t.Fatalf("Timed out waiting for config reload after file reappearance. Logs:\n%s", strings.Join(capturedLogs, "\n"))
	}

	// --- Assert 2: Verify new content is loaded ---
	p.ConfigMutex.RLock()
	reloadedFileDep, ok := p.Config.FileDependencies[testFilePath]
	p.ConfigMutex.RUnlock()

	if !ok {
		t.Fatalf("File dependency '%s' not found in reloaded config", testFilePath)
	}
	expectedContent := []string{"reappeared_line"}
	if !reflect.DeepEqual(reloadedFileDep.Content, expectedContent) {
		t.Errorf("FileDependency Content mismatch after reappearance.\nGot:  %v\nWant: %v", reloadedFileDep.Content, expectedContent)
	}
	if reloadedFileDep.Status != FileStatusLoaded {
		t.Errorf("FileDependency Status mismatch after reappearance. Got '%s', want '%s'", reloadedFileDep.Status, FileStatusLoaded)
	}
	if reloadedFileDep.Error != nil {
		t.Errorf("FileDependency Error mismatch after reappearance. Got '%v', want nil", reloadedFileDep.Error)
	}

	// Verify log message for file reappearance
	logMutex.Lock()
	foundLog := false
	for _, log := range capturedLogs {
		if strings.Contains(log, "Modified file dependencies: '"+testFilePath+"'") {
			foundLog = true
			break
		}
	}
	logMutex.Unlock()
	if !foundLog {
		t.Errorf("Expected log message for file reappearance not found.")
		fmt.Println("Captured Logs (TestFileDependency_FileReappears):")
		for _, log := range capturedLogs {
			fmt.Println(log)
		}
	}
}

// TestFileDependency_FileDisappears verifies that if a loaded file disappears,
// ConfigWatcher detects it and triggers a reload, and LoadConfigFromYAML marks it as FileStatusMissing.
func TestFileDependency_FileDisappears(t *testing.T) {
	resetGlobalState()

	tempDir := t.TempDir()
	testFilePath := filepath.Join(tempDir, "disappear_file.txt")
	configPath := filepath.Join(tempDir, "config.yaml")

	// Create initial file content
	initialContent := "initial_line"
	if err := os.WriteFile(testFilePath, []byte(initialContent), 0644); err != nil {
		t.Fatalf("Failed to write initial test file: %v", err)
	}

	// Create config.yaml that references the test file
	configContent := `
version: "1.0"
chains:
  - name: "test_chain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          Path: "file:` + testFilePath + `"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// --- Initial Load ---
	loadedCfg, err := LoadConfigFromYAML(configPath)
	if err != nil {
		t.Fatalf("Initial LoadConfigFromYAML failed: %v", err)
	}
	if loadedCfg == nil {
		t.Fatal("Loaded config is nil")
	}
	initialFileDep, ok := loadedCfg.FileDependencies[testFilePath]
	if !ok || initialFileDep.Status != FileStatusLoaded {
		t.Fatalf("Expected file dependency to be initially loaded, got status '%s'", initialFileDep.Status)
	}

	// Capture logs to check for reload messages
	var capturedLogs []string
	var logMutex sync.Mutex
	logCaptureFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		defer logMutex.Unlock()
		capturedLogs = append(capturedLogs, fmt.Sprintf("[%s] %s: %s", level, tag, fmt.Sprintf(format, args...)))
	}

	// Create a processor with the loaded config
	p := newTestProcessorWithFileDeps(t, &AppConfig{
		PollingInterval:  10 * time.Millisecond, // Short polling interval for test
		LastModTime:      time.Now(),            // Set a LastModTime for the config itself
		FileDependencies: loadedCfg.FileDependencies,
	}, logCaptureFunc)
	p.ConfigPath = configPath

	// Setup channels for ConfigWatcher
	stopCh := make(chan struct{})
	reloadDoneCh := make(chan struct{})
	p.TestSignals = &TestSignals{ReloadDoneSignal: reloadDoneCh}

	go ConfigWatcher(p, stopCh)
	defer close(stopCh)

	// --- Act 1: Delete the file ---
	time.Sleep(20 * time.Millisecond) // Ensure enough time passes for ModTime to be different
	if err := os.Remove(testFilePath); err != nil {
		t.Fatalf("Failed to delete test file: %v", err)
	}

	// --- Assert 1: Wait for reload to be detected and completed ---
	select {
	case <-reloadDoneCh:
		// Reload completed
	case <-time.After(2 * time.Second):
		t.Fatalf("Timed out waiting for config reload after file disappearance. Logs:\n%s", strings.Join(capturedLogs, "\n"))
	}

	// --- Assert 2: Verify file is marked as missing ---
	p.ConfigMutex.RLock()
	reloadedFileDep, ok := p.Config.FileDependencies[testFilePath]
	p.ConfigMutex.RUnlock()

	if !ok {
		t.Fatalf("File dependency '%s' not found in reloaded config", testFilePath)
	}
	if reloadedFileDep.Status != FileStatusMissing {
		t.Errorf("FileDependency Status mismatch after disappearance. Got '%s', want '%s'", reloadedFileDep.Status, FileStatusMissing)
	}
	if reloadedFileDep.Error == nil {
		t.Error("FileDependency Error is nil, expected an error for missing file")
	}
	if len(reloadedFileDep.Content) != 0 {
		t.Errorf("FileDependency Content mismatch after disappearance. Got %v, want empty", reloadedFileDep.Content)
	}

	// Verify log message for file disappearance
	logMutex.Lock()
	foundLog := false
	for _, log := range capturedLogs {
		if strings.Contains(log, "Modified file dependencies: '"+testFilePath+"'") {
			foundLog = true
			break
		}
	}
	logMutex.Unlock()
	if !foundLog {
		t.Errorf("Expected log message for file disappearance not found.")
		fmt.Println("Captured Logs (TestFileDependency_FileDisappears):")
		for _, log := range capturedLogs {
			fmt.Println(log)
		}
	}
}

// TestFileDependency_CyclicDependency verifies that LoadConfigFromYAML correctly
// detects and reports a cyclic dependency between files.
func TestFileDependency_CyclicDependency(t *testing.T) {
	resetGlobalState()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	fileAPath := filepath.Join(tempDir, "a.txt")
	fileBPath := filepath.Join(tempDir, "b.txt")

	// Create files with a cyclic dependency: a.txt -> b.txt -> a.txt
	// Note: The paths in the files are relative. The loader resolves them.
	if err := os.WriteFile(fileAPath, []byte("file:b.txt"), 0644); err != nil {
		t.Fatalf("Failed to write file a.txt: %v", err)
	}
	if err := os.WriteFile(fileBPath, []byte("file:a.txt"), 0644); err != nil {
		t.Fatalf("Failed to write file b.txt: %v", err)
	}

	// Create a config.yaml that references one of the files in the cycle.
	configContent := `
version: "1.0"
chains:
  - name: "cycle_test_chain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          Path: "file:a.txt"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config.yaml: %v", err)
	}

	// --- Act ---
	_, err := LoadConfigFromYAML(configPath)

	// --- Assert ---
	if err == nil {
		t.Fatal("Expected an error for cyclic dependency, but got nil")
	}

	expectedError := fmt.Sprintf("cyclic dependency detected: %s -> %s -> %s", configPath, fileAPath, fileBPath)
	if !strings.Contains(err.Error(), expectedError) {
		t.Errorf("Error message mismatch.\nGot:  %s\nWant to contain: %s", err.Error(), expectedError)
	}
}
