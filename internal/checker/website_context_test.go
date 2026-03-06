package checker

import (
	"bot-detector/internal/app"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/metrics"
	"bot-detector/internal/store"
	"bot-detector/internal/types"
	"bot-detector/internal/utils"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestChainCompletion_WebsiteContext tests that website context is included in completion logs
func TestChainCompletion_WebsiteContext(t *testing.T) {
	tests := []struct {
		name             string
		websites         []config.WebsiteConfig
		vhostMap         map[string]string
		entryVHost       string
		expectedInLog    string
		expectedNotInLog string
	}{
		{
			name: "multi-website mode with known vhost",
			websites: []config.WebsiteConfig{
				{Name: "main", VHosts: []string{"www.example.com"}},
				{Name: "api", VHosts: []string{"api.example.com"}},
			},
			vhostMap: map[string]string{
				"www.example.com": "main",
				"api.example.com": "api",
			},
			entryVHost:       "api.example.com",
			expectedInLog:    "on website 'api' (vhost: api.example.com)",
			expectedNotInLog: "",
		},
		{
			name: "multi-website mode with unknown vhost",
			websites: []config.WebsiteConfig{
				{Name: "main", VHosts: []string{"www.example.com"}},
			},
			vhostMap: map[string]string{
				"www.example.com": "main",
			},
			entryVHost:       "unknown.example.com",
			expectedInLog:    "",
			expectedNotInLog: "on website",
		},
		{
			name:             "single-website mode (no websites configured)",
			websites:         []config.WebsiteConfig{},
			vhostMap:         nil,
			entryVHost:       "www.example.com",
			expectedInLog:    "",
			expectedNotInLog: "on website",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var loggedMessages []string
			var logMutex sync.Mutex

			p := &app.Processor{
				ActivityMutex: &sync.RWMutex{},
				ActivityStore: make(map[store.Actor]*store.ActorActivity),
				ConfigMutex:   &sync.RWMutex{},
				Metrics:       metrics.NewMetrics(),
				EnableMetrics: true,
				DryRun:        true,
				Websites:      tt.websites,
				VHostToWebsite: tt.vhostMap,
				ReasonCache:    make(map[string]*string),
				LogFunc: func(level logging.LogLevel, tag string, format string, v ...interface{}) {
					logMutex.Lock()
					defer logMutex.Unlock()
					// Actually format the message
					msg := fmt.Sprintf(format, v...)
					loggedMessages = append(loggedMessages, msg)
				},
			}

			chain := &config.BehavioralChain{
				Name:          "TestChain",
				Action:        "block",
				BlockDuration: 30 * time.Minute,
			}

			entry := &types.LogEntry{
				VHost:  tt.entryVHost,
				IPInfo: utils.IPInfo{Address: "10.0.0.1", Version: utils.VersionIPv4},
			}

			activity := &store.ActorActivity{}

			// Call the function
			handleChainCompletion(p, chain, entry, activity)

			// Check logged messages
			logMutex.Lock()
			defer logMutex.Unlock()

			if len(loggedMessages) == 0 {
				t.Fatal("No messages were logged")
			}

			logMsg := loggedMessages[0]

			if tt.expectedInLog != "" {
				if !strings.Contains(logMsg, tt.expectedInLog) {
					t.Errorf("Expected log to contain %q, but got: %s", tt.expectedInLog, logMsg)
				}
			}

			if tt.expectedNotInLog != "" {
				if strings.Contains(logMsg, tt.expectedNotInLog) {
					t.Errorf("Expected log NOT to contain %q, but got: %s", tt.expectedNotInLog, logMsg)
				}
			}
		})
	}
}

// TestChainCompletion_LiveMode tests website context in live mode (non-dry-run)
func TestChainCompletion_LiveMode(t *testing.T) {
	var loggedMessage string

	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:   &sync.RWMutex{},
		Metrics:       metrics.NewMetrics(),
		EnableMetrics: true,
		DryRun:        true, // Use dry-run to avoid needing a real blocker
		Websites: []config.WebsiteConfig{
			{Name: "production", VHosts: []string{"prod.example.com"}},
		},
		VHostToWebsite: map[string]string{
			"prod.example.com": "production",
		},
		ReasonCache: make(map[string]*string),
		LogFunc: func(level logging.LogLevel, tag string, format string, v ...interface{}) {
			loggedMessage = fmt.Sprintf(format, v...)
		},
	}

	chain := &config.BehavioralChain{
		Name:          "ProdChain",
		Action:        "block",
		BlockDuration: 1 * time.Hour,
	}

	entry := &types.LogEntry{
		VHost:  "prod.example.com",
		IPInfo: utils.IPInfo{Address: "192.168.1.1", Version: utils.VersionIPv4},
	}

	activity := &store.ActorActivity{}

	handleChainCompletion(p, chain, entry, activity)

	expectedSubstrings := []string{
		"BLOCK!",
		"ProdChain",
		"192.168.1.1",
		"on website 'production'",
		"vhost: prod.example.com",
		"(DryRun)", // Since we're using dry-run mode
	}

	for _, substr := range expectedSubstrings {
		if !strings.Contains(loggedMessage, substr) {
			t.Errorf("Expected log to contain %q, but got: %s", substr, loggedMessage)
		}
	}
}
