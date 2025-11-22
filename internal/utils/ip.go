package utils

import "net"

// IPVersion is used internally to track whether an IP is v4 or v6.
type IPVersion byte

const (
	VersionInvalid IPVersion = 0
	VersionIPv4    IPVersion = 4
	VersionIPv6    IPVersion = 6
)

// IPInfo holds the string representation of an IP and its version.
type IPInfo struct {
	Address string
	Version IPVersion
}

// NewIPInfo parses an IP string and returns a structured IPInfo object.
// The IP address is stored in canonical form (e.g., 2001:db8::1 instead of 2001:0db8::1).
func NewIPInfo(ipStr string) IPInfo {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return IPInfo{Address: ipStr, Version: VersionInvalid}
	}

	// Store canonical form
	canonical := ip.String()

	// Check if it's an IPv4 address.
	if ip.To4() != nil {
		return IPInfo{Address: canonical, Version: VersionIPv4}
	}

	// Check if it's an IPv6 address.
	if ip.To16() != nil {
		return IPInfo{Address: canonical, Version: VersionIPv6}
	}

	return IPInfo{Address: ipStr, Version: VersionInvalid}
}
