package app

import (
	"bot-detector/internal/config"
	"reflect"
	"testing"
)

func TestBuildVHostMap(t *testing.T) {
	tests := []struct {
		name     string
		websites []config.WebsiteConfig
		expected map[string]string
	}{
		{
			name:     "empty websites",
			websites: []config.WebsiteConfig{},
			expected: map[string]string{},
		},
		{
			name: "single website single vhost",
			websites: []config.WebsiteConfig{
				{Name: "main", VHosts: []string{"www.example.com"}},
			},
			expected: map[string]string{
				"www.example.com": "main",
			},
		},
		{
			name: "single website multiple vhosts",
			websites: []config.WebsiteConfig{
				{Name: "main", VHosts: []string{"www.example.com", "example.com"}},
			},
			expected: map[string]string{
				"www.example.com": "main",
				"example.com":     "main",
			},
		},
		{
			name: "multiple websites",
			websites: []config.WebsiteConfig{
				{Name: "main", VHosts: []string{"www.example.com", "example.com"}},
				{Name: "api", VHosts: []string{"api.example.com"}},
				{Name: "admin", VHosts: []string{"admin.example.com"}},
			},
			expected: map[string]string{
				"www.example.com":   "main",
				"example.com":       "main",
				"api.example.com":   "api",
				"admin.example.com": "admin",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, _ := BuildVHostMap(tt.websites)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("BuildVHostMap() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestCategorizeChains(t *testing.T) {
	tests := []struct {
		name            string
		chains          []config.BehavioralChain
		expectedWebsite map[string][]int
		expectedGlobal  []int
	}{
		{
			name:            "empty chains",
			chains:          []config.BehavioralChain{},
			expectedWebsite: map[string][]int{},
			expectedGlobal:  nil,
		},
		{
			name: "all global chains",
			chains: []config.BehavioralChain{
				{Name: "Chain1", Websites: []string{}},
				{Name: "Chain2", Websites: nil},
			},
			expectedWebsite: map[string][]int{},
			expectedGlobal:  []int{0, 1},
		},
		{
			name: "all website-specific chains",
			chains: []config.BehavioralChain{
				{Name: "MainChain", Websites: []string{"main"}},
				{Name: "APIChain", Websites: []string{"api"}},
			},
			expectedWebsite: map[string][]int{
				"main": {0},
				"api":  {1},
			},
			expectedGlobal: nil,
		},
		{
			name: "mixed global and website-specific",
			chains: []config.BehavioralChain{
				{Name: "GlobalChain", Websites: []string{}},
				{Name: "MainChain", Websites: []string{"main"}},
				{Name: "APIChain", Websites: []string{"api"}},
			},
			expectedWebsite: map[string][]int{
				"main": {1},
				"api":  {2},
			},
			expectedGlobal: []int{0},
		},
		{
			name: "chain applies to multiple websites",
			chains: []config.BehavioralChain{
				{Name: "SharedChain", Websites: []string{"main", "api"}},
				{Name: "MainOnly", Websites: []string{"main"}},
			},
			expectedWebsite: map[string][]int{
				"main": {0, 1},
				"api":  {0},
			},
			expectedGlobal: nil,
		},
		{
			name: "complex scenario",
			chains: []config.BehavioralChain{
				{Name: "Global1", Websites: []string{}},
				{Name: "Main1", Websites: []string{"main"}},
				{Name: "Global2", Websites: nil},
				{Name: "Shared", Websites: []string{"main", "api", "admin"}},
				{Name: "API1", Websites: []string{"api"}},
			},
			expectedWebsite: map[string][]int{
				"main":  {1, 3},
				"api":   {3, 4},
				"admin": {3},
			},
			expectedGlobal: []int{0, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			websiteChains, globalChains := CategorizeChains(tt.chains)

			if !reflect.DeepEqual(websiteChains, tt.expectedWebsite) {
				t.Errorf("CategorizeChains() websiteChains = %v, want %v", websiteChains, tt.expectedWebsite)
			}

			if !reflect.DeepEqual(globalChains, tt.expectedGlobal) {
				t.Errorf("CategorizeChains() globalChains = %v, want %v", globalChains, tt.expectedGlobal)
			}
		})
	}
}
