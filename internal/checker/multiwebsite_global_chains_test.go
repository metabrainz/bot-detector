package checker_test

import (
	"bot-detector/internal/app"
	"bot-detector/internal/config"
	"bot-detector/internal/utils"
	"testing"
	"time"
)

// TestMultiWebsite_GlobalChainsApplyToAllVHosts verifies that global chains
// (chains without websites specified) apply to all vhosts in multi-website mode.
func TestMultiWebsite_GlobalChainsApplyToAllVHosts(t *testing.T) {
	// Setup: Create a global chain (no websites specified)
	h := NewCheckerTestHarness(t, nil)

	// Configure multi-website mode
	h.processor.Websites = []config.WebsiteConfig{
		{Name: "site1", VHosts: []string{"site1.example.com"}},
		{Name: "site2", VHosts: []string{"site2.example.com"}},
	}
	h.processor.VHostToWebsite = map[string]string{
		"site1.example.com": "site1",
		"site2.example.com": "site2",
	}

	// Add a global chain (no websites field)
	h.addChain(config.BehavioralChain{
		Name:          "GlobalTestChain",
		MatchKey:      "ip",
		Action:        "block",
		BlockDuration: 1 * time.Hour,
		StepsYAML: []config.StepDefYAML{
			{FieldMatches: map[string]interface{}{"Path": "/test"}},
		},
	})

	// Categorize chains
	h.processor.WebsiteChains, h.processor.GlobalChains = app.CategorizeChains(h.processor.Chains)

	// Verify chain is global
	if len(h.processor.GlobalChains) != 1 {
		t.Fatalf("Expected 1 global chain, got %d", len(h.processor.GlobalChains))
	}

	// Test entry for site1
	entry1 := &app.LogEntry{
		IPInfo:    utils.NewIPInfo("10.0.0.1"),
		VHost:     "site1.example.com",
		Path:      "/test",
		Timestamp: time.Now(),
	}
	h.processEntry(entry1)

	if !h.blockCalled {
		t.Fatal("Expected block for site1.example.com, but it was not called")
	}
	h.blockCalled = false

	// Test entry for site2 with different IP
	entry2 := &app.LogEntry{
		IPInfo:    utils.NewIPInfo("10.0.0.2"),
		VHost:     "site2.example.com",
		Path:      "/test",
		Timestamp: time.Now(),
	}
	h.processEntry(entry2)

	if !h.blockCalled {
		t.Fatal("Expected block for site2.example.com, but it was not called")
	}
}
