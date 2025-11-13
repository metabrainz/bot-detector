package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestFlagUsage(t *testing.T) {
	// --- Setup ---
	// The flag package uses a global flag set. To test it in isolation,
	// we need to create a new, empty flag set.
	var actualOutput bytes.Buffer
	testFlagSet := flag.NewFlagSet("test", flag.ContinueOnError)
	testFlagSet.SetOutput(&actualOutput) // Redirect output to our buffer

	// We need to re-register our flags and usage function on this new flag set.
	_ = RegisterCLIFlags(testFlagSet) // Ignore return value for this test

	// Define the usage function directly for the test flag set.
	testFlagSet.Usage = func() {
		fmt.Fprintf(&actualOutput, "Usage of %s:\n", os.Args[0])
		testFlagSet.PrintDefaults()
	}

	// --- Act ---
	// Calling Usage on the test flag set will now execute our custom usage function.
	testFlagSet.Usage()

	// --- Assert ---
	// Check if the output contains some key phrases from our custom usage message.
	// We can't easily test the custom part of the global flag.Usage,
	// but we can test that our flags are correctly registered on the test set.

	expectedFlag := "-log-path"
	if !strings.Contains(actualOutput.String(), expectedFlag) {
		t.Errorf("Expected usage output to contain the flag '%s', but it did not.\nGot:\n%s", expectedFlag, actualOutput.String())
	}
}

func TestFlagUsageOutput(t *testing.T) {
	// --- Setup ---
	// The init() function in flags_and_cli.go sets the global flag.Usage.
	// To test it, we need to capture the output that it writes to os.Stderr.

	// 1. Keep a copy of the original os.Stderr.
	originalStderr := os.Stderr
	// 2. Create a pipe. The writer will replace os.Stderr, and we'll read from the reader.
	r, w, _ := os.Pipe()
	os.Stderr = w

	// 3. Ensure we restore os.Stderr and close the pipe when the test is done.
	t.Cleanup(func() {
		os.Stderr = originalStderr
		_ = w.Close()
		_ = r.Close()
	})

	// --- Act ---
	// Call the global Usage function, which was set by init().
	flag.Usage()
	_ = w.Close() // Close the writer to signal EOF to the reader.

	// Read the output from the pipe.
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	// --- Assert ---
	// Check that our custom help text is present.
	expectedSubstring := "A behavioral bot detection tool"
	if !strings.Contains(output, expectedSubstring) {
		t.Errorf("Expected usage output to contain '%s', but it did not.\nGot:\n%s", expectedSubstring, output)
	}
}
