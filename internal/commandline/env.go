package commandline

import (
	"fmt"
	"os"
	"strings"

	"bot-detector/internal/cluster"
)

// EnvParameters holds configuration values parsed from environment variables.
type EnvParameters struct {
	ClusterNodes []cluster.NodeConfig
}

// ParseEnv reads environment variables and returns parsed configuration.
// It returns an error if environment variable parsing fails.
func ParseEnv() (*EnvParameters, error) {
	params := &EnvParameters{}

	// Parse BOT_DETECTOR_NODES
	nodesEnv := os.Getenv("BOT_DETECTOR_NODES")
	if nodesEnv != "" {
		nodes, err := parseClusterNodes(nodesEnv)
		if err != nil {
			return nil, fmt.Errorf("failed to parse BOT_DETECTOR_NODES: %w", err)
		}
		params.ClusterNodes = nodes
	}

	return params, nil
}

// parseClusterNodes parses the BOT_DETECTOR_NODES format:
// "nodename1:address1;nodename2:address2;..."
//
// The format uses semicolon (;) as the node separator and splits each entry
// on the FIRST colon to separate the node name from its address. This allows
// addresses to contain additional colons (e.g., for ports or IPv6).
//
// Examples:
//   - "leader:http://localhost:8080;follower:http://localhost:9090"
//   - "leader:http://[2001:db8::1]:8080;follower:http://[2001:db8::2]:9090"
func parseClusterNodes(nodesStr string) ([]cluster.NodeConfig, error) {
	var nodes []cluster.NodeConfig

	// Split by semicolon to get individual node entries
	entries := strings.Split(nodesStr, ";")

	for i, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			// Skip empty entries (e.g., trailing semicolons)
			continue
		}

		// Split on FIRST colon to separate name from address
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid node entry at position %d: '%s' (expected 'name:address')", i, entry)
		}

		name := strings.TrimSpace(parts[0])
		address := strings.TrimSpace(parts[1])

		if name == "" {
			return nil, fmt.Errorf("empty node name at position %d", i)
		}
		if address == "" {
			return nil, fmt.Errorf("empty address for node '%s' at position %d", name, i)
		}

		nodes = append(nodes, cluster.NodeConfig{
			Name:    name,
			Address: address,
		})
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("no valid nodes parsed from BOT_DETECTOR_NODES")
	}

	return nodes, nil
}
