package blocker

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/utils" // Import utils for IPInfo
	"sync"
	"time"
)

// Blocker defines the interface for external IP blocking services (e.g., HAProxy).
type Blocker interface {
	Block(ipInfo utils.IPInfo, duration time.Duration) error
	Unblock(ipInfo utils.IPInfo) error
	ListBlocked() ([]string, error) // New method
}

// LogProvider defines the interface for logging, decoupling the blocker from the main logger.
type LogProvider interface {
	Log(level logging.LogLevel, tag string, format string, v ...interface{})
}

// MetricsProvider defines the interface for metrics, decoupling the blocker from the main metrics struct.
type MetricsProvider interface {
	IncrementBlockerCmdsQueued()
	IncrementBlockerCmdsDropped()
	IncrementBlockerCmdsExecuted()
}

// BlockerCommand defines a command to be executed by the blocker.
type BlockerCommand struct {
	Action   string
	IPInfo   utils.IPInfo // Use utils.IPInfo
	Duration time.Duration
}

// RateLimitedBlocker is a Blocker that queues commands and executes them at a given rate.
type RateLimitedBlocker struct {
	LogProvider
	MetricsProvider
	WrappedBlocker    Blocker
	CommandQueue      chan BlockerCommand
	stopCh            chan struct{}
	wg                sync.WaitGroup
	once              sync.Once
	QueueSize         int
	CommandsPerSecond int
}

// NewRateLimitedBlocker creates a new RateLimitedBlocker.
func NewRateLimitedBlocker(lp LogProvider, mp MetricsProvider, wrapped Blocker, queueSize int, commandsPerSecond int) *RateLimitedBlocker {
	rlb := &RateLimitedBlocker{
		LogProvider:       lp,
		MetricsProvider:   mp,
		WrappedBlocker:    wrapped,
		CommandQueue:      make(chan BlockerCommand, queueSize),
		stopCh:            make(chan struct{}),
		QueueSize:         queueSize,
		CommandsPerSecond: commandsPerSecond,
	}
	rlb.wg.Add(1)
	go rlb.worker(commandsPerSecond)
	return rlb
}

// Block adds a block command to the queue.
func (b *RateLimitedBlocker) Block(ipInfo utils.IPInfo, duration time.Duration) error {
	command := BlockerCommand{Action: "block", IPInfo: ipInfo, Duration: duration}
	select {
	case b.CommandQueue <- command:
		b.Log(logging.LevelDebug, "RATE_LIMITER", "Queued block command for IP %s.", ipInfo.Address)
		b.IncrementBlockerCmdsQueued()
	default:
		b.Log(logging.LevelWarning, "RATE_LIMITER", "Command queue is full (size: %d). Dropping block command for IP %s. Rate: %d/s.", b.QueueSize, ipInfo.Address, b.CommandsPerSecond)
		b.IncrementBlockerCmdsDropped()
	}
	return nil
}

// Unblock adds an unblock command to the queue.
func (b *RateLimitedBlocker) Unblock(ipInfo utils.IPInfo) error {
	command := BlockerCommand{Action: "unblock", IPInfo: ipInfo}
	select {
	case b.CommandQueue <- command:
		b.Log(logging.LevelDebug, "RATE_LIMITER", "Queued unblock command for IP %s.", ipInfo.Address)
		b.IncrementBlockerCmdsQueued()
	default:
		b.Log(logging.LevelWarning, "RATE_LIMITER", "Command queue is full (size: %d). Dropping unblock command for IP %s. Rate: %d/s.", b.QueueSize, ipInfo.Address, b.CommandsPerSecond)
		b.IncrementBlockerCmdsDropped()
	}
	return nil
}

// ListBlocked retrieves all currently blocked IPs from the wrapped blocker.
func (b *RateLimitedBlocker) ListBlocked() ([]string, error) {
	return b.WrappedBlocker.ListBlocked()
}

// Stop stops the command queue worker.
func (b *RateLimitedBlocker) Stop() {
	b.once.Do(func() {
		close(b.stopCh)
		b.wg.Wait()
	})
}

// IPInfo needs to be defined here to avoid circular dependencies.
type IPInfo struct {
	Address string
	Version byte // Using a byte to be lightweight
}
