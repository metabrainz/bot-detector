package main

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/types"
	"crypto/sha256"
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
		config.FileDependencies = make(map[string]*types.FileDependency)
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
	loadedCfg, err := LoadConfigFromYAML(LoadConfigOptions{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("Initial LoadConfigFromYAML failed: %v", err)
	}
	var capturedLogs []string
	var logMutex sync.Mutex
	logCaptureFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		logMutex.Lock()
		defer logMutex.Unlock()
		capturedLogs = append(capturedLogs, fmt.Sprintf("[%s] %s: %s", level, tag, fmt.Sprintf(format, args...)))
	}

	// Redirect global logging.LogOutput to our capture function
	originalLogOutput := logging.LogOutput
	logging.LogOutput = logCaptureFunc
	t.Cleanup(func() { logging.LogOutput = originalLogOutput })

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
	if reloadedFileDep.CurrentStatus.Status != types.FileStatusLoaded {
		t.Errorf("FileDependency Status mismatch after reload. Got '%s', want '%s'", reloadedFileDep.CurrentStatus.Status, types.FileStatusLoaded)
	}
	if reloadedFileDep.CurrentStatus.Error != nil {
		t.Errorf("FileDependency Error mismatch after reload. Got '%v', want nil", reloadedFileDep.CurrentStatus.Error)
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
	loadedCfg, err := LoadConfigFromYAML(LoadConfigOptions{ConfigPath: configPath})
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

	if fileDep.CurrentStatus.Status != types.FileStatusMissing {
		t.Errorf("FileDependency Status mismatch. Got '%s', want '%s'", fileDep.CurrentStatus.Status, types.FileStatusMissing)
	}
	if fileDep.CurrentStatus.Error == nil {
		t.Error("Expected an error for missing file")
	}
	if !strings.Contains(fileDep.CurrentStatus.Error.Error(), "no such file or directory") {
		t.Errorf("Expected error to contain 'no such file or directory', got '%v'", fileDep.CurrentStatus.Error)
	}
	if len(fileDep.Content) != 0 {
		t.Errorf("FileDependency Content mismatch. Got %v, want empty", fileDep.Content)
	}

	// Verify warning was logged by compileStringMatcher
	logMutex.Lock()
	foundWarning := false
	for _, log := range capturedLogs {
		if strings.Contains(log, "file matcher '"+testFilePath+"' is Missing, treating as empty") {
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
	loadedCfg, err := LoadConfigFromYAML(LoadConfigOptions{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("Initial LoadConfigFromYAML failed: %v", err)
	}
	if loadedCfg == nil {
		t.Fatal("Loaded config is nil")
	}
	initialFileDep, ok := loadedCfg.FileDependencies[testFilePath]
	if !ok || initialFileDep.CurrentStatus.Status != types.FileStatusMissing {
		t.Fatalf("Expected file dependency to be initially missing, got status '%s'", initialFileDep.CurrentStatus.Status)
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
	if reloadedFileDep.CurrentStatus.Status != types.FileStatusLoaded {
		t.Errorf("FileDependency Status mismatch after reappearance. Got '%s', want '%s'", reloadedFileDep.CurrentStatus.Status, types.FileStatusLoaded)
	}
	if reloadedFileDep.CurrentStatus.Error != nil {
		t.Errorf("FileDependency Error mismatch after reappearance. Got '%v', want nil", reloadedFileDep.CurrentStatus.Error)
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
	loadedCfg, err := LoadConfigFromYAML(LoadConfigOptions{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("Initial LoadConfigFromYAML failed: %v", err)
	}
	if loadedCfg == nil {
		t.Fatal("Loaded config is nil")
	}
	initialFileDep, ok := loadedCfg.FileDependencies[testFilePath]
	if !ok || initialFileDep.CurrentStatus.Status != types.FileStatusLoaded {
		t.Fatalf("Expected file dependency to be initially loaded, got status '%s'", initialFileDep.CurrentStatus.Status)
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
	if reloadedFileDep.CurrentStatus.Status != types.FileStatusMissing {
		t.Errorf("FileDependency Status mismatch after disappearance. Got '%s', want '%s'", reloadedFileDep.CurrentStatus.Status, types.FileStatusMissing)
	}
	if reloadedFileDep.CurrentStatus.Error == nil {
		t.Error("Expected an error for missing file")
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
	_, err := LoadConfigFromYAML(LoadConfigOptions{ConfigPath: configPath})

	// --- Assert ---
	if err == nil {
		t.Fatal("Expected an error for cyclic dependency, but got nil")
	}

	expectedError := fmt.Sprintf("cyclic dependency detected: %s -> %s -> %s", configPath, fileAPath, fileBPath)
	if !strings.Contains(err.Error(), expectedError) {
		t.Errorf("Error message mismatch.\nGot:  %s\nWant to contain: %s", err.Error(), expectedError)
	}
}

// TestFileDependency_UpdateStatusScenarios verifies the correct behavior of FileDependency.updateStatus()
// under various file state changes (initial load, no change, content change, disappearance, reappearance).
func TestFileDependency_UpdateStatusScenarios(t *testing.T) {
	resetGlobalState()

	tempDir := t.TempDir()
	testFilePath := filepath.Join(tempDir, "test_file.txt")

	// Helper function to create a FileDependency and perform initial updateStatus
	createAndInitFileDep := func(path, content string) *types.FileDependency {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test file %s: %v", path, err)
		}
		fd := &types.FileDependency{Path: path}
		fd.UpdateStatus()
		return fd
	}

	// Helper function to get checksum of content
	getChecksum := func(content string) string {
		return calculateChecksum([]string{content})
	}

	// Scenario 1: Initial Load
	t.Run("Initial Load", func(t *testing.T) {
		fd := createAndInitFileDep(testFilePath, "initial content")
		if fd.CurrentStatus.Status != types.FileStatusLoaded {
			t.Errorf("Expected status Loaded, got %s", fd.CurrentStatus.Status)
		}
		if fd.PreviousStatus != nil {
			t.Error("Expected PreviousStatus to be nil on initial load")
		}
		expectedChecksum := getChecksum("initial content")
		if fd.CurrentStatus.Checksum != expectedChecksum {
			t.Errorf("Expected checksum %s, got %s", expectedChecksum, fd.CurrentStatus.Checksum)
		}
		if fd.CurrentStatus.Error != nil {
			t.Errorf("Expected no error, got %v", fd.CurrentStatus.Error)
		}
	})

	// Scenario 2: No Change (ModTime same, Checksum same)
	t.Run("No Change", func(t *testing.T) {
		fd := createAndInitFileDep(testFilePath, "stable content")
		initialCurrentStatus := fd.CurrentStatus.Clone() // Clone to compare later

		// Wait a bit, but don't modify the file, so ModTime should be the same
		time.Sleep(10 * time.Millisecond)
		fd.UpdateStatus()

		if fd.CurrentStatus.Status != types.FileStatusLoaded {
			t.Errorf("Expected status Loaded, got %s", fd.CurrentStatus.Status)
		}
		if fd.PreviousStatus == nil {
			t.Error("Expected PreviousStatus to be set after update")
		}
		if fd.CurrentStatus.Checksum != initialCurrentStatus.Checksum {
			t.Errorf("Expected checksum to be same, got %s, want %s", fd.CurrentStatus.Checksum, initialCurrentStatus.Checksum)
		}
		if !fd.CurrentStatus.ModTime.Equal(initialCurrentStatus.ModTime) {
			t.Errorf("Expected ModTime to be same, got %v, want %v", fd.CurrentStatus.ModTime, initialCurrentStatus.ModTime)
		}
		if fd.CurrentStatus.Error != nil {
			t.Errorf("Expected no error, got %v", fd.CurrentStatus.Error)
		}
	})

	// Scenario 3: Content Change
	t.Run("Content Change", func(t *testing.T) {
		fd := createAndInitFileDep(testFilePath, "original content")
		originalChecksum := fd.CurrentStatus.Checksum
		originalModTime := fd.CurrentStatus.ModTime

		// Modify file content
		time.Sleep(20 * time.Millisecond) // Ensure ModTime changes
		newContent := "updated content"
		if err := os.WriteFile(testFilePath, []byte(newContent), 0644); err != nil {
			t.Fatalf("Failed to write updated test file: %v", err)
		}
		fd.UpdateStatus()

		if fd.CurrentStatus.Status != types.FileStatusLoaded {
			t.Errorf("Expected status Loaded, got %s", fd.CurrentStatus.Status)
		}
		if fd.PreviousStatus == nil {
			t.Error("Expected PreviousStatus to be set")
		}
		if fd.PreviousStatus.Checksum != originalChecksum {
			t.Errorf("Expected PreviousStatus checksum %s, got %s", originalChecksum, fd.PreviousStatus.Checksum)
		}
		if fd.PreviousStatus.ModTime != originalModTime {
			t.Errorf("Expected PreviousStatus ModTime %v, got %v", originalModTime, fd.PreviousStatus.ModTime)
		}
		expectedNewChecksum := getChecksum(newContent)
		if fd.CurrentStatus.Checksum != expectedNewChecksum {
			t.Errorf("Expected CurrentStatus checksum %s, got %s", expectedNewChecksum, fd.CurrentStatus.Checksum)
		}
		if fd.CurrentStatus.ModTime.Equal(originalModTime) {
			t.Error("Expected CurrentStatus ModTime to be updated")
		}
		if fd.CurrentStatus.Error != nil {
			t.Errorf("Expected no error, got %v", fd.CurrentStatus.Error)
		}
	})

	// Scenario 4: File Disappears
	t.Run("File Disappears", func(t *testing.T) {
		fd := createAndInitFileDep(testFilePath, "content to disappear")
		originalCurrentStatus := fd.CurrentStatus.Clone()

		// Delete the file
		if err := os.Remove(testFilePath); err != nil {
			t.Fatalf("Failed to remove test file: %v", err)
		}
		fd.UpdateStatus()

		if fd.CurrentStatus.Status != types.FileStatusMissing {
			t.Errorf("Expected status Missing, got %s", fd.CurrentStatus.Status)
		}
		if fd.CurrentStatus.Error == nil {
			t.Error("Expected an error for missing file")
		}
		if fd.PreviousStatus == nil {
			t.Error("Expected PreviousStatus to be set")
		}
		if fd.PreviousStatus.Checksum != originalCurrentStatus.Checksum {
			t.Errorf("Expected PreviousStatus checksum %s, got %s", originalCurrentStatus.Checksum, fd.PreviousStatus.Checksum)
		}
	})

	// Scenario 5: File Reappears
	t.Run("File Reappears", func(t *testing.T) {
		fd := createAndInitFileDep(testFilePath, "initial content")
		// Delete the file
		if err := os.Remove(testFilePath); err != nil {
			t.Fatalf("Failed to remove test file: %v", err)
		}
		fd.UpdateStatus() // Status should now be Missing

		if fd.CurrentStatus.Status != types.FileStatusMissing {
			t.Fatalf("Pre-condition failed: Expected status Missing, got %s", fd.CurrentStatus.Status)
		}
		missingStatus := fd.CurrentStatus.Clone()

		// Recreate the file
		time.Sleep(20 * time.Millisecond) // Ensure ModTime changes
		reappearedContent := "reappeared content"
		if err := os.WriteFile(testFilePath, []byte(reappearedContent), 0644); err != nil {
			t.Fatalf("Failed to recreate test file: %v", err)
		}
		fd.UpdateStatus()

		if fd.CurrentStatus.Status != types.FileStatusLoaded {
			t.Errorf("Expected status Loaded, got %s", fd.CurrentStatus.Status)
		}
		if fd.CurrentStatus.Error != nil {
			t.Errorf("Expected no error, got %v", fd.CurrentStatus.Error)
		}
		if fd.PreviousStatus == nil {
			t.Error("Expected PreviousStatus to be set")
		}
		if fd.PreviousStatus.Status != missingStatus.Status {
			t.Errorf("Expected PreviousStatus status %s, got %s", missingStatus.Status, fd.PreviousStatus.Status)
		}
		expectedChecksum := getChecksum(reappearedContent)
		if fd.CurrentStatus.Checksum != expectedChecksum {
			t.Errorf("Expected CurrentStatus checksum %s, got %s", expectedChecksum, fd.CurrentStatus.Checksum)
		}
	})

	// Scenario 6: File Error (e.g., permissions - simulate by making unreadable)
	t.Run("File Error", func(t *testing.T) {
		// Create a file that we will make unreadable
		unreadableFilePath := filepath.Join(tempDir, "unreadable_file.txt")
		if err := os.WriteFile(unreadableFilePath, []byte("some content"), 0644); err != nil {
			t.Fatalf("Failed to write unreadable test file: %v", err)
		}

		fd := &types.FileDependency{Path: unreadableFilePath}
		fd.UpdateStatus() // Initial load should be fine

		if fd.CurrentStatus.Status != types.FileStatusLoaded {
			t.Fatalf("Pre-condition failed: Expected status Loaded, got %s", fd.CurrentStatus.Status)
		}
		loadedStatus := fd.CurrentStatus.Clone()

		// Make the file unreadable (e.g., chmod 000)
		if err := os.Chmod(unreadableFilePath, 0000); err != nil {
			t.Fatalf("Failed to chmod file to unreadable: %v", err)
		}
		defer func() {
			// Restore permissions for cleanup
			_ = os.Chmod(unreadableFilePath, 0644)
		}()

		fd.UpdateStatus()

		if fd.CurrentStatus.Status != types.FileStatusError {
			t.Errorf("Expected status Error, got %s", fd.CurrentStatus.Status)
		}
		if fd.CurrentStatus.Error == nil {
			t.Error("Expected an error for unreadable file")
		}
		if fd.PreviousStatus == nil {
			t.Error("Expected PreviousStatus to be set")
		}
		if fd.PreviousStatus.Checksum != loadedStatus.Checksum {
			t.Errorf("Expected PreviousStatus checksum %s, got %s", loadedStatus.Checksum, fd.PreviousStatus.Checksum)
		}
	})
}

func calculateChecksum(lines []string) string {
	h := sha256.New()
	h.Write([]byte(strings.Join(lines, "\n")))
	return fmt.Sprintf("%x", h.Sum(nil))
}
