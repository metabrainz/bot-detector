package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// listen address, and cluster configuration.
//
// Parameters:
//   - configDir: Directory containing config.yaml (and potentially FOLLOW file)
//   - listenAddr: The HTTP server listen address (e.g., ":8080" or "192.168.1.10:8080")
//   - clusterCfg: The cluster configuration from the YAML file (can be nil)
//
// Returns the NodeIdentity or an error if configuration is invalid.
func DetermineIdentity(configDir, listenAddr string, clusterCfg *ClusterConfig) (*NodeIdentity, error) {
	identity := &NodeIdentity{}

	// Determine role based on FOLLOW file existence
	followPath := filepath.Join(configDir, "FOLLOW")
	followData, err := os.ReadFile(followPath)
	if err == nil {
		// FOLLOW file exists - this is a follower
		leaderAddr := strings.TrimSpace(string(followData))
		if leaderAddr == "" {
			return nil, fmt.Errorf("FOLLOW file exists but is empty")
		}
		identity.Role = RoleFollower
		identity.LeaderAddress = leaderAddr
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

	// Try to find this node in the cluster configuration
	node, found := matchNodeByListenAddress(listenAddr, clusterCfg)
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
