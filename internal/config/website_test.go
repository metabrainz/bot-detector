package config

import (
	"testing"
)

func TestValidateWebsites_Empty(t *testing.T) {
	// Empty websites should pass validation
	err := validateWebsites([]WebsiteConfig{}, []BehavioralChain{})
	if err != nil {
		t.Errorf("Expected no error for empty websites, got: %v", err)
	}
}

func TestValidateWebsites_Valid(t *testing.T) {
	websites := []WebsiteConfig{
		{
			Name:    "main",
			VHosts:  []string{"www.example.com", "example.com"},
			LogPath: "/var/log/main.log",
		},
		{
			Name:    "api",
			VHosts:  []string{"api.example.com"},
			LogPath: "/var/log/api.log",
		},
	}

	chains := []BehavioralChain{
		{Name: "GlobalChain", Websites: []string{}},
		{Name: "MainChain", Websites: []string{"main"}},
		{Name: "APIChain", Websites: []string{"api"}},
		{Name: "BothChain", Websites: []string{"main", "api"}},
	}

	err := validateWebsites(websites, chains)
	if err != nil {
		t.Errorf("Expected no error for valid config, got: %v", err)
	}
}

func TestValidateWebsites_DuplicateName(t *testing.T) {
	websites := []WebsiteConfig{
		{Name: "main", VHosts: []string{"www.example.com"}, LogPath: "/var/log/main.log"},
		{Name: "main", VHosts: []string{"api.example.com"}, LogPath: "/var/log/api.log"},
	}

	err := validateWebsites(websites, []BehavioralChain{})
	if err == nil {
		t.Error("Expected error for duplicate website name")
	}
	if err != nil && err.Error() != "duplicate website name: main" {
		t.Errorf("Expected duplicate name error, got: %v", err)
	}
}

func TestValidateWebsites_DuplicateVHost(t *testing.T) {
	websites := []WebsiteConfig{
		{Name: "main", VHosts: []string{"www.example.com"}, LogPath: "/var/log/main.log"},
		{Name: "api", VHosts: []string{"www.example.com"}, LogPath: "/var/log/api.log"},
	}

	err := validateWebsites(websites, []BehavioralChain{})
	if err == nil {
		t.Error("Expected error for duplicate vhost")
	}
}

func TestValidateWebsites_EmptyName(t *testing.T) {
	websites := []WebsiteConfig{
		{Name: "", VHosts: []string{"www.example.com"}, LogPath: "/var/log/main.log"},
	}

	err := validateWebsites(websites, []BehavioralChain{})
	if err == nil {
		t.Error("Expected error for empty website name")
	}
}

func TestValidateWebsites_EmptyVHosts(t *testing.T) {
	// Empty vhosts is now allowed (catch-all website)
	websites := []WebsiteConfig{
		{Name: "main", VHosts: []string{}, LogPath: "/var/log/main.log"},
	}

	err := validateWebsites(websites, []BehavioralChain{})
	if err != nil {
		t.Errorf("Empty vhosts should be allowed (catch-all website), got error: %v", err)
	}
}

func TestValidateWebsites_EmptyLogPath(t *testing.T) {
	websites := []WebsiteConfig{
		{Name: "main", VHosts: []string{"www.example.com"}, LogPath: ""},
	}

	err := validateWebsites(websites, []BehavioralChain{})
	if err == nil {
		t.Error("Expected error for empty log_path")
	}
}

func TestValidateWebsites_UnknownWebsiteInChain(t *testing.T) {
	websites := []WebsiteConfig{
		{Name: "main", VHosts: []string{"www.example.com"}, LogPath: "/var/log/main.log"},
	}

	chains := []BehavioralChain{
		{Name: "TestChain", Websites: []string{"unknown"}},
	}

	// validateWebsites no longer checks chain references
	err := validateWebsites(websites, chains)
	if err != nil {
		t.Errorf("validateWebsites should not error on unknown website in chain: %v", err)
	}

	// filterInvalidWebsitesFromChains should filter out invalid websites
	validWebsites := map[string]bool{"main": true}
	filterInvalidWebsitesFromChains(chains, validWebsites)

	// Chain should have empty website list after filtering
	if len(chains[0].Websites) != 0 {
		t.Errorf("Expected chain to have 0 websites after filtering, got %d", len(chains[0].Websites))
	}
}

func TestFilterInvalidWebsitesFromChains(t *testing.T) {
	validWebsites := map[string]bool{
		"main": true,
		"api":  true,
	}

	tests := []struct {
		name           string
		inputWebsites  []string
		expectedOutput []string
		expectWarning  bool
	}{
		{
			name:           "All valid websites",
			inputWebsites:  []string{"main", "api"},
			expectedOutput: []string{"main", "api"},
			expectWarning:  false,
		},
		{
			name:           "Some invalid websites",
			inputWebsites:  []string{"main", "unknown", "api"},
			expectedOutput: []string{"main", "api"},
			expectWarning:  true,
		},
		{
			name:           "All invalid websites",
			inputWebsites:  []string{"unknown1", "unknown2"},
			expectedOutput: []string{},
			expectWarning:  true,
		},
		{
			name:           "Global chain (empty list)",
			inputWebsites:  []string{},
			expectedOutput: []string{},
			expectWarning:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chains := []BehavioralChain{
				{Name: "TestChain", Websites: tt.inputWebsites},
			}

			filterInvalidWebsitesFromChains(chains, validWebsites)

			if len(chains[0].Websites) != len(tt.expectedOutput) {
				t.Errorf("Expected %d websites, got %d", len(tt.expectedOutput), len(chains[0].Websites))
			}

			for i, ws := range chains[0].Websites {
				if ws != tt.expectedOutput[i] {
					t.Errorf("Expected website[%d] = %s, got %s", i, tt.expectedOutput[i], ws)
				}
			}
		})
	}
}

func TestValidateWebsites_EmptyVHost(t *testing.T) {
	websites := []WebsiteConfig{
		{Name: "main", VHosts: []string{"www.example.com", ""}, LogPath: "/var/log/main.log"},
	}

	err := validateWebsites(websites, []BehavioralChain{})
	if err == nil {
		t.Error("Expected error for empty vhost in list")
	}
}
