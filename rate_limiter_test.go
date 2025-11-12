package main

import (
	"bot-detector/internal/logging"
	metrics "bot-detector/internal/metrics"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// rateLimiterTestHarness encapsulates the setup for rate limiter tests.
type rateLimiterTestHarness struct {
	t                *testing.T
	processor        *Processor
	mockBlocker      *MockBlocker
	rlb              *RateLimitedBlocker
	blockCount       atomic.Int32
	unblockCount     atomic.Int32
	commandProcessed chan struct{}
}

// newRateLimiterTestHarness creates a new test harness.
func newRateLimiterTestHarness(t *testing.T, queueSize, commandsPerSecond int) *rateLimiterTestHarness {
	t.Helper()
	h := &rateLimiterTestHarness{
		t:                t,
		commandProcessed: make(chan struct{}, queueSize+5), // Buffer to prevent blocking
	}
	h.mockBlocker = &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			h.blockCount.Add(1)
			h.commandProcessed <- struct{}{}
			return nil
		},
		UnblockFunc: func(ipInfo IPInfo) error {
			h.unblockCount.Add(1)
			h.commandProcessed <- struct{}{}
			return nil
		},
	}
	h.processor = &Processor{
		LogFunc: logging.LogOutput,
		Metrics: metrics.NewMetrics(), // Initialize the Metrics struct
	}
	h.rlb = NewRateLimitedBlocker(h.processor, h.mockBlocker, queueSize, commandsPerSecond)
	t.Cleanup(h.rlb.Stop)
	return h
}

// waitForCommands waits for a specific number of commands to be processed.
func (h *rateLimiterTestHarness) waitForCommands(count int) {
	for i := 0; i < count; i++ {
		select {
		case <-h.commandProcessed:
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
		ip := IPInfo{Address: fmt.Sprintf("192.168.1.%d", i)}
		h.rlb.Block(ip, 5*time.Minute)
	}

	h.waitForCommands(numBlockCommands)

	if h.blockCount.Load() != int32(numBlockCommands) {
		t.Errorf("Expected %d blocks, got %d", numBlockCommands, h.blockCount.Load())
	}

	// --- Test Unblocking ---
	numUnblockCommands := 3
	for i := 0; i < numUnblockCommands; i++ {
		ip := IPInfo{Address: fmt.Sprintf("192.168.2.%d", i)}
		h.rlb.Unblock(ip)
	}

	h.waitForCommands(numUnblockCommands)

	if h.unblockCount.Load() != int32(numUnblockCommands) {
		t.Errorf("Expected %d unblocks, got %d", numUnblockCommands, h.unblockCount.Load())
	}
}

func TestRateLimitedBlocker_QueueFull(t *testing.T) {
	// Use a slow rate to ensure the queue fills up.
	h := newRateLimiterTestHarness(t, 2, 1)

	// Fill the queue (size 2) and send one more to be dropped.
	numCommands := 3
	for i := 0; i < numCommands; i++ {
		ip := IPInfo{Address: fmt.Sprintf("192.168.1.%d", i)}
		h.rlb.Block(ip, 5*time.Minute)
	}

	// Give a moment for the non-blocking sends to complete.
	time.Sleep(50 * time.Millisecond)

	// The queue should be full, and one command should have been dropped.
	// We expect only `queueSize` commands to be processed.
	h.waitForCommands(2)

	// We expect only `queueSize` commands to be processed, as one was dropped.
	if h.blockCount.Load() != int32(2) {
		t.Errorf("Expected %d blocks (queue size), got %d", 2, h.blockCount.Load())
	}

	// Verify no more commands are processed.
	select {
	case <-h.commandProcessed:
		t.Error("an extra command was processed, but it should have been dropped")
	case <-time.After(100 * time.Millisecond):
		// Correct, no more commands processed.
	}
}

func TestRateLimitedBlocker_ZeroRate(t *testing.T) {
	var blockCount atomic.Int32
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCount.Add(1)
			return nil
		},
	}
	processor := &Processor{
		LogFunc: logging.LogOutput,
		Metrics: metrics.NewMetrics(),
	}

	queueSize := 10
	commandsPerSecond := 0 // Zero rate should disable the worker

	rlb := NewRateLimitedBlocker(processor, mockBlocker, queueSize, commandsPerSecond)
	defer rlb.Stop()

	ip := IPInfo{Address: "192.168.1.1"}
	rlb.Block(ip, 5*time.Minute)

	time.Sleep(50 * time.Millisecond) // Give some time for the command to be queued.

	if blockCount.Load() != 0 {
		t.Errorf("Expected 0 blocks when rate is zero, got %d", blockCount.Load())
	}
	if len(rlb.CommandQueue) != 1 {
		t.Errorf("Expected 1 command in queue when rate is zero, got %d", len(rlb.CommandQueue))
	}
}

func TestRateLimitedBlocker_Stop(t *testing.T) {
	h := newRateLimiterTestHarness(t, 10, 100) // High rate

	ip := IPInfo{Address: "192.168.1.1"}
	h.rlb.Block(ip, 5*time.Minute)

	// Wait for the first command to ensure the worker is running.
	h.waitForCommands(1)

	// Stop the worker immediately. This is safe to call multiple times.
	h.rlb.Stop()

	// Now that Stop() waits, we can be sure no more commands will be processed.
	// Try to queue another command. It should be ignored because the queue channel
	// is not being read from anymore.
	h.rlb.Block(ip, 5*time.Minute)

	// Only the first command should have been processed.
	if h.blockCount.Load() != 1 {
		t.Errorf("Expected exactly 1 block to be processed after stopping, got %d", h.blockCount.Load())
	}
}
