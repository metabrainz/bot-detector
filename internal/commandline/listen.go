package commandline

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ListenConfig represents a parsed listen address with optional configuration.
type ListenConfig struct {
	Address  string   // e.g., ":8080", "0.0.0.0:8080", "[::]:8080"
	Roles    []string // e.g., ["api", "metrics"] or empty for all
	Protocol string   // "http" (only supported value for now)
}

// Valid role values
const (
	RoleAPI     = "api"
	RoleMetrics = "metrics"
	RoleCluster = "cluster"
	RoleAll     = "all"
)

var validRoles = map[string]bool{
	RoleAPI:     true,
	RoleMetrics: true,
	RoleCluster: true,
	RoleAll:     true,
}

// ParseListenFlag parses a listen flag value in the format:
// <address>[,key=value,...]
//
// Examples:
//   - ":8080"
//   - "0.0.0.0:8080,role=api"
//   - "[::]:8080,role=api+metrics"
//   - ":8080,role=api,role=metrics,proto=http"
func ParseListenFlag(flag string) (*ListenConfig, error) {
	if flag == "" {
		return nil, fmt.Errorf("listen flag cannot be empty")
	}

	// Split by comma, but need to handle IPv6 addresses like [::]:8080
	parts := splitListenFlag(flag)
	if len(parts) == 0 {
		return nil, fmt.Errorf("invalid listen flag format: %q", flag)
	}

	config := &ListenConfig{
		Address:  parts[0],
		Protocol: "http", // default
		Roles:    []string{},
	}

	// Validate address format
	if err := validateAddress(config.Address); err != nil {
		return nil, fmt.Errorf("invalid address %q: %w", config.Address, err)
	}

	// Parse key=value pairs
	roleMap := make(map[string]bool)
	for _, part := range parts[1:] {
		key, value, err := parseKeyValue(part)
		if err != nil {
			return nil, err
		}

		switch key {
		case "role":
			if value == "" {
				return nil, fmt.Errorf("role cannot be empty (check for unset environment variables)")
			}
			// Support both role=a+b and role=a,role=b
			roles := strings.Split(value, "+")
			for _, role := range roles {
				role = strings.TrimSpace(role)
				if !validRoles[role] {
					return nil, fmt.Errorf("invalid role %q, must be one of: api, metrics, cluster, all", role)
				}
				roleMap[role] = true
			}
		case "proto":
			if value != "http" {
				return nil, fmt.Errorf("only proto=http is supported, got proto=%s", value)
			}
			config.Protocol = value
		default:
			return nil, fmt.Errorf("unknown option %q", key)
		}
	}

	// Convert role map to sorted slice for consistency
	for role := range roleMap {
		config.Roles = append(config.Roles, role)
	}

	return config, nil
}

// splitListenFlag splits a listen flag by comma, handling IPv6 addresses correctly.
// Example: "[::]:8080,role=api" -> ["[::]:8080", "role=api"]
func splitListenFlag(flag string) []string {
	var parts []string
	var current strings.Builder
	inBrackets := false

	for _, ch := range flag {
		switch ch {
		case '[':
			inBrackets = true
			current.WriteRune(ch)
		case ']':
			inBrackets = false
			current.WriteRune(ch)
		case ',':
			if inBrackets {
				current.WriteRune(ch)
			} else {
				if current.Len() > 0 {
					parts = append(parts, current.String())
					current.Reset()
				}
			}
		default:
			current.WriteRune(ch)
		}
	}

	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	return parts
}

// parseKeyValue parses a "key=value" string.
func parseKeyValue(s string) (key, value string, err error) {
	idx := strings.Index(s, "=")
	if idx == -1 {
		return "", "", fmt.Errorf("invalid key=value format: %q", s)
	}
	key = strings.TrimSpace(s[:idx])
	value = strings.TrimSpace(s[idx+1:])
	if key == "" {
		return "", "", fmt.Errorf("empty key in %q", s)
	}
	return key, value, nil
}

// validateAddress validates that the address is in a valid format.
// Supports: :port, host:port, ipv4:port, [ipv6]:port
func validateAddress(addr string) error {
	// Extract host and port
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address format: %w", err)
	}

	// Validate port
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("invalid port %q: must be numeric", portStr)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %d: must be between 1 and 65535", port)
	}

	// Validate host (if not empty or wildcard)
	if host != "" && host != "0.0.0.0" && host != "::" {
		// Try parsing as IP
		if ip := net.ParseIP(host); ip == nil {
			// Not an IP, could be hostname - basic validation
			if strings.Contains(host, " ") {
				return fmt.Errorf("invalid hostname %q: contains spaces", host)
			}
		}
	}

	return nil
}

// HasRole checks if this listener should serve a specific role.
// If no roles are configured, it serves all roles.
func (lc *ListenConfig) HasRole(role string) bool {
	if len(lc.Roles) == 0 {
		return true // No roles specified = serves all
	}
	for _, r := range lc.Roles {
		if r == role || r == RoleAll {
			return true
		}
	}
	return false
}

// HasExplicitRoles returns true if this listener has explicitly configured roles.
func (lc *ListenConfig) HasExplicitRoles() bool {
	return len(lc.Roles) > 0
}

// GetAddress returns the listen address.
func (lc *ListenConfig) GetAddress() string {
	return lc.Address
}

// String returns a human-readable representation of the ListenConfig.
func (lc *ListenConfig) String() string {
	if len(lc.Roles) == 0 {
		return lc.Address
	}
	return fmt.Sprintf("%s (roles: %s)", lc.Address, strings.Join(lc.Roles, ", "))
}
