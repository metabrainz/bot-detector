package main

import (
	"net"
	"strings"
)

// --- IP VERSION CONSTANTS ---
const (
	VersionInvalid = "invalid"
	VersionIPv4    = "ipv4"
	VersionIPv6    = "ipv6"
)

// GetIPVersion returns the version of the IP address string ("ipv4", "ipv6", or "invalid").
func GetIPVersion(ipStr string) string {
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

// GetTrackingKey generates the unique state-tracking key based on the chain's configuration.
func GetTrackingKey(chain *BehavioralChain, entry *LogEntry) TrackingKey {
	ipVersion := GetIPVersion(entry.IP)
	trackingKey := TrackingKey{IP: entry.IP}

	switch chain.MatchKey {
	case "ip", "ip_ua":
		if ipVersion == VersionInvalid {
			return TrackingKey{}
		}
	case "ipv4", "ipv4_ua":
		if ipVersion != VersionIPv4 {
			return TrackingKey{}
		}
	case "ipv6", "ipv6_ua":
		if ipVersion != VersionIPv6 {
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