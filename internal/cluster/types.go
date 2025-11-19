// Package cluster provides types and functionality for bot-detector's
// leader/follower cluster architecture, enabling configuration synchronization
// and metrics aggregation across multiple instances.
package cluster

import (
	"fmt"
	"time"
)

// NodeRole represents the role of a node in the cluster.
type NodeRole int

const (
	// RoleFollower indicates the node polls a leader for configuration updates.
	RoleFollower NodeRole = iota
	// RoleLeader indicates the node serves configuration and aggregates metrics.
	RoleLeader
)

// String returns a human-readable representation of the NodeRole.
func (r NodeRole) String() string {
	switch r {
	case RoleFollower:
		return "follower"
	case RoleLeader:
		return "leader"
	default:
		return "unknown"
	}
}

// NodeConfig represents a single node in the cluster.
type NodeConfig struct {
	// Name is the unique identifier for this node (e.g., "node-1", "node-2").
	Name string

	// Address is the HTTP listen address for this node (e.g., "node-1.internal:8080").
	Address string
}

// Validate checks if the NodeConfig is properly configured.
func (n *NodeConfig) Validate() error {
	if n.Name == "" {
		return fmt.Errorf("node name cannot be empty")
	}
	if n.Address == "" {
		return fmt.Errorf("node %q: address cannot be empty", n.Name)
	}
	return nil
}

// ClusterConfig contains the configuration for cluster operation.
type ClusterConfig struct {
	// Nodes is the list of all nodes in the cluster.
	Nodes []NodeConfig

	// ConfigPollInterval is how often followers check for configuration updates.
	ConfigPollInterval time.Duration

	// MetricsReportInterval is how often the leader polls followers for metrics.
	MetricsReportInterval time.Duration

	// Protocol specifies the protocol to use for cluster communication ("http" or "https").
	Protocol string
}

// Validate checks if the ClusterConfig is properly configured.
func (c *ClusterConfig) Validate() error {
	// Note: Empty nodes list is allowed when BOT_DETECTOR_NODES environment
	// variable will be used to populate nodes at runtime. Validation of
	// non-empty nodes happens after environment override is applied.

	// Check for duplicate node names or addresses
	names := make(map[string]bool)
	addrs := make(map[string]bool)

	for i, node := range c.Nodes {
		if err := node.Validate(); err != nil {
			return fmt.Errorf("node %d: %w", i, err)
		}

		if names[node.Name] {
			return fmt.Errorf("duplicate node name: %q", node.Name)
		}
		names[node.Name] = true

		if addrs[node.Address] {
			return fmt.Errorf("duplicate node address: %q", node.Address)
		}
		addrs[node.Address] = true
	}

	if c.ConfigPollInterval <= 0 {
		return fmt.Errorf("config_poll_interval must be greater than zero")
	}

	if c.MetricsReportInterval <= 0 {
		return fmt.Errorf("metrics_report_interval must be greater than zero")
	}

	if c.Protocol != "http" && c.Protocol != "https" {
		return fmt.Errorf("protocol must be 'http' or 'https', got %q", c.Protocol)
	}

	return nil
}

// FindNodeByAddress searches for a node by its address.
// Returns the NodeConfig and true if found, or an empty NodeConfig and false if not found.
func (c *ClusterConfig) FindNodeByAddress(address string) (NodeConfig, bool) {
	for _, node := range c.Nodes {
		if node.Address == address {
			return node, true
		}
	}
	return NodeConfig{}, false
}

// FindNodeByName searches for a node by its name.
// Returns the NodeConfig and true if found, or an empty NodeConfig and false if not found.
func (c *ClusterConfig) FindNodeByName(name string) (NodeConfig, bool) {
	for _, node := range c.Nodes {
		if node.Name == name {
			return node, true
		}
	}
	return NodeConfig{}, false
}
