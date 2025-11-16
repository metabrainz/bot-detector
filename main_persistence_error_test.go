package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCorruptedSnapshot(t *testing.T) {
	// Create a temporary directory for the test
	tmpDir, err := os.MkdirTemp("", "bot-detector-test-persistence")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			t.Errorf("Failed to remove temporary directory: %v", err)
		}
	}()

	// Create a dummy config file
	configFile, err := os.CreateTemp(tmpDir, "config.yaml")
	if err != nil {
		t.Fatalf("Failed to create temporary config file: %v", err)
	}

	configContent := `
version: "1.0"
persistence:
  enabled: true
chains:
  - name: "test_chain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          uri: "/test"
`
	if _, err := fmt.Fprint(configFile, configContent); err != nil {
		t.Fatalf("Failed to write to config file: %v", err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatalf("Failed to close config file: %v", err)
	}

	// Create a corrupted snapshot file
	snapshotPath := filepath.Join(tmpDir, "state.snapshot")
	if err := os.WriteFile(snapshotPath, []byte("this is not valid json"), 0600); err != nil {
		t.Fatalf("Failed to write corrupted snapshot file: %v", err)
	}

	// Attempt to run the application
	args := []string{"bot-detector", "-config", configFile.Name(), "-log-path", "/dev/null", "-state-dir", tmpDir}
	err = run(args)

	// Verify the outcome
	if err == nil {
		t.Fatal("Expected an error when running with a corrupted snapshot, but got nil")
	}

	expectedError := "failed to unmarshal snapshot"
	if !strings.Contains(err.Error(), expectedError) {
		t.Errorf("Expected error message to contain '%s', but got: %v", expectedError, err)
	}
}

func TestCorruptedJournal(t *testing.T) {
	// Create a temporary directory for the test
	tmpDir, err := os.MkdirTemp("", "bot-detector-test-persistence")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			t.Errorf("Failed to remove temporary directory: %v", err)
		}
	}()

	// Create a dummy config file
	configFile, err := os.CreateTemp(tmpDir, "config.yaml")
	if err != nil {
		t.Fatalf("Failed to create temporary config file: %v", err)
	}

	configContent := `
version: "1.0"
persistence:
  enabled: true
chains:
  - name: "test_chain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          uri: "/test"
`
	if _, err := fmt.Fprint(configFile, configContent); err != nil {
		t.Fatalf("Failed to write to config file: %v", err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatalf("Failed to close config file: %v", err)
	}

	// Create a valid, empty snapshot file
	snapshotPath := filepath.Join(tmpDir, "state.snapshot")
	if err := os.WriteFile(snapshotPath, []byte(`{"timestamp":"0001-01-01T00:00:00Z","active_blocks":{}}`), 0600); err != nil {
		t.Fatalf("Failed to write empty snapshot file: %v", err)
	}

	// Create a corrupted journal file
	journalPath := filepath.Join(tmpDir, "events.log")
	if err := os.WriteFile(journalPath, []byte("this is not valid json\n"), 0600); err != nil {
		t.Fatalf("Failed to write corrupted journal file: %v", err)
	}

	// Attempt to run the application
	args := []string{"bot-detector", "-config", configFile.Name(), "-log-path", "/dev/null", "-state-dir", tmpDir, "-exit-on-eof"}
	err = run(args)

	// Verify the outcome
	if err != nil {
		t.Fatalf("Expected no error when running with a corrupted journal, but got: %v", err)
	}
}
