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
	RegisterCLIFlags(testFlagSet)

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
