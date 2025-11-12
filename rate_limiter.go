package main

import (
	"bot-detector/internal/logging"
	"sync"
	"time"
)

// BlockerCommand defines a command to be executed by the blocker.
type BlockerCommand struct {
	Action   string
	IPInfo   IPInfo
	Duration time.Duration
}

// RateLimitedBlocker is a Blocker that queues commands and executes them at a given rate.
type RateLimitedBlocker struct {
	P              *Processor
	WrappedBlocker Blocker
	CommandQueue   chan BlockerCommand
	stopCh         chan struct{}
	wg             sync.WaitGroup
	once           sync.Once
}

// NewRateLimitedBlocker creates a new RateLimitedBlocker.
func NewRateLimitedBlocker(p *Processor, wrapped Blocker, queueSize int, commandsPerSecond int) *RateLimitedBlocker {
	rlb := &RateLimitedBlocker{
		P:              p,
		WrappedBlocker: wrapped,
		CommandQueue:   make(chan BlockerCommand, queueSize),
		stopCh:         make(chan struct{}),
	}
	rlb.wg.Add(1)
	go rlb.worker(commandsPerSecond)
	return rlb
}

// Block adds a block command to the queue.
func (b *RateLimitedBlocker) Block(ipInfo IPInfo, duration time.Duration) error {
	command := BlockerCommand{
		Action:   "block",
		IPInfo:   ipInfo,
		Duration: duration,
	}

	select {
	case b.CommandQueue <- command:
		b.P.LogFunc(logging.LevelDebug, "RATE_LIMITER", "Queued block command for IP %s.", ipInfo.Address)
		b.P.Metrics.BlockerCmdsQueued.Add(1)
	default:
		b.P.LogFunc(logging.LevelWarning, "RATE_LIMITER", "Command queue is full. Dropping block command for IP %s.", ipInfo.Address)
		b.P.Metrics.BlockerCmdsDropped.Add(1)
	}

	return nil
}

// Unblock adds an unblock command to the queue.
func (b *RateLimitedBlocker) Unblock(ipInfo IPInfo) error {
	command := BlockerCommand{
		Action: "unblock",
		IPInfo: ipInfo,
	}

	select {
	case b.CommandQueue <- command:
		b.P.LogFunc(logging.LevelDebug, "RATE_LIMITER", "Queued unblock command for IP %s.", ipInfo.Address)
		b.P.Metrics.BlockerCmdsQueued.Add(1)
	default:
		b.P.LogFunc(logging.LevelWarning, "RATE_LIMITER", "Command queue is full. Dropping unblock command for IP %s.", ipInfo.Address)
		b.P.Metrics.BlockerCmdsDropped.Add(1)
	}

	return nil
}

// Stop stops the command queue worker.
func (b *RateLimitedBlocker) Stop() {
	b.once.Do(func() { // Ensure stop logic is only run once.
		close(b.stopCh) // Signal the worker to stop.
		b.wg.Wait()     // Wait for the worker goroutine to finish.
	})
}

// worker is the background goroutine that processes commands from the queue at a controlled rate.
func (rlb *RateLimitedBlocker) worker(commandsPerSecond int) {
	// If commandsPerSecond is 0 or less, the worker will not process any commands.
	// This effectively disables the blocker while still allowing commands to be queued (and dropped if the queue is full).
	p := rlb.P
	defer rlb.wg.Done()

	if commandsPerSecond <= 0 {
		p.LogFunc(logging.LevelInfo, "BLOCKER_RATE_LIMIT", "Blocker rate is <= 0, worker will not process commands.")
		return
	}
	ticker := time.NewTicker(time.Second / time.Duration(commandsPerSecond))
	defer ticker.Stop()

	p.LogFunc(logging.LevelInfo, "BLOCKER_RATE_LIMIT", "Starting blocker worker with a rate of %d commands/sec.", commandsPerSecond)

	for {
		select {
		case <-rlb.stopCh:
			p.LogFunc(logging.LevelInfo, "BLOCKER_RATE_LIMIT", "Stopping blocker worker.")
			return
		case <-ticker.C:
			select {
			case cmd := <-rlb.CommandQueue:
				rlb.P.Metrics.BlockerCmdsExecuted.Add(1)
				var err error
				if cmd.Action == "block" {
					err = rlb.WrappedBlocker.Block(cmd.IPInfo, cmd.Duration)
				} else {
					err = rlb.WrappedBlocker.Unblock(cmd.IPInfo)
				}
				if err != nil {
					p.LogFunc(logging.LevelError, "BLOCKER_CMD_ERROR", "Error executing %s command for IP %s: %v", cmd.Action, cmd.IPInfo.Address, err)
				}
			default:
				// No command in queue, continue and wait for the next tick or stop signal.
			}
		}
	}
}
