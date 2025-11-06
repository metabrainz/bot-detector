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

// GetTrackingKey creates a TrackingKey for a given log entry and chain configuration.
// It respects the chain's MatchKey to decide whether to include the User Agent and to validate the IP version.
func GetTrackingKey(chain *BehavioralChain, entry *LogEntry) TrackingKey {
	ipInfo := entry.IPInfo

	// NOTE: We now compare against the byte constants (4 and 6) instead of strings.
	switch chain.MatchKey {
	case "ip", "ip_ua":
		if ipInfo.Version == VersionInvalid {
			return TrackingKey{} // Mismatch: return empty key
		}
	case "ipv4", "ipv4_ua":
		if ipInfo.Version != VersionIPv4 { // Changed from string to byte constant
			return TrackingKey{}
		}
	case "ipv6", "ipv6_ua":
		if ipInfo.Version != VersionIPv6 { // Changed from string to byte constant
			return TrackingKey{}
		}
	default:
		return TrackingKey{} // Unknown match key, treat as mismatch
	}

	trackingKey := TrackingKey{IPInfo: ipInfo}
	if strings.HasSuffix(chain.MatchKey, "_ua") {
		trackingKey.UA = entry.UserAgent
	}

	return trackingKey
}

// IsIPWhitelisted checks if the given IP address falls within any configured CIDR whitelist range.
func IsIPWhitelisted(ipInfo IPInfo) bool {
	ip := net.ParseIP(ipInfo.Address)
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

// IsIPWhitelistedInList checks if an IP is in the provided list of CIDR networks.
func IsIPWhitelistedInList(ipInfo IPInfo, whitelist []*net.IPNet) bool {
	ip := net.ParseIP(ipInfo.Address)
	if ip == nil {
		return false
	}
	for _, net := range whitelist {
		if net.Contains(ip) {
			return true
		}
	}
	return false
}
