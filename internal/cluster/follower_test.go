package cluster

import (
	"testing"
)

func TestEnsureURIScheme(t *testing.T) {
	testCases := []struct {
		name     string
		address  string
		protocol string
		expected string
	}{
		{
			name:     "No scheme, http protocol",
			address:  "localhost:8080",
			protocol: "http",
			expected: "http://localhost:8080",
		},
		{
			name:     "No scheme, https protocol",
			address:  "localhost:8080",
			protocol: "https",
			expected: "https://localhost:8080",
		},
		{
			name:     "HTTP scheme already present",
			address:  "http://localhost:8080",
			protocol: "https", // Should not matter if scheme is present
			expected: "http://localhost:8080",
		},
		{
			name:     "HTTPS scheme already present",
			address:  "https://localhost:8080",
			protocol: "http", // Should not matter if scheme is present
			expected: "https://localhost:8080",
		},
		{
			name:     "FTP scheme already present",
			address:  "ftp://localhost:8080",
			protocol: "http",
			expected: "ftp://localhost:8080",
		},
		{
			name:     "Empty string, http protocol",
			address:  "",
			protocol: "http",
			expected: "http://",
		},
		{
			name:     "Empty string, https protocol",
			address:  "",
			protocol: "https",
			expected: "https://",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := ensureURIScheme(tc.address, tc.protocol)
			if actual != tc.expected {
				t.Errorf("For address '%s' with protocol '%s', expected '%s' but got '%s'", tc.address, tc.protocol, tc.expected, actual)
			}
		})
	}
}
