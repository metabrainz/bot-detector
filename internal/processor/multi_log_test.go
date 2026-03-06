package processor

import (
	"bot-detector/internal/app"
	"bot-detector/internal/config"
	"testing"
)

func TestIsMultiWebsiteMode(t *testing.T) {
	tests := []struct {
		name     string
		websites []config.WebsiteConfig
		expected bool
	}{
		{
			name:     "no websites - legacy mode",
			websites: []config.WebsiteConfig{},
			expected: false,
		},
		{
			name:     "nil websites - legacy mode",
			websites: nil,
			expected: false,
		},
		{
			name: "one website - multi-website mode",
			websites: []config.WebsiteConfig{
				{Name: "main", VHosts: []string{"www.example.com"}},
			},
			expected: true,
		},
		{
			name: "multiple websites - multi-website mode",
			websites: []config.WebsiteConfig{
				{Name: "main", VHosts: []string{"www.example.com"}},
				{Name: "api", VHosts: []string{"api.example.com"}},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &app.Processor{
				Websites: tt.websites,
			}
			result := IsMultiWebsiteMode(p)
			if result != tt.expected {
				t.Errorf("IsMultiWebsiteMode() = %v, want %v", result, tt.expected)
			}
		})
	}
}
