package main

import (
	"bot-detector/internal/commandline"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func createDummyConfig(tmpDir string) error {
	// Create a dummy config file
	configFile, err := os.Create(filepath.Join(tmpDir, "config.yaml"))
	if err != nil {
		return fmt.Errorf("Failed to create temporary config file: %v", err)
	}

	configContent := `
version: "1.0"
application:
  persistence:
    enabled: true
chains:
  - name: "test_chain"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          path: "/test"
`
	if _, err := fmt.Fprint(configFile, configContent); err != nil {
		return fmt.Errorf("Failed to write to config file: %v", err)
	}
	if err := configFile.Close(); err != nil {
		return fmt.Errorf("Failed to close config file: %v", err)
	}
	return nil
}

func TestCorruptedSnapshot(t *testing.T) {
	// Create a temporary directory for the test
	tmpDir, err := os.MkdirTemp("", "bot-detector-test-persistence")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			t.Logf("Warning: failed to clean up temp dir %s: %v", tmpDir, err)
		}
	}()

	// Create a dummy config file
	if err := createDummyConfig(tmpDir); err != nil {
		t.Fatalf("Error creating dummy config file: %v", err)
	}

	// Create a corrupted snapshot file
	snapshotPath := filepath.Join(tmpDir, "state.snapshot")
	if err := os.WriteFile(snapshotPath, []byte("this is not valid json"), 0600); err != nil {
		t.Fatalf("Failed to write corrupted snapshot file: %v", err)
	}

	// Attempt to run the application
	params := &commandline.AppParameters{
		ConfigDir: tmpDir,
		LogPath:   "/dev/null",
		StateDir:  tmpDir,
	}
	err = execute(params)

	// Verify the outcome
	if err == nil {
		t.Fatal("Expected an error when running with a corrupted snapshot, but got nil")
	}

	expectedError := "failed to unmarshal v1 snapshot"
	if !strings.Contains(err.Error(), expectedError) && !strings.Contains(err.Error(), "Failed to migrate legacy persistence") {
		t.Errorf("Expected error message to contain '%s' or migration failure, but got: %v", expectedError, err)
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
			t.Logf("Warning: failed to clean up temp dir %s: %v", tmpDir, err)
		}
	}()

	// Create a dummy config file
	if err := createDummyConfig(tmpDir); err != nil {
		t.Fatalf("Error creating dummy config file: %v", err)
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
	params := &commandline.AppParameters{
		ConfigDir: tmpDir,
		LogPath:   "/dev/null",
		StateDir:  tmpDir,
		ExitOnEOF: true,
	}
	err = execute(params)

	// Verify the outcome
	if err != nil {
		t.Fatalf("Expected no error when running with a corrupted journal, but got: %v", err)
	}
}

func TestUnwritableStateDir(t *testing.T) {
	// Create a temporary directory for the test
	tmpDir, err := os.MkdirTemp("", "bot-detector-test-persistence")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			t.Logf("Warning: failed to clean up temp dir %s: %v", tmpDir, err)
		}
	}()

	// Create a dummy config file
	if err := createDummyConfig(tmpDir); err != nil {
		t.Fatalf("Error creating dummy config file: %v", err)
	}

	// Create an unwritable state directory
	unwritableDir := filepath.Join(tmpDir, "unwritable")
	if err := os.Mkdir(unwritableDir, 0500); err != nil {
		t.Fatalf("Failed to create unwritable directory: %v", err)
	}

	// Attempt to run the application
	params := &commandline.AppParameters{
		ConfigDir: tmpDir,
		LogPath:   "/dev/null",
		StateDir:  unwritableDir,
		ExitOnEOF: true,
	}
	err = execute(params)

	// Verify the outcome
	if err == nil {
		t.Fatal("Expected an error when running with an unwritable state directory, but got nil")
	}

	expectedError := "permission denied"
	if !strings.Contains(err.Error(), expectedError) && !strings.Contains(err.Error(), "unable to open database") && !strings.Contains(err.Error(), "Failed to initialize SQLite") {
		t.Errorf("Expected error message to contain '%s' or SQLite open failure, but got: %v", expectedError, err)
	}
}
