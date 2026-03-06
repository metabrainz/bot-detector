package processor

import (
	"bot-detector/internal/app"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"os"
	"sync"
)

// MultiLogTailer tails multiple log files concurrently, one per website.
// Each website's log is processed in its own goroutine.
// All goroutines share the same signal channel for coordinated shutdown.
func MultiLogTailer(p *app.Processor, signalCh <-chan os.Signal) {
	var wg sync.WaitGroup

	for _, website := range p.Websites {
		wg.Add(1)
		go func(ws config.WebsiteConfig) {
			defer wg.Done()

			// Create a sub-processor context with this website's log path
			// We share everything except the LogPath
			subP := *p
			subP.LogPath = ws.LogPath

			p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "Starting tailer for website '%s' on %s", ws.Name, ws.LogPath)

			// Tail this website's log file
			// Pass nil for readySignal since we don't need per-website ready signals
			LiveLogTailer(&subP, signalCh, nil)

			p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "Tailer stopped for website '%s'", ws.Name)
		}(website)
	}

	// Wait for all tailers to finish
	wg.Wait()
	p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "All website tailers stopped")
}

// IsMultiWebsiteMode returns true if the processor is configured for multi-website mode.
func IsMultiWebsiteMode(p *app.Processor) bool {
	return len(p.Websites) > 0
}
