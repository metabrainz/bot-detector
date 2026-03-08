package processor

import (
	"bot-detector/internal/app"
	"bot-detector/internal/logparser"
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

// liveLogTailerWithPath is like LiveLogTailer but accepts logPath and website as parameters
// to avoid race conditions when multiple goroutines tail different files.
func liveLogTailerWithPath(p *app.Processor, logPath, website string, signalCh <-chan os.Signal, readySignal chan<- struct{}) {
	// Set LogPath for this tailer
	// CRITICAL: We set p.LogPath here, and LiveLogTailer reads it immediately at startup
	// We don't restore it because each tailer reads it once at the start
	p.LogPathMutex.Lock()
	p.LogPath = logPath
	
	// Create a website-specific ProcessLogLine that calls ProcessLogLineWithWebsite
	// This avoids race conditions from sharing p.ProcessLogLine
	p.ProcessLogLine = func(line string) {
		logparser.ProcessLogLineWithWebsite(p, line, website)
	}
	p.LogPathMutex.Unlock()

	// LiveLogTailer will read p.LogPath and p.ProcessLogLine immediately at startup
	// and capture them locally, so it's safe even if another tailer changes them later
	LiveLogTailer(p, signalCh, readySignal)
}

// IsMultiWebsiteMode returns true if the processor is configured for multi-website mode.
func IsMultiWebsiteMode(p *app.Processor) bool {
	return len(p.Websites) > 0
}
