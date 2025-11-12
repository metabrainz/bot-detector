package main

import (
	"net"
	"strings"
)

// IPInfo holds both the string representation of an IP and its parsed version.
type IPInfo struct {
	Address string
	Version IPVersion
}

const (
	// VersionInvalid is 0, the default value for a byte, simplifying initialization checks.
	VersionInvalid IPVersion = 0
	VersionIPv4    IPVersion = 4
	VersionIPv6    IPVersion = 6
)

// NewIPInfo creates a new IPInfo struct, calculating the IP version automatically.
func NewIPInfo(ipStr string) IPInfo {
	return IPInfo{
		Address: ipStr,
		Version: GetIPVersion(ipStr),
	}
}

// GetIPVersion returns the version of the IP address string (IPVersion byte).
func GetIPVersion(ipStr string) IPVersion {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return VersionInvalid
	}
	if ip.To4() != nil {
		return VersionIPv4
	}
	if len(ip) == net.IPv6len {
		return VersionIPv6
	}
	return VersionInvalid
}

// GetActor creates a Actor for a given log entry and chain configuration.
// It respects the chain's MatchKey to decide whether to include the User Agent and to validate the IP version.
func GetActor(chain *BehavioralChain, entry *LogEntry) Actor {
	ipInfo := entry.IPInfo

	// NOTE: We now compare against the byte constants (4 and 6) instead of strings.
	switch chain.MatchKey {
	case "ip", "ip_ua":
		if ipInfo.Version == VersionInvalid {
			return Actor{} // Mismatch: return empty key
		}
	case "ipv4", "ipv4_ua":
		if ipInfo.Version != VersionIPv4 { // Changed from string to byte constant
			return Actor{}
		}
	case "ipv6", "ipv6_ua":
		if ipInfo.Version != VersionIPv6 { // Changed from string to byte constant
			return Actor{}
		}
	default:
		return Actor{} // Unknown match key, treat as mismatch
	}

	actor := Actor{IPInfo: ipInfo}
	if strings.HasSuffix(chain.MatchKey, "_ua") {
		actor.UA = entry.UserAgent
	}

	return actor
}
