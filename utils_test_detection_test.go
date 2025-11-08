package main

import (
	"os"
	"testing"
)

func TestIsTesting(t *testing.T) {
	// The default case when running `go test`
	t.Run("when running under go test", func(t *testing.T) {
		if !isTesting() {
			t.Error("isTesting() returned false, but it should be true when running tests.")
		}
	})

	// The case where we simulate a production run
	t.Run("when not running under go test", func(t *testing.T) {
		// --- Setup: Manipulate os.Args ---
		originalArgs := os.Args
		// Set os.Args to a slice that does not contain test flags.
		os.Args = []string{"/path/to/my/program", "-some-flag"}
		// Ensure os.Args is restored after the test.
		t.Cleanup(func() { os.Args = originalArgs })

		// --- Act & Assert ---
		if isTesting() {
			t.Error("isTesting() returned true, but it should be false for a simulated production run.")
		}
	})
}
