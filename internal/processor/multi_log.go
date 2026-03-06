package processor

import (
	"bot-detector/internal/app"
	"os"
)

// MultiLogTailer tails multiple log files concurrently, one per website.
// Each website's log is processed in its own goroutine.
// All goroutines share the same signal channel for coordinated shutdown.
// This version uses the dynamic manager to support runtime website changes.
func MultiLogTailer(p *app.Processor, signalCh <-chan os.Signal) {
	manager := NewMultiWebsiteTailerManager(p, signalCh)

	// Store manager in processor for config reload access
	p.WebsiteTailerMgr = manager

	// Start initial tailers
	manager.Start()

	// Wait for all tailers to finish (blocks until shutdown)
	manager.Wait()
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
