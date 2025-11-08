package main

import (
	"flag"
	"io"
	"log"
	"sync"
	"testing"
	"time"
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

// MockBlocker implements the Blocker interface for testing, allowing Block() calls to be intercepted.
type MockBlocker struct {
	BlockFunc   func(ipInfo IPInfo, duration time.Duration) error
	UnblockFunc func(ipInfo IPInfo) error
}

// Block calls the stored mock function to simulate the blocking action.
func (m *MockBlocker) Block(ipInfo IPInfo, duration time.Duration) error {
	if m.BlockFunc != nil {
		return m.BlockFunc(ipInfo, duration)
	}
	return nil
}

// Unblock calls the stored mock function to simulate the unblocking action.
func (m *MockBlocker) Unblock(ipInfo IPInfo) error {
	if m.UnblockFunc != nil {
		return m.UnblockFunc(ipInfo)
	}
	return nil
}

// newTestProcessor creates a new Processor instance with sensible defaults for testing.
func newTestProcessor(config *AppConfig, chains []BehavioralChain) *Processor {
	if config == nil {
		config = &AppConfig{}
	}
	p := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[TrackingKey]*BotActivity),
		// Blocker will be set below
		ChainMutex:        &sync.RWMutex{},
		Chains:            chains,
		Config:            config,
		LogFunc:           func(level LogLevel, tag string, format string, args ...interface{}) {},
		IsWhitelistedFunc: func(ipInfo IPInfo) bool { return false },
	}
	// Create a real HAProxyBlocker and link it to the processor.
	blocker := &HAProxyBlocker{P: p}
	p.Blocker = blocker
	p.CheckChainsFunc = p.CheckChains
	return p
}
