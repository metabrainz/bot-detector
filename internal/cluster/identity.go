package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"bot-detector/internal/logging"
)

// NodeIdentity represents the determined identity and role of this node.
type NodeIdentity struct {
	// Role indicates whether this node is a leader or follower.
	Role NodeRole

	// Name is the node's name from the cluster configuration.
	// Empty if cluster is not configured or node is not in the cluster list.
	Name string

	// Address is the node's address from the cluster configuration.
	// Empty if cluster is not configured or node is not in the cluster list.
	Address string

	// LeaderAddress is the address of the leader node (only set for followers).
	// Format: "hostname:port" as read from the FOLLOW file.
	LeaderAddress string
}

// DetermineIdentity determines the node's identity based on the FOLLOW file,
// listen addresses, and cluster configuration.
//
// Parameters:
//   - configDir: Directory containing config.yaml (and potentially FOLLOW file)
//   - listenAddrs: Slice of listen addresses (e.g., [":8080", ":9090"])
//   - clusterNodeName: Explicit node name from command line (e.g., "node-1")
//   - clusterCfg: The cluster configuration from the YAML file (can be nil)
//
// Returns the NodeIdentity or an error if configuration is invalid.
func DetermineIdentity(configDir string, listenAddrs []string, clusterNodeName string, clusterCfg *ClusterConfig) (*NodeIdentity, error) {
	identity := &NodeIdentity{}

	// Determine role based on FOLLOW file existence
	followPath := filepath.Join(configDir, "FOLLOW")
	followData, err := os.ReadFile(followPath)
	if err == nil {
		// FOLLOW file exists - this is a follower
		followContent := strings.TrimSpace(string(followData))
		if followContent == "" {
			return nil, fmt.Errorf("FOLLOW file exists but is empty")
		}
		identity.Role = RoleFollower

		// Detect if FOLLOW content is a URL/host:port or a node name
		if isURLOrHostPort(followContent) {
			// Use directly as leader address (backward compatible)
			identity.LeaderAddress = followContent
		} else {
			// Treat as node name - resolve from cluster config
			if clusterCfg == nil || len(clusterCfg.Nodes) == 0 {
				return nil, fmt.Errorf(
					"FOLLOW file contains node name '%s', but no cluster configuration available",
					followContent,
				)
			}

			// Search for node by name
			leaderNode, found := clusterCfg.FindNodeByName(followContent)
			if !found {
				return nil, fmt.Errorf(
					"FOLLOW file contains node name '%s', but no such node found in cluster configuration",
					followContent,
				)
			}

			identity.LeaderAddress = leaderNode.Address
			logging.LogOutput(logging.LevelInfo, "CLUSTER",
				"Resolved leader name '%s' to address '%s'",
				followContent, leaderNode.Address)
		}
	} else if os.IsNotExist(err) {
		// FOLLOW file doesn't exist - this is a leader
		identity.Role = RoleLeader
	} else {
		// Error reading FOLLOW file
		return nil, fmt.Errorf("failed to read FOLLOW file: %w", err)
	}

	// If no cluster configuration, we're done (single-node mode)
	if clusterCfg == nil {
		return identity, nil
	}

	var found bool
	var node NodeConfig

	// Try to find this node in the cluster configuration
	if clusterNodeName != "" {
		// Primary method: explicit name from --cluster-node-name
		node, found = clusterCfg.FindNodeByName(clusterNodeName)
		if !found {
			return nil, fmt.Errorf("node '%s' provided via --cluster-node-name not found in cluster configuration", clusterNodeName)
		}
	} else {
		// Fallback method: for backward compatibility, match by listen addresses
		// Try each listen address until we find a match
		for _, addr := range listenAddrs {
			node, found = matchNodeByListenAddress(addr, clusterCfg)
			if found {
				break
			}
		}
	}

	if found {
		identity.Name = node.Name
		identity.Address = node.Address
	}

	// For followers, verify that the leader address makes sense
	if identity.Role == RoleFollower && identity.LeaderAddress != "" {
		// Check if the leader address is in the cluster nodes list
		_, _ = clusterCfg.FindNodeByAddress(identity.LeaderAddress)
		// Note: We don't treat it as an error if the leader is not in the nodes list.
		// The leader might use a different address format or might not be in the list.
		// The nodes list is primarily for the leader to know which followers to poll.
	}

	return identity, nil
}

// matchNodeByListenAddress attempts to match the listen address against cluster nodes.
// It handles various address formats like ":8080", "0.0.0.0:8080", or full hostnames.
func matchNodeByListenAddress(listenAddr string, clusterCfg *ClusterConfig) (NodeConfig, bool) {
	// Extract the port from the listen address
	port := extractPort(listenAddr)
	if port == "" {
		return NodeConfig{}, false
	}

	// Try exact match first
	for _, node := range clusterCfg.Nodes {
		if node.Address == listenAddr {
			return node, true
		}
	}

	// Try matching by port if listen address is a bare port like ":8080"
	if strings.HasPrefix(listenAddr, ":") || strings.HasPrefix(listenAddr, "0.0.0.0:") {
		for _, node := range clusterCfg.Nodes {
			nodePort := extractPort(node.Address)
			if nodePort != "" && nodePort == port {
				return node, true
			}
		}
	}

	return NodeConfig{}, false
}

// extractPort extracts the port portion from an address.
// Examples:
//   - ":8080" -> "8080"
//   - "localhost:8080" -> "8080"
//   - "192.168.1.10:8080" -> "8080"
func extractPort(address string) string {
	// Find the last colon (to handle IPv6 addresses correctly)
	idx := strings.LastIndex(address, ":")
	if idx == -1 {
		return ""
	}
	return address[idx+1:]
}

// String returns a human-readable representation of the NodeIdentity.
func (i *NodeIdentity) String() string {
	var parts []string

	parts = append(parts, fmt.Sprintf("role=%s", i.Role))

	if i.Name != "" {
		parts = append(parts, fmt.Sprintf("name=%s", i.Name))
	}

	if i.Address != "" {
		parts = append(parts, fmt.Sprintf("address=%s", i.Address))
	}

	if i.LeaderAddress != "" {
		parts = append(parts, fmt.Sprintf("leader=%s", i.LeaderAddress))
	}

	return strings.Join(parts, ", ")
}

// isURLOrHostPort detects if a string is a URL or host:port format.
// Returns true if the string appears to be a URL (contains "://") or
// a host:port address (contains ":" followed by a numeric port).
// Returns false if it appears to be a simple node name.
func isURLOrHostPort(s string) bool {
	// URL with scheme (e.g., http://localhost:8080)
	if strings.Contains(s, "://") {
		return true
	}

	// IPv6 addresses have multiple colons - treat as address if it starts with [
	if strings.HasPrefix(s, "[") {
		return true
	}

	// Check for simple host:port pattern
	// Split on colon and check if we have exactly 2 parts where the second is numeric
	parts := strings.Split(s, ":")
	if len(parts) == 2 {
		// Check if second part looks like a port number
		if _, err := strconv.Atoi(parts[1]); err == nil {
			return true
		}
	}

	// Otherwise, assume it's a node name
	return false
}
