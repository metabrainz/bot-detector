package main

import (


	"testing"
)



func TestGetActor(t *testing.T) {
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
		expectedKey Actor
	}{
		// --- Success Cases (Key returned) ---
		// 1. IP-only keys
		{"Match: ip (IPv4)", "ip", baseEntry, Actor{IPInfo: NewIPInfo("192.0.2.1"), UA: ""}},
		{"Match: ip (IPv6)", "ip", &LogEntry{IPInfo: NewIPInfo("2001:db8::1")}, Actor{IPInfo: NewIPInfo("2001:db8::1"), UA: ""}},
		{"Match: ipv4 (IPv4)", "ipv4", baseEntry, Actor{IPInfo: NewIPInfo("192.0.2.1"), UA: ""}},
		{"Match: ipv6 (IPv6)", "ipv6", &LogEntry{IPInfo: NewIPInfo("2001:db8::1")}, Actor{IPInfo: NewIPInfo("2001:db8::1"), UA: ""}},

		// 2. IP+UA keys
		{"Match: ip_ua (IPv4)", "ip_ua", baseEntry, Actor{IPInfo: NewIPInfo("192.0.2.1"), UA: "TestAgent"}},
		{"Match: ipv4_ua (IPv4)", "ipv4_ua", baseEntry, Actor{IPInfo: NewIPInfo("192.0.2.1"), UA: "TestAgent"}},
		{"Match: ipv6_ua (IPv6)", "ipv6_ua", &LogEntry{IPInfo: NewIPInfo("2001:db8::1"), UserAgent: "TestAgent"}, Actor{IPInfo: NewIPInfo("2001:db8::1"), UA: "TestAgent"}},

		// --- Failure Cases (Empty Key is now expected) ---
		{"Mismatch: ip (Invalid Version)", "ip", &LogEntry{IPInfo: NewIPInfo("bad-ip")}, Actor{}},
		{"Mismatch: ipv4 (is IPv6)", "ipv4", &LogEntry{IPInfo: NewIPInfo("2001:db8::1")}, Actor{}},
		{"Mismatch: ipv6 (is IPv4)", "ipv6", baseEntry, Actor{}},
		{"Mismatch: Unknown MatchKey", "bad_key", baseEntry, Actor{}},
		{"Mismatch: ipv4_ua (is IPv6)", "ipv4_ua", &LogEntry{IPInfo: NewIPInfo("2001:db8::1")}, Actor{}},
		{"Mismatch: ipv6_ua (is IPv4)", "ipv6_ua", baseEntry, Actor{}},
		{"Mismatch: Malformed IP", "ip", &LogEntry{IPInfo: NewIPInfo("1.2.3.4.5")}, Actor{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chain := &BehavioralChain{MatchKey: tt.matchKey}
			result := GetActor(chain, tt.entry)

			if result != tt.expectedKey {
				t.Errorf("GetActor() got key %+v, want %+v", result, tt.expectedKey)
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

// TestGetIPVersion_NilButNotIPv4 tests the final return path of GetIPVersion.
func TestGetIPVersion_NilButNotIPv4(t *testing.T) {
	// net.ParseIP can return a non-nil slice that is not a valid IPv4 or IPv6 address.
	// For example, an IPv4-mapped IPv6 address that is not 16 bytes.
	// This test ensures we correctly classify such cases as invalid.
	if GetIPVersion("::ffff:1.2.3") != VersionInvalid {
		t.Error("Expected VersionInvalid for a malformed IPv4-mapped IPv6 address string.")
	}
}
