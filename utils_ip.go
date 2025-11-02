package main

import (
	"net"
	"strings"
)

// IPVersion represents the version of an IP address (0=invalid, 4=IPv4, 6=IPv6).
type IPVersion byte

const (
	// VersionInvalid is 0, the default value for a byte, simplifying initialization checks.
	VersionInvalid IPVersion = 0
	VersionIPv4    IPVersion = 4
	VersionIPv6    IPVersion = 6
)

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

// GetTrackingKey (Update to use the new type in the switch statement)
func GetTrackingKey(chain *BehavioralChain, entry *LogEntry) TrackingKey {
	// The IP version is now available directly on the LogEntry
	ipVersion := entry.IPVersion
	trackingKey := TrackingKey{IP: entry.IP}

	// NOTE: We now compare against the byte constants (4 and 6) instead of strings.
	switch chain.MatchKey {
	case "ip", "ip_ua":
		if ipVersion == VersionInvalid {
			return TrackingKey{}
		}
	case "ipv4", "ipv4_ua":
		if ipVersion != VersionIPv4 { // Changed from string to byte constant
			return TrackingKey{}
		}
	case "ipv6", "ipv6_ua":
		if ipVersion != VersionIPv6 { // Changed from string to byte constant
			return TrackingKey{}
		}
	default:
		return TrackingKey{}
	}

	if strings.HasSuffix(chain.MatchKey, "_ua") {
		trackingKey.UA = entry.UserAgent
	}

	return trackingKey
}

// IsIPWhitelisted checks if the given IP address falls within any configured CIDR whitelist range.
func IsIPWhitelisted(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	// Note: WhitelistNets is protected by ChainMutex because it's populated during config reload.
	ChainMutex.RLock()
	defer ChainMutex.RUnlock()

	for _, ipNet := range WhitelistNets {
		if ipNet.Contains(ip) {
			return true
		}
	}
	return false
}
