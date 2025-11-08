package main

import (
	"net"
	"sync"
	"testing"
)

func TestIsIPWhitelisted(t *testing.T) {
	// Ensure a clean starting state
	resetGlobalState() // Assumed to be available from main_test.go

	// Setup multiple CIDR whitelists (IPv4 and IPv6)
	_, netV4, _ := net.ParseCIDR("192.168.0.0/24")
	_, netV6, _ := net.ParseCIDR("2001:db8::/32")

	// Set the global WhitelistNets
	processor := &Processor{
		ChainMutex: &sync.RWMutex{},
		Config: &AppConfig{
			WhitelistNets: []*net.IPNet{netV4, netV6},
		},
	}
	// Assign the method to the func field for this test instance.
	processor.IsWhitelistedFunc = processor.IsIPWhitelisted

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
			result := processor.IsWhitelistedFunc(NewIPInfo(tt.ip))
			if result != tt.expected {
				t.Errorf("IsIPWhitelisted(%s) = %v, want %v", tt.ip, result, tt.expected)
			}
		})
	}
}

func TestGetTrackingKey(t *testing.T) {
	// Dummy LogEntry for testing
	baseEntry := &LogEntry{
		IPInfo:    NewIPInfo("192.0.2.1"),
		UserAgent: "TestAgent",
	}

	// Test cases for different MatchKeys and IP versions
	tests := []struct {
		name        string
		matchKey    string
		entry       *LogEntry
		expectedKey TrackingKey
	}{
		// --- Success Cases (Key returned) ---
		// 1. IP-only keys
		{"Match: ip (IPv4)", "ip", baseEntry, TrackingKey{IPInfo: NewIPInfo("192.0.2.1"), UA: ""}},
		{"Match: ip (IPv6)", "ip", &LogEntry{IPInfo: NewIPInfo("2001:db8::1")}, TrackingKey{IPInfo: NewIPInfo("2001:db8::1"), UA: ""}},
		{"Match: ipv4 (IPv4)", "ipv4", baseEntry, TrackingKey{IPInfo: NewIPInfo("192.0.2.1"), UA: ""}},
		{"Match: ipv6 (IPv6)", "ipv6", &LogEntry{IPInfo: NewIPInfo("2001:db8::1")}, TrackingKey{IPInfo: NewIPInfo("2001:db8::1"), UA: ""}},

		// 2. IP+UA keys
		{"Match: ip_ua (IPv4)", "ip_ua", baseEntry, TrackingKey{IPInfo: NewIPInfo("192.0.2.1"), UA: "TestAgent"}},
		{"Match: ipv4_ua (IPv4)", "ipv4_ua", baseEntry, TrackingKey{IPInfo: NewIPInfo("192.0.2.1"), UA: "TestAgent"}},
		{"Match: ipv6_ua (IPv6)", "ipv6_ua", &LogEntry{IPInfo: NewIPInfo("2001:db8::1"), UserAgent: "TestAgent"}, TrackingKey{IPInfo: NewIPInfo("2001:db8::1"), UA: "TestAgent"}},

		// --- Failure Cases (Empty Key is now expected) ---
		{"Mismatch: ip (Invalid Version)", "ip", &LogEntry{IPInfo: NewIPInfo("bad-ip")}, TrackingKey{}},
		{"Mismatch: ipv4 (is IPv6)", "ipv4", &LogEntry{IPInfo: NewIPInfo("2001:db8::1")}, TrackingKey{}},
		{"Mismatch: ipv6 (is IPv4)", "ipv6", baseEntry, TrackingKey{}},
		{"Mismatch: Unknown MatchKey", "bad_key", baseEntry, TrackingKey{}},
		{"Mismatch: ipv4_ua (is IPv6)", "ipv4_ua", &LogEntry{IPInfo: NewIPInfo("2001:db8::1")}, TrackingKey{}},
		{"Mismatch: ipv6_ua (is IPv4)", "ipv6_ua", baseEntry, TrackingKey{}},
		{"Mismatch: Malformed IP", "ip", &LogEntry{IPInfo: NewIPInfo("1.2.3.4.5")}, TrackingKey{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chain := &BehavioralChain{MatchKey: tt.matchKey}
			result := GetTrackingKey(chain, tt.entry)

			if result != tt.expectedKey {
				t.Errorf("GetTrackingKey() got key %+v, want %+v", result, tt.expectedKey)
			}
		})
	}
}

func TestIsIPWhitelistedInList(t *testing.T) {
	// Setup a sample whitelist (IPv4 and IPv6)
	_, netV4, _ := net.ParseCIDR("192.168.0.0/24")
	_, netV6, _ := net.ParseCIDR("2001:db8::/32")
	whitelist := []*net.IPNet{netV4, netV6}

	tests := []struct {
		name     string
		ip       string
		list     []*net.IPNet
		expected bool
	}{
		// --- Success Cases ---
		{"IPv4: In Range", "192.168.0.10", whitelist, true},
		{"IPv6: In Range", "2001:db8::1", whitelist, true},
		// --- Failure Cases (Not in range) ---
		{"IPv4: Out of Range", "192.168.1.1", whitelist, false},
		{"IPv6: Out of Range", "2001:db9::1", whitelist, false},
		// --- Edge Cases ---
		{"Invalid IP", "invalid-ip", whitelist, false},
		{"Empty List", "192.168.0.10", []*net.IPNet{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsIPWhitelistedInList(NewIPInfo(tt.ip), tt.list)

			if result != tt.expected {
				t.Errorf("IsIPWhitelistedInList(%s) got %t, want %t", tt.ip, result, tt.expected)
			}
		})
	}
}

// TestGetIPVersion_Malformed covers the edge case where a string is not a valid
// IP but is parsed by net.ParseIP into a non-standard byte slice.
func TestGetIPVersion_Malformed(t *testing.T) {
	// This specific string is known to be parsed by net.ParseIP into a non-nil, 5-byte slice,
	// which fails both ip.To4() and len(ip) == net.IPv6len checks, hitting the final return path.
	version := GetIPVersion("1.2.3.4.5")
	if version != VersionInvalid {
		t.Errorf("Expected VersionInvalid for malformed IP string '1.2.3.4.5', but got version %d", version)
	}
}
