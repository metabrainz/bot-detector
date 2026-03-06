package processor

import (
	"bot-detector/internal/app"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"os"
	"path/filepath"
	"sync"
)

// WebsiteTailer manages a single website's log tailer with its own stop channel
type WebsiteTailer struct {
	Name    string
	LogPath string
	StopCh  chan struct{}
	DoneCh  chan struct{}
	Running bool
	mu      sync.Mutex
}

// MultiWebsiteTailerManager manages dynamic starting/stopping of website tailers
type MultiWebsiteTailerManager struct {
	p            *app.Processor
	globalSignal <-chan os.Signal
	tailers      map[string]*WebsiteTailer
	mu           sync.RWMutex
	wg           sync.WaitGroup
}

// NewMultiWebsiteTailerManager creates a new manager
func NewMultiWebsiteTailerManager(p *app.Processor, signalCh <-chan os.Signal) *MultiWebsiteTailerManager {
	return &MultiWebsiteTailerManager{
		p:            p,
		globalSignal: signalCh,
		tailers:      make(map[string]*WebsiteTailer),
	}
}

// Start initializes tailers for all configured websites
func (m *MultiWebsiteTailerManager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, website := range m.p.Websites {
		m.startTailerLocked(website)
	}
}

// startTailerLocked starts a tailer for a website (must hold lock)
func (m *MultiWebsiteTailerManager) startTailerLocked(website config.WebsiteConfig) {
	if _, exists := m.tailers[website.Name]; exists {
		return // Already running
	}

	tailer := &WebsiteTailer{
		Name:    website.Name,
		LogPath: website.LogPath,
		StopCh:  make(chan struct{}),
		DoneCh:  make(chan struct{}),
		Running: true,
	}

	m.tailers[website.Name] = tailer
	m.wg.Add(1)

	go func() {
		defer m.wg.Done()
		defer close(tailer.DoneCh)

		// Resolve to absolute path for clarity
		absPath := tailer.LogPath
		if !filepath.IsAbs(absPath) {
			if abs, err := filepath.Abs(absPath); err == nil {
				absPath = abs
			}
		}

		if absPath != tailer.LogPath {
			m.p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "Starting tailer for website '%s' on %s (resolved to %s)", tailer.Name, tailer.LogPath, absPath)
		} else {
			m.p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "Starting tailer for website '%s' on %s", tailer.Name, tailer.LogPath)
		}

		// Create a combined signal channel that listens to both global signal and tailer-specific stop
		combinedSignal := make(chan os.Signal, 1)
		go func() {
			select {
			case sig := <-m.globalSignal:
				combinedSignal <- sig
			case <-tailer.StopCh:
				close(combinedSignal)
			}
		}()

		liveLogTailerWithPath(m.p, tailer.LogPath, combinedSignal, nil)

		tailer.mu.Lock()
		tailer.Running = false
		tailer.mu.Unlock()

		m.p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "Tailer stopped for website '%s'", tailer.Name)
	}()
}

// UpdateWebsites updates the running tailers based on new website configuration
func (m *MultiWebsiteTailerManager) UpdateWebsites(newWebsites []config.WebsiteConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Build map of new websites
	newWebsiteMap := make(map[string]config.WebsiteConfig)
	for _, website := range newWebsites {
		newWebsiteMap[website.Name] = website
	}

	// Stop tailers for removed websites
	for name, tailer := range m.tailers {
		if _, exists := newWebsiteMap[name]; !exists {
			m.p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "Stopping tailer for removed website '%s'", name)
			close(tailer.StopCh)
			delete(m.tailers, name)
		}
	}

	// Start tailers for new websites
	for _, website := range newWebsites {
		if _, exists := m.tailers[website.Name]; !exists {
			m.p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "Starting tailer for new website '%s'", website.Name)
			m.startTailerLocked(website)
		} else {
			// Website exists - check if log path changed
			existingTailer := m.tailers[website.Name]
			if existingTailer.LogPath != website.LogPath {
				m.p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "Log path changed for website '%s', restarting tailer", website.Name)
				// Stop old tailer
				close(existingTailer.StopCh)
				delete(m.tailers, website.Name)
				// Start new tailer
				m.startTailerLocked(website)
			}
		}
	}
}

// Wait waits for all tailers to finish
func (m *MultiWebsiteTailerManager) Wait() {
	m.wg.Wait()
	m.p.LogFunc(logging.LevelInfo, "MULTI_TAIL", "All website tailers stopped")
}

// Stop stops all tailers
func (m *MultiWebsiteTailerManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, tailer := range m.tailers {
		close(tailer.StopCh)
	}
}
