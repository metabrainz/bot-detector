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

// liveLogTailerWithPath is like LiveLogTailer but accepts logPath and website as parameters
// to avoid race conditions when multiple goroutines tail different files.
func liveLogTailerWithPath(p *app.Processor, logPath, website string, signalCh <-chan os.Signal, readySignal chan<- struct{}) {
	// Create a wrapper for ProcessLogLine that captures the website name
	// This avoids race conditions with the shared CurrentWebsite variable
	p.LogPathMutex.Lock()
	originalLogPath := p.LogPath
	originalProcessLogLine := p.ProcessLogLine
	p.LogPath = logPath
	
	// Wrap the original ProcessLogLine to inject the website parameter
	wrappedProcessLogLine := func(line string) {
		// Temporarily set CurrentWebsite for this line processing
		// This is needed for backward compatibility with code that reads CurrentWebsite
		p.LogPathMutex.Lock()
		savedWebsite := p.CurrentWebsite
		p.CurrentWebsite = website
		p.LogPathMutex.Unlock()
		
		// Call the original function
		originalProcessLogLine(line)
		
		// Restore the previous value
		p.LogPathMutex.Lock()
		p.CurrentWebsite = savedWebsite
		p.LogPathMutex.Unlock()
	}
	
	p.ProcessLogLine = wrappedProcessLogLine
	p.LogPathMutex.Unlock()

	// Ensure we restore the original values
	defer func() {
		p.LogPathMutex.Lock()
		p.LogPath = originalLogPath
		p.ProcessLogLine = originalProcessLogLine
		p.LogPathMutex.Unlock()
	}()

	LiveLogTailer(p, signalCh, readySignal)
}

// IsMultiWebsiteMode returns true if the processor is configured for multi-website mode.
func IsMultiWebsiteMode(p *app.Processor) bool {
	return len(p.Websites) > 0
}
