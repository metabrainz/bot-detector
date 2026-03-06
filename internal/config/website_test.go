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

	err := validateWebsites(websites, chains)
	if err == nil {
		t.Error("Expected error for unknown website reference in chain")
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
