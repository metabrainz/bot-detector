package main

import (
	"net"
	"testing"
)

func TestIsIPWhitelisted(t *testing.T) {
	// Ensure a clean starting state
	resetGlobalState() // Assumed to be available from main_test.go

	// Setup multiple CIDR whitelists (IPv4 and IPv6)
	_, netV4, _ := net.ParseCIDR("192.168.0.0/24")
	_, netV6, _ := net.ParseCIDR("2001:db8::/32")

	// Set the global WhitelistNets
	ChainMutex.Lock()
	WhitelistNets = []*net.IPNet{netV4, netV6}
	ChainMutex.Unlock()

	tests := []struct {
		name     string
		ip       string
		expected bool
	}{
		// IPv4 Tests
		{"IPv4 in range", "192.168.0.10", true},
		{"IPv4 out of range", "192.168.1.1", false},
		{"IPv4 boundary (network address)", "192.168.0.0", true},
		// IPv6 Tests
		{"IPv6 in range", "2001:db8:0:0:0:0:0:1", true},
		{"IPv6 in range (shorthand)", "2001:db8::1", true},
		{"IPv6 out of range", "2001:db9::1", false},
		// Edge Cases
		{"Invalid IP string", "invalid-ip", false},
		{"Empty IP string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsIPWhitelisted(tt.ip)
			if result != tt.expected {
				t.Errorf("IsIPWhitelisted(%s) = %v, want %v", tt.ip, result, tt.expected)
			}
		})
	}
}
