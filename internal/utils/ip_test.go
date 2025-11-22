package utils

import (
	"testing"
)

func TestNewIPInfo(t *testing.T) {
	tests := []struct {
		name            string
		ipStr           string
		expectedAddress string
		expectedVersion IPVersion
	}{
		{
			name:            "Valid IPv4",
			ipStr:           "192.0.2.1",
			expectedAddress: "192.0.2.1",
			expectedVersion: VersionIPv4,
		},
		{
			name:            "Valid IPv6",
			ipStr:           "2001:db8::1",
			expectedAddress: "2001:db8::1",
			expectedVersion: VersionIPv6,
		},
		{
			name:            "IPv6 with leading zeros (canonicalized)",
			ipStr:           "2001:0db8::1",
			expectedAddress: "2001:db8::1",
			expectedVersion: VersionIPv6,
		},
		{
			name:            "IPv6 full form (canonicalized to compressed)",
			ipStr:           "2001:0db8:0000:0000:0000:0000:0000:0001",
			expectedAddress: "2001:db8::1",
			expectedVersion: VersionIPv6,
		},
		{
			name:            "IPv6 localhost (canonicalized)",
			ipStr:           "0000:0000:0000:0000:0000:0000:0000:0001",
			expectedAddress: "::1",
			expectedVersion: VersionIPv6,
		},
		{
			name:            "Invalid IP String",
			ipStr:           "not-an-ip",
			expectedAddress: "not-an-ip",
			expectedVersion: VersionInvalid,
		},
		{
			name:            "Malformed IP (5 octets)",
			ipStr:           "1.2.3.4.5",
			expectedAddress: "1.2.3.4.5",
			expectedVersion: VersionInvalid,
		},
		{
			name:            "Malformed IPv4-mapped IPv6",
			ipStr:           "::ffff:1.2.3",
			expectedAddress: "::ffff:1.2.3",
			expectedVersion: VersionInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NewIPInfo(tt.ipStr)
			if result.Address != tt.expectedAddress || result.Version != tt.expectedVersion {
				t.Errorf("NewIPInfo(%q) = {Address: %s, Version: %d}, want {Address: %s, Version: %d}",
					tt.ipStr, result.Address, result.Version, tt.expectedAddress, tt.expectedVersion)
			}
		})
	}
}
