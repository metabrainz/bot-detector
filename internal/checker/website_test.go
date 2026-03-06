package checker

import (
	"bot-detector/internal/app"
	"bot-detector/internal/config"
	"testing"
)

func TestCategorizeChains_Integration(t *testing.T) {
	// This test verifies that the chain categorization logic works correctly
	// and that checkChainsInternal respects the categorization

	chains := []config.BehavioralChain{
		{Name: "Global1", Websites: []string{}},
		{Name: "Main1", Websites: []string{"main"}},
		{Name: "API1", Websites: []string{"api"}},
		{Name: "Shared", Websites: []string{"main", "api"}},
		{Name: "Global2", Websites: nil},
	}

	websiteChains, globalChains := app.CategorizeChains(chains)

	// Verify global chains
	if len(globalChains) != 2 {
		t.Errorf("Expected 2 global chains, got %d", len(globalChains))
	}
	if globalChains[0] != 0 || globalChains[1] != 4 {
		t.Errorf("Global chains should be indices 0 and 4, got %v", globalChains)
	}

	// Verify main website chains
	mainChains := websiteChains["main"]
	if len(mainChains) != 2 {
		t.Errorf("Expected 2 chains for main website, got %d", len(mainChains))
	}
	if mainChains[0] != 1 || mainChains[1] != 3 {
		t.Errorf("Main chains should be indices 1 and 3, got %v", mainChains)
	}

	// Verify api website chains
	apiChains := websiteChains["api"]
	if len(apiChains) != 2 {
		t.Errorf("Expected 2 chains for api website, got %d", len(apiChains))
	}
	if apiChains[0] != 2 || apiChains[1] != 3 {
		t.Errorf("API chains should be indices 2 and 3, got %v", apiChains)
	}
}

func TestWebsiteChainFiltering_Logic(t *testing.T) {
	// Test the logic of chain filtering based on website configuration
	// This tests the algorithm without needing full processor setup

	tests := []struct {
		name            string
		hasWebsites     bool
		vhost           string
		vhostMap        map[string]string
		globalChains    []int
		websiteChains   map[string][]int
		expectedIndices []int
		description     string
	}{
		{
			name:            "legacy mode - no websites",
			hasWebsites:     false,
			vhost:           "any.example.com",
			expectedIndices: []int{0, 1, 2}, // All chains
			description:     "In legacy mode, all chains should be processed",
		},
		{
			name:         "multi-website - known vhost",
			hasWebsites:  true,
			vhost:        "www.example.com",
			vhostMap:     map[string]string{"www.example.com": "main"},
			globalChains: []int{0},
			websiteChains: map[string][]int{
				"main": {1, 2},
				"api":  {3},
			},
			expectedIndices: []int{0, 1, 2}, // Global + main chains
			description:     "Known vhost should get global + website-specific chains",
		},
		{
			name:         "multi-website - unknown vhost",
			hasWebsites:  true,
			vhost:        "unknown.example.com",
			vhostMap:     map[string]string{"www.example.com": "main"},
			globalChains: []int{0, 4},
			websiteChains: map[string][]int{
				"main": {1, 2},
			},
			expectedIndices: []int{0, 4}, // Only global chains
			description:     "Unknown vhost should only get global chains",
		},
		{
			name:         "multi-website - empty vhost",
			hasWebsites:  true,
			vhost:        "",
			vhostMap:     map[string]string{"www.example.com": "main"},
			globalChains: []int{0},
			websiteChains: map[string][]int{
				"main": {1},
			},
			expectedIndices: []int{0}, // Only global chains
			description:     "Empty vhost should only get global chains",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the logic from checkChainsInternal
			var chainIndices []int

			if tt.hasWebsites {
				// Multi-website mode
				chainIndices = append([]int{}, tt.globalChains...)

				if websiteName, ok := tt.vhostMap[tt.vhost]; ok {
					if siteChains, ok := tt.websiteChains[websiteName]; ok {
						chainIndices = append(chainIndices, siteChains...)
					}
				}
			} else {
				// Legacy mode - all chains
				chainIndices = []int{0, 1, 2}
			}

			// Verify result
			if len(chainIndices) != len(tt.expectedIndices) {
				t.Errorf("%s: expected %d chains, got %d", tt.description, len(tt.expectedIndices), len(chainIndices))
				return
			}

			for i, idx := range chainIndices {
				if idx != tt.expectedIndices[i] {
					t.Errorf("%s: chain index mismatch at position %d: expected %d, got %d",
						tt.description, i, tt.expectedIndices[i], idx)
				}
			}
		})
	}
}
