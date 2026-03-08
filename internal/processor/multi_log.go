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
	// Set LogPath and create a website-specific ProcessLogLine
	p.LogPathMutex.Lock()
	originalLogPath := p.LogPath
	p.LogPath = logPath
	
	// Use the ORIGINAL ProcessLogLine (captured at startup) to avoid nested wrappers
	// Each tailer wraps the same original function, not each other's wrappers
	baseProcessLogLine := p.OriginalProcessLogLine
	if baseProcessLogLine == nil {
		// Fallback if not set (shouldn't happen in production)
		baseProcessLogLine = p.ProcessLogLine
	}
	
	// Create a closure that sets CurrentWebsite before calling the base function
	p.ProcessLogLine = func(line string) {
		// Set CurrentWebsite for this line
		p.LogPathMutex.Lock()
		savedWebsite := p.CurrentWebsite
		p.CurrentWebsite = website
		p.LogPathMutex.Unlock()
		
		// Call the base ProcessLogLine
		baseProcessLogLine(line)
		
		// Restore CurrentWebsite
		p.LogPathMutex.Lock()
		p.CurrentWebsite = savedWebsite
		p.LogPathMutex.Unlock()
	}
	p.LogPathMutex.Unlock()

	// Ensure we restore the original values when done
	defer func() {
		p.LogPathMutex.Lock()
		p.LogPath = originalLogPath
		p.ProcessLogLine = baseProcessLogLine
		p.LogPathMutex.Unlock()
	}()

	LiveLogTailer(p, signalCh, readySignal)
}

// IsMultiWebsiteMode returns true if the processor is configured for multi-website mode.
func IsMultiWebsiteMode(p *app.Processor) bool {
	return len(p.Websites) > 0
}
