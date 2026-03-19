package config

import (
	"path/filepath"
	"testing"
)

func TestResolveWebsiteLogPaths(t *testing.T) {
	// Get current working directory for tests
	cwd, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}

	tests := []struct {
		name           string
		websites       []WebsiteConfig
		rootDir        string
		configFilePath string
		expected       []WebsiteConfig
	}{
		{
			name:           "empty websites",
			websites:       []WebsiteConfig{},
			rootDir:        "",
			configFilePath: "/etc/bot-detector/config.yaml",
			expected:       []WebsiteConfig{},
		},
		{
			name: "no root_dir - defaults to working dir",
			websites: []WebsiteConfig{
				{Name: "main", LogPath: "logs/main.log"},
			},
			rootDir:        "",
			configFilePath: "/etc/bot-detector/config.yaml",
			expected: []WebsiteConfig{
				{Name: "main", LogPath: filepath.Join(cwd, "logs/main.log")},
			},
		},
		{
			name: "absolute root_dir",
			websites: []WebsiteConfig{
				{Name: "main", LogPath: "main.log"},
				{Name: "api", LogPath: "api/access.log"},
			},
			rootDir:        "/var/log/haproxy",
			configFilePath: "/etc/bot-detector/config.yaml",
			expected: []WebsiteConfig{
				{Name: "main", LogPath: "/var/log/haproxy/main.log"},
				{Name: "api", LogPath: "/var/log/haproxy/api/access.log"},
			},
		},
		{
			name: "relative root_dir",
			websites: []WebsiteConfig{
				{Name: "main", LogPath: "main.log"},
			},
			rootDir:        "logs",
			configFilePath: "/etc/bot-detector/config.yaml",
			expected: []WebsiteConfig{
				{Name: "main", LogPath: filepath.Join(cwd, "logs/main.log")},
			},
		},
		{
			name: "absolute log_path ignores root_dir",
			websites: []WebsiteConfig{
				{Name: "main", LogPath: "/absolute/path/main.log"},
				{Name: "api", LogPath: "relative.log"},
			},
			rootDir:        "/var/log",
			configFilePath: "/etc/bot-detector/config.yaml",
			expected: []WebsiteConfig{
				{Name: "main", LogPath: "/absolute/path/main.log"},
				{Name: "api", LogPath: "/var/log/relative.log"},
			},
		},
		{
			name: "empty log_path",
			websites: []WebsiteConfig{
				{Name: "main", LogPath: ""},
			},
			rootDir:        "/var/log",
			configFilePath: "/etc/bot-detector/config.yaml",
			expected: []WebsiteConfig{
				{Name: "main", LogPath: ""},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := resolveWebsiteLogPaths(tt.websites, tt.rootDir, tt.configFilePath)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(result) != len(tt.expected) {
				t.Fatalf("expected %d websites, got %d", len(tt.expected), len(result))
			}

			for i := range result {
				if result[i].Name != tt.expected[i].Name {
					t.Errorf("website[%d].Name: expected %s, got %s", i, tt.expected[i].Name, result[i].Name)
				}

				// Normalize paths for comparison (handle different OS path separators)
				expectedPath := filepath.Clean(tt.expected[i].LogPath)
				resultPath := filepath.Clean(result[i].LogPath)

				if resultPath != expectedPath {
					t.Errorf("website[%d].LogPath: expected %s, got %s", i, expectedPath, resultPath)
				}
			}
		})
	}
}
