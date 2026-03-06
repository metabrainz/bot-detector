package processor

import (
	"bot-detector/internal/app"
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

		// Capture values for goroutine closure
		websiteName := website.Name
		logPath := website.LogPath

		go func() {
			defer wg.Done()

			p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "Starting tailer for website '%s' on %s", websiteName, logPath)

			// Use liveLogTailerWithPath to avoid race on p.LogPath
			liveLogTailerWithPath(p, logPath, signalCh, nil)

			p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "Tailer stopped for website '%s'", websiteName)
		}()
	}

	// Wait for all tailers to finish
	wg.Wait()
	p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "All website tailers stopped")
}

// liveLogTailerWithPath is like LiveLogTailer but accepts logPath as a parameter
// to avoid race conditions when multiple goroutines tail different files.
func liveLogTailerWithPath(p *app.Processor, logPath string, signalCh <-chan os.Signal, readySignal chan<- struct{}) {
	// Safely set LogPath for this goroutine's use
	p.LogPathMutex.Lock()
	originalLogPath := p.LogPath
	p.LogPath = logPath
	p.LogPathMutex.Unlock()

	// Ensure we restore the original value
	defer func() {
		p.LogPathMutex.Lock()
		p.LogPath = originalLogPath
		p.LogPathMutex.Unlock()
	}()

	LiveLogTailer(p, signalCh, readySignal)
}

// IsMultiWebsiteMode returns true if the processor is configured for multi-website mode.
func IsMultiWebsiteMode(p *app.Processor) bool {
	return len(p.Websites) > 0
}
