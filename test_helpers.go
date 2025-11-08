package main

import (
	"flag"
	"io"
	"log"
	"testing"
)

// muteGlobalLogger redirects the output of the standard logger to discard,
// effectively silencing any direct calls to log.Printf during tests.
func muteGlobalLogger() {
	log.SetOutput(io.Discard)
}

// resetGlobalState resets global variables to their default state for test isolation.
// This is critical for tests that modify global state, such as command-line flags.
func resetGlobalState() {
	muteGlobalLogger()

	// Reset the global flag set to clear any flags parsed in other tests.
	flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
	// Re-register application-specific flags.
	RegisterCLIFlags(flag.CommandLine)
	// Re-register the standard testing flags. This is crucial for `isTesting()` to work.
	testing.Init()
}
