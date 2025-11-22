package cluster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"bot-detector/internal/logging"
)

func TestEnsureURIScheme(t *testing.T) {
	testCases := []struct {
		name     string
		address  string
		protocol string
		expected string
	}{
		{
			name:     "No scheme, http protocol",
			address:  "localhost:8080",
			protocol: "http",
			expected: "http://localhost:8080",
		},
		{
			name:     "No scheme, https protocol",
			address:  "localhost:8080",
			protocol: "https",
			expected: "https://localhost:8080",
		},
		{
			name:     "HTTP scheme already present",
			address:  "http://localhost:8080",
			protocol: "https", // Should not matter if scheme is present
			expected: "http://localhost:8080",
		},
		{
			name:     "HTTPS scheme already present",
			address:  "https://localhost:8080",
			protocol: "http", // Should not matter if scheme is present
			expected: "https://localhost:8080",
		},
		{
			name:     "FTP scheme already present",
			address:  "ftp://localhost:8080",
			protocol: "http",
			expected: "ftp://localhost:8080",
		},
		{
			name:     "Empty string, http protocol",
			address:  "",
			protocol: "http",
			expected: "http://",
		},
		{
			name:     "Empty string, https protocol",
			address:  "",
			protocol: "https",
			expected: "https://",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := ensureURIScheme(tc.address, tc.protocol)
			if actual != tc.expected {
				t.Errorf("For address '%s' with protocol '%s', expected '%s' but got '%s'", tc.address, tc.protocol, tc.expected, actual)
			}
		})
	}
}

func TestExtractTarGz_ChecksumVerification(t *testing.T) {
	// Create a simple tar.gz archive
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// Add a test file
	content := []byte("test content")
	hdr := &tar.Header{
		Name: "test.txt",
		Mode: 0644,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("Failed to write tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("Failed to write tar content: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("Failed to close tar writer: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("Failed to close gzip writer: %v", err)
	}

	archiveData := buf.Bytes()
	hash := sha256.Sum256(archiveData)
	correctChecksum := fmt.Sprintf("%x", hash)
	incorrectChecksum := "0000000000000000000000000000000000000000000000000000000000000000"

	logFunc := func(level logging.LogLevel, tag string, format string, v ...interface{}) {
		t.Logf("[%s] %s", tag, fmt.Sprintf(format, v...))
	}

	t.Run("Valid checksum", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := extractTarGz(archiveData, tmpDir, correctChecksum, logFunc)
		if err != nil {
			t.Errorf("Expected no error with valid checksum, got: %v", err)
		}

		// Verify file was extracted
		extractedPath := filepath.Join(tmpDir, "test.txt")
		if _, err := os.Stat(extractedPath); os.IsNotExist(err) {
			t.Error("Expected file to be extracted")
		}
	})

	t.Run("Invalid checksum", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := extractTarGz(archiveData, tmpDir, incorrectChecksum, logFunc)
		if err == nil {
			t.Error("Expected error with invalid checksum, got nil")
		}
		if err != nil && err.Error() != fmt.Sprintf("archive checksum verification failed (expected: %s, got: %s)", incorrectChecksum, correctChecksum) {
			t.Errorf("Expected checksum verification error, got: %v", err)
		}
	})

	t.Run("No checksum provided", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := extractTarGz(archiveData, tmpDir, "", logFunc)
		if err != nil {
			t.Errorf("Expected no error when no checksum provided, got: %v", err)
		}

		// Verify file was extracted
		extractedPath := filepath.Join(tmpDir, "test.txt")
		if _, err := os.Stat(extractedPath); os.IsNotExist(err) {
			t.Error("Expected file to be extracted")
		}
	})

	t.Run("Checksum with quotes", func(t *testing.T) {
		tmpDir := t.TempDir()
		quotedChecksum := fmt.Sprintf("\"%s\"", correctChecksum)
		err := extractTarGz(archiveData, tmpDir, quotedChecksum, logFunc)
		if err != nil {
			t.Errorf("Expected no error with quoted checksum, got: %v", err)
		}
	})
}
