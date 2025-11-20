package cluster

import (
	"testing"
)

func TestEnsureURIScheme(t *testing.T) {
	testCases := []struct {
		name     string
		address  string
		expected string
	}{
		{
			name:     "No scheme",
			address:  "localhost:8080",
			expected: "http://localhost:8080",
		},
		{
			name:     "HTTP scheme already present",
			address:  "http://localhost:8080",
			expected: "http://localhost:8080",
		},
		{
			name:     "HTTPS scheme already present",
			address:  "https://localhost:8080",
			expected: "https://localhost:8080",
		},
		{
			name:     "FTP scheme already present",
			address:  "ftp://localhost:8080",
			expected: "ftp://localhost:8080",
		},
		{
			name:     "Empty string",
			address:  "",
			expected: "http://",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := ensureURIScheme(tc.address)
			if actual != tc.expected {
				t.Errorf("For address '%s', expected '%s' but got '%s'", tc.address, tc.expected, actual)
			}
		})
	}
}
