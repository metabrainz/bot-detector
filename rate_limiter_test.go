package main

import (
	"bot-detector/internal/logging"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimitedBlocker_Block(t *testing.T) {
	var blockCount atomic.Int32
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCount.Add(1)
			return nil
		},
	}
	processor := &Processor{
		LogFunc: logging.LogOutput,
	}

	queueSize := 10
	commandsPerSecond := 2 // 2 commands per second

	rlb := NewRateLimitedBlocker(processor, mockBlocker, queueSize, commandsPerSecond)
	defer rlb.Stop()

	// Send more commands than the rate limit allows in a short burst
	numCommands := 5
	for i := 0; i < numCommands; i++ {
		ip := IPInfo{Address: fmt.Sprintf("192.168.1.%d", i)}
		rlb.Block(ip, 5*time.Minute)
	}

	// Allow some time for the worker to process commands
	time.Sleep(2 * time.Second) // Should process 4 commands (2 per second)

	// Check that not all commands were processed immediately
	if blockCount.Load() > int32(commandsPerSecond*2) {
		t.Errorf("Expected at most %d blocks after 2 seconds, got %d", commandsPerSecond*2, blockCount.Load())
	}

	// Wait for all commands to be processed
	time.Sleep(3 * time.Second) // Total 5 seconds, should process all 5 commands

	if blockCount.Load() != int32(numCommands) {
		t.Errorf("Expected %d blocks after sufficient time, got %d", numCommands, blockCount.Load())
	}
}

func TestRateLimitedBlocker_Unblock(t *testing.T) {
	var unblockCount atomic.Int32
	mockBlocker := &MockBlocker{
		UnblockFunc: func(ipInfo IPInfo) error {
			unblockCount.Add(1)
			return nil
		},
	}
	processor := &Processor{
		LogFunc: logging.LogOutput,
	}

	queueSize := 10
	commandsPerSecond := 2

	rlb := NewRateLimitedBlocker(processor, mockBlocker, queueSize, commandsPerSecond)
	defer rlb.Stop()

	numCommands := 5
	for i := 0; i < numCommands; i++ {
		ip := IPInfo{Address: fmt.Sprintf("192.168.1.%d", i)}
		rlb.Unblock(ip)
	}

	time.Sleep(2 * time.Second)

	if unblockCount.Load() > int32(commandsPerSecond*2) {
		t.Errorf("Expected at most %d unblocks after 2 seconds, got %d", commandsPerSecond*2, unblockCount.Load())
	}

	time.Sleep(3 * time.Second)

	if unblockCount.Load() != int32(numCommands) {
		t.Errorf("Expected %d unblocks after sufficient time, got %d", numCommands, unblockCount.Load())
	}
}

func TestRateLimitedBlocker_QueueFull(t *testing.T) {
	var blockCount atomic.Int32
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCount.Add(1)
			return nil
		},
	}
	processor := &Processor{
		LogFunc: logging.LogOutput,
	}

	queueSize := 2
	commandsPerSecond := 1

	rlb := NewRateLimitedBlocker(processor, mockBlocker, queueSize, commandsPerSecond)
	defer rlb.Stop()

	// Fill the queue and send one more to overflow
	for i := 0; i < queueSize+1; i++ {
		ip := IPInfo{Address: fmt.Sprintf("192.168.1.%d", i)}
		rlb.Block(ip, 5*time.Minute)
	}

	// The queue has size 2, so 2 commands should be in the queue, 1 should be dropped.
	// The worker will process them slowly.
	time.Sleep(100 * time.Millisecond) // Give a little time for the queue to be filled

	// Check that the queue is full (or close to it) and some might have been dropped
	if len(rlb.CommandQueue) > queueSize {
		t.Errorf("Expected queue size to be at most %d, got %d", queueSize, len(rlb.CommandQueue))
	}

	// Wait for all commands that were accepted to be processed
	time.Sleep(time.Duration(queueSize+1) * time.Second) // Enough time for all accepted commands to process

	// We expect only `queueSize` commands to be processed, as one was dropped.
	if blockCount.Load() != int32(queueSize) {
		t.Errorf("Expected %d blocks (queue size), got %d", queueSize, blockCount.Load())
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
	}

	queueSize := 10
	commandsPerSecond := 0 // Zero rate should disable the worker

	rlb := NewRateLimitedBlocker(processor, mockBlocker, queueSize, commandsPerSecond)
	defer rlb.Stop()

	ip := IPInfo{Address: "192.168.1.1"}
	rlb.Block(ip, 5*time.Minute)

	time.Sleep(100 * time.Millisecond) // Give some time

	if blockCount.Load() != 0 {
		t.Errorf("Expected 0 blocks when rate is zero, got %d", blockCount.Load())
	}
	if len(rlb.CommandQueue) != 1 {
		t.Errorf("Expected 1 command in queue when rate is zero, got %d", len(rlb.CommandQueue))
	}
}

func TestRateLimitedBlocker_Stop(t *testing.T) {
	var blockCount atomic.Int32
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo IPInfo, duration time.Duration) error {
			blockCount.Add(1)
			return nil
		},
	}
	processor := &Processor{
		LogFunc: logging.LogOutput,
	}

	queueSize := 10
	commandsPerSecond := 1

	rlb := NewRateLimitedBlocker(processor, mockBlocker, queueSize, commandsPerSecond)

	ip := IPInfo{Address: "192.168.1.1"}
	rlb.Block(ip, 5*time.Minute)

	time.Sleep(50 * time.Millisecond) // Allow command to be queued

	rlb.Stop() // Stop the worker

	time.Sleep(2 * time.Second) // Give time for worker to shut down and no more processing

	// At most one command should have been processed if the timing was just right,
	// but likely none if Stop() was called quickly.
	if blockCount.Load() > 1 {
		t.Errorf("Expected at most 1 block after stopping, got %d", blockCount.Load())
	}
}
