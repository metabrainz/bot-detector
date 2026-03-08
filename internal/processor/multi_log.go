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
	// Capture the original ProcessLogLine ONCE before any tailers start
	// This prevents nested wrappers when multiple tailers run concurrently
	if p.OriginalProcessLogLine == nil {
		p.OriginalProcessLogLine = p.ProcessLogLine
	}
	
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
	// Set LogPath and ProcessLogLine for this tailer
	// CRITICAL: We set p.LogPath here, and LiveLogTailer reads it immediately at startup
	// We don't restore it because each tailer reads it once at the start
	p.LogPathMutex.Lock()
	p.LogPath = logPath
	
	// Use the ORIGINAL ProcessLogLine (captured at startup) to avoid nested wrappers
	baseProcessLogLine := p.OriginalProcessLogLine
	if baseProcessLogLine == nil {
		baseProcessLogLine = p.ProcessLogLine
	}
	
	// Create a closure that sets CurrentWebsite before calling the base function
	p.ProcessLogLine = func(line string) {
		p.LogPathMutex.Lock()
		savedWebsite := p.CurrentWebsite
		p.CurrentWebsite = website
		p.LogPathMutex.Unlock()
		
		baseProcessLogLine(line)
		
		p.LogPathMutex.Lock()
		p.CurrentWebsite = savedWebsite
		p.LogPathMutex.Unlock()
	}
	p.LogPathMutex.Unlock()

	// LiveLogTailer will read p.LogPath immediately at startup (line 460)
	// and capture it locally, so it's safe even if another tailer changes it later
	LiveLogTailer(p, signalCh, readySignal)
	
	// Note: We don't restore p.LogPath or p.ProcessLogLine because:
	// 1. Each tailer captures them locally at startup
	// 2. Restoring them causes races when tailers start concurrently
}

// IsMultiWebsiteMode returns true if the processor is configured for multi-website mode.
func IsMultiWebsiteMode(p *app.Processor) bool {
	return len(p.Websites) > 0
}
