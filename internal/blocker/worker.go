package blocker

import (
	"bot-detector/internal/logging"
	"time"
)

// worker is the background goroutine that processes commands from the queue at a controlled rate.
func (rlb *RateLimitedBlocker) worker(commandsPerSecond int) {
	defer rlb.wg.Done()

	if commandsPerSecond <= 0 {
		rlb.Log(logging.LevelInfo, "BLOCKER_RATE_LIMIT", "Blocker rate is <= 0, worker will not process commands.")
		return
	}
	ticker := time.NewTicker(time.Second / time.Duration(commandsPerSecond))
	defer ticker.Stop()

	rlb.Log(logging.LevelInfo, "BLOCKER_RATE_LIMIT", "Starting blocker worker with a rate of %d commands/sec.", commandsPerSecond)

	for {
		select {
		case <-rlb.stopCh:
			rlb.Log(logging.LevelInfo, "BLOCKER_RATE_LIMIT", "Stopping blocker worker.")
			return
		case <-ticker.C:
			select {
			case cmd := <-rlb.CommandQueue:
				rlb.IncrementBlockerCmdsExecuted()
				var err error
				if cmd.Action == "block" {
					err = rlb.WrappedBlocker.Block(cmd.IPInfo, cmd.Duration)
				} else {
					err = rlb.WrappedBlocker.Unblock(cmd.IPInfo)
				}
				if err != nil {
					rlb.Log(logging.LevelError, "BLOCKER_CMD_ERROR", "Error executing %s command for IP %s: %v", cmd.Action, cmd.IPInfo.Address, err)
				}
			default:
			}
		}
	}
}
