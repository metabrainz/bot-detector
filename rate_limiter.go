package main

import (
	"bot-detector/internal/logging"
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
	StopChannel    chan struct{}
}

// NewRateLimitedBlocker creates a new RateLimitedBlocker.
func NewRateLimitedBlocker(p *Processor, wrapped Blocker, queueSize int, commandsPerSecond int) *RateLimitedBlocker {
	blocker := &RateLimitedBlocker{
		P:              p,
		WrappedBlocker: wrapped,
		CommandQueue:   make(chan BlockerCommand, queueSize),
		StopChannel:    make(chan struct{}),
	}

	if commandsPerSecond <= 0 {
		p.LogFunc(logging.LevelWarning, "RATE_LIMITER", "Rate limiting is disabled (commandsPerSecond <= 0).")
		return blocker
	}

	// Start the worker goroutine.
	go blocker.commandQueueWorker(commandsPerSecond)

	return blocker
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
	default:
		b.P.LogFunc(logging.LevelWarning, "RATE_LIMITER", "Command queue is full. Dropping block command for IP %s.", ipInfo.Address)
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
	default:
		b.P.LogFunc(logging.LevelWarning, "RATE_LIMITER", "Command queue is full. Dropping unblock command for IP %s.", ipInfo.Address)
	}

	return nil
}

// Stop stops the command queue worker.
func (b *RateLimitedBlocker) Stop() {
	close(b.StopChannel)
}

// commandQueueWorker processes commands from the queue at a given rate.
func (b *RateLimitedBlocker) commandQueueWorker(commandsPerSecond int) {
	// If rate is 0 or negative, do not run the worker.
	if commandsPerSecond <= 0 {
		return
	}
	ticker := time.NewTicker(time.Second / time.Duration(commandsPerSecond))
	defer ticker.Stop()

	b.P.LogFunc(logging.LevelInfo, "RATE_LIMITER", "Starting command queue worker with a rate of %d commands/sec.", commandsPerSecond)

	for {
		select {
		case <-b.StopChannel:
			b.P.LogFunc(logging.LevelInfo, "RATE_LIMITER", "Stopping command queue worker.")
			return
		case <-ticker.C:
			select {
			case cmd := <-b.CommandQueue:
				b.P.LogFunc(logging.LevelDebug, "RATE_LIMITER", "Executing %s command for IP %s.", cmd.Action, cmd.IPInfo.Address)
				var err error
				if cmd.Action == "block" {
					err = b.WrappedBlocker.Block(cmd.IPInfo, cmd.Duration)
				} else {
					err = b.WrappedBlocker.Unblock(cmd.IPInfo)
				}
				if err != nil {
					b.P.LogFunc(logging.LevelError, "RATE_LIMITER", "Error executing %s command for IP %s: %v", cmd.Action, cmd.IPInfo.Address, err)
				}
			default:
				// No command in queue, continue.
			}
		}
	}
}
