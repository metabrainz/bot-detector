package main

import (
	"flag"
	"io"
	"log"
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
	RegisterCLIFlags(flag.CommandLine) // Re-register flags on the new, clean set.
}
