package blocker_test

import (
	"bot-detector/internal/blocker"
	"bot-detector/internal/logging"
	"bot-detector/internal/utils" // Added for IPInfo
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// mockWrappedBlocker is a mock for the underlying Blocker interface.
type mockWrappedBlocker struct {
	blockCount        atomic.Int32
	unblockCount      atomic.Int32
	dumpBackendsCount atomic.Int32
	processCh         chan struct{}
	blockedIPs        []string // For ListBlocked
}

func (m *mockWrappedBlocker) Block(ipInfo utils.IPInfo, duration time.Duration, reason string) error {
	m.blockCount.Add(1)
	m.processCh <- struct{}{}
	return nil
}

func (m *mockWrappedBlocker) Unblock(ipInfo utils.IPInfo, reason string) error {
	m.unblockCount.Add(1)
	m.processCh <- struct{}{}
	return nil
}

func (m *mockWrappedBlocker) IsIPBlocked(ipInfo utils.IPInfo) (bool, error) {
	for _, ip := range m.blockedIPs {
		if ip == ipInfo.Address {
			return true, nil
		}
	}
	return false, nil
}

func (m *mockWrappedBlocker) DumpBackends() ([]string, error) {
	m.dumpBackendsCount.Add(1)
	return m.blockedIPs, nil
}

func (m *mockWrappedBlocker) CompareHAProxyBackends(expTolerance time.Duration) ([]blocker.SyncDiscrepancy, error) {
	return []blocker.SyncDiscrepancy{}, nil
}

func (m *mockWrappedBlocker) GetCurrentState() (map[string]int, error) {
	return make(map[string]int), nil
}

func (m *mockWrappedBlocker) Shutdown() {
	// No-op for mock
}

// mockProvider implements both LogProvider and MetricsProvider for testing.
type mockProvider struct {
	cmdsDropped atomic.Int32
}

func (m *mockProvider) Log(level logging.LogLevel, tag string, format string, v ...interface{}) {}
func (m *mockProvider) IncrementBlockerCmdsQueued()                                             {}
func (m *mockProvider) IncrementBlockerCmdsDropped()                                            { m.cmdsDropped.Add(1) }
func (m *mockProvider) IncrementBlockerCmdsExecuted()                                           {}

// rateLimiterTestHarness encapsulates the setup for rate limiter tests.
type rateLimiterTestHarness struct {
	t            *testing.T
	mockBlocker  *mockWrappedBlocker
	mockProvider *mockProvider
	rlb          *blocker.RateLimitedBlocker
}

// newRateLimiterTestHarness creates a new test harness.
func newRateLimiterTestHarness(t *testing.T, queueSize, commandsPerSecond int) *rateLimiterTestHarness {
	t.Helper()
	h := &rateLimiterTestHarness{t: t}
	h.mockBlocker = &mockWrappedBlocker{
		processCh: make(chan struct{}, queueSize+5),
	}
	h.mockProvider = &mockProvider{}
	h.rlb = blocker.NewRateLimitedBlocker(h.mockProvider, h.mockProvider, h.mockBlocker, queueSize, commandsPerSecond)
	t.Cleanup(h.rlb.Stop)
	return h
}

// waitForCommands waits for a specific number of commands to be processed.
func (h *rateLimiterTestHarness) waitForCommands(count int) {
	for i := 0; i < count; i++ {
		select {
		case <-h.mockBlocker.processCh:
			// Command processed
		case <-time.After(2 * time.Second): // Generous timeout
			h.t.Fatalf("timed out waiting for command %d of %d to be processed", i+1, count)
		}
	}
}

func TestRateLimitedBlocker_BlockAndUnblock(t *testing.T) {
	// Use a high rate to make the test fast.
	h := newRateLimiterTestHarness(t, 10, 1000)

	// --- Test Blocking ---
	numBlockCommands := 5
	for i := 0; i < numBlockCommands; i++ {
		ip := utils.NewIPInfo(fmt.Sprintf("192.168.1.%d", i))
		_ = h.rlb.Block(ip, 5*time.Minute, "test-reason")
	}

	h.waitForCommands(numBlockCommands)

	if h.mockBlocker.blockCount.Load() != int32(numBlockCommands) {
		t.Errorf("Expected %d blocks, got %d", numBlockCommands, h.mockBlocker.blockCount.Load())
	}

	// --- Test Unblocking ---
	numUnblockCommands := 3
	for i := 0; i < numUnblockCommands; i++ {
		ip := utils.NewIPInfo(fmt.Sprintf("192.168.2.%d", i))
		_ = h.rlb.Unblock(ip, "test-unblock")
	}

	h.waitForCommands(numUnblockCommands)

	if h.mockBlocker.unblockCount.Load() != int32(numUnblockCommands) {
		t.Errorf("Expected %d unblocks, got %d", numUnblockCommands, h.mockBlocker.unblockCount.Load())
	}
}

func TestRateLimitedBlocker_QueueFull(t *testing.T) {
	// Use a slow rate to ensure the queue fills up.
	h := newRateLimiterTestHarness(t, 2, 1)

	// Fill the queue (size 2) and send one more to be dropped.
	numCommands := 3
	for i := 0; i < numCommands; i++ {
		ip := utils.NewIPInfo(fmt.Sprintf("192.168.1.%d", i))
		_ = h.rlb.Block(ip, 5*time.Minute, "test-reason")
	}

	// Give a moment for the non-blocking sends to complete.
	time.Sleep(50 * time.Millisecond)

	// We expect only `queueSize` commands to be processed.
	h.waitForCommands(2)

	if h.mockBlocker.blockCount.Load() != int32(2) {
		t.Errorf("Expected %d blocks (queue size), got %d", 2, h.mockBlocker.blockCount.Load())
	}
	if h.mockProvider.cmdsDropped.Load() != 1 {
		t.Errorf("Expected 1 command to be dropped, but got %d", h.mockProvider.cmdsDropped.Load())
	}
}

func TestRateLimitedBlocker_Stop(t *testing.T) {
	h := newRateLimiterTestHarness(t, 10, 100) // High rate

	ip := utils.NewIPInfo("192.168.1.1")
	_ = h.rlb.Block(ip, 5*time.Minute, "test-reason")

	h.waitForCommands(1)

	h.rlb.Stop()

	_ = h.rlb.Block(ip, 5*time.Minute, "test-reason")

	if h.mockBlocker.blockCount.Load() != 1 {
		t.Errorf("Expected exactly 1 block to be processed after stopping, got %d", h.mockBlocker.blockCount.Load())
	}
}
