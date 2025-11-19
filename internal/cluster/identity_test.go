package cluster

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestDetermineIdentity_SingleNodeLeader(t *testing.T) {
	// Single node with no cluster config - should be leader
	// No FOLLOW file = leader
	tmpDir := t.TempDir()

	identity, err := DetermineIdentity(tmpDir, ":8080", "", nil)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if identity.Role != RoleLeader {
		t.Errorf("Expected role to be Leader, got: %v", identity.Role)
	}

	if identity.Name != "" {
		t.Errorf("Expected empty name for single node, got: %q", identity.Name)
	}

	if identity.Address != "" {
		t.Errorf("Expected empty address for single node, got: %q", identity.Address)
	}

	if identity.LeaderAddress != "" {
		t.Errorf("Expected empty leader address for leader, got: %q", identity.LeaderAddress)
	}
}

func TestDetermineIdentity_Follower(t *testing.T) {
	// Node with FOLLOW file - should be follower
	tmpDir := t.TempDir()

	// Create FOLLOW file with leader address
	followPath := tmpDir + "/FOLLOW"
	if err := os.WriteFile(followPath, []byte("leader-node:8080"), 0644); err != nil {
		t.Fatalf("Failed to create FOLLOW file: %v", err)
	}

	identity, err := DetermineIdentity(tmpDir, ":9090", "", nil)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if identity.Role != RoleFollower {
		t.Errorf("Expected role to be Follower, got: %v", identity.Role)
	}

	if identity.LeaderAddress != "leader-node:8080" {
		t.Errorf("Expected leader address to be 'leader-node:8080', got: %q", identity.LeaderAddress)
	}
}

func TestDetermineIdentity_WithClusterConfig_ExactMatch(t *testing.T) {
	tmpDir := t.TempDir() // No FOLLOW file = leader

	clusterCfg := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "node-1", Address: "localhost:8080"},
			{Name: "node-2", Address: "localhost:9090"},
		},
		ConfigPollInterval:    30 * time.Second,
		MetricsReportInterval: 10 * time.Second,
		Protocol:              "http",
	}

	identity, err := DetermineIdentity(tmpDir, "localhost:8080", "", clusterCfg)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if identity.Role != RoleLeader {
		t.Errorf("Expected role to be Leader, got: %v", identity.Role)
	}

	if identity.Name != "node-1" {
		t.Errorf("Expected name to be 'node-1', got: %q", identity.Name)
	}

	if identity.Address != "localhost:8080" {
		t.Errorf("Expected address to be 'localhost:8080', got: %q", identity.Address)
	}
}

func TestDetermineIdentity_WithClusterConfig_PortMatch(t *testing.T) {
	tmpDir := t.TempDir() // No FOLLOW file = leader

	clusterCfg := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "node-1", Address: "node-1.example.com:8080"},
			{Name: "node-2", Address: "node-2.example.com:9090"},
		},
		ConfigPollInterval:    30 * time.Second,
		MetricsReportInterval: 10 * time.Second,
		Protocol:              "http",
	}

	// Listen on :8080 should match node-1 by port
	identity, err := DetermineIdentity(tmpDir, ":8080", "", clusterCfg)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if identity.Role != RoleLeader {
		t.Errorf("Expected role to be Leader, got: %v", identity.Role)
	}

	if identity.Name != "node-1" {
		t.Errorf("Expected name to be 'node-1', got: %q", identity.Name)
	}

	if identity.Address != "node-1.example.com:8080" {
		t.Errorf("Expected address to be 'node-1.example.com:8080', got: %q", identity.Address)
	}
}

func TestDetermineIdentity_WithClusterConfig_0000PortMatch(t *testing.T) {
	tmpDir := t.TempDir() // No FOLLOW file = leader

	clusterCfg := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "node-1", Address: "node-1.example.com:8080"},
		},
		ConfigPollInterval:    30 * time.Second,
		MetricsReportInterval: 10 * time.Second,
		Protocol:              "http",
	}

	// Listen on 0.0.0.0:8080 should match node-1 by port
	identity, err := DetermineIdentity(tmpDir, "0.0.0.0:8080", "", clusterCfg)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if identity.Name != "node-1" {
		t.Errorf("Expected name to be 'node-1', got: %q", identity.Name)
	}

	if identity.Address != "node-1.example.com:8080" {
		t.Errorf("Expected address to be 'node-1.example.com:8080', got: %q", identity.Address)
	}
}

func TestDetermineIdentity_WithClusterNodeName(t *testing.T) {
	tmpDir := t.TempDir() // No FOLLOW file = leader

	clusterCfg := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "node-1", Address: "node-1.example.com:8080"},
			{Name: "node-2", Address: "node-2.example.com:9090"},
		},
	}

	t.Run("it identifies correctly by name", func(t *testing.T) {
		// Even though listen address port matches node-1, the explicit name "node-2" should be used.
		identity, err := DetermineIdentity(tmpDir, ":8080", "node-2", clusterCfg)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}

		if identity.Name != "node-2" {
			t.Errorf("Expected name to be 'node-2', got: %q", identity.Name)
		}
		if identity.Address != "node-2.example.com:9090" {
			t.Errorf("Expected address to be 'node-2.example.com:9090', got: %q", identity.Address)
		}
	})

	t.Run("it returns an error for an unknown name", func(t *testing.T) {
		_, err := DetermineIdentity(tmpDir, ":8080", "unknown-node", clusterCfg)
		if err == nil {
			t.Fatal("Expected an error for unknown node name, got nil")
		}
		expectedErr := "not found in cluster configuration"
		if !strings.Contains(err.Error(), expectedErr) {
			t.Errorf("Expected error message to contain %q, got: %q", expectedErr, err.Error())
		}
	})
}

func TestDetermineIdentity_WithClusterConfig_NoMatch(t *testing.T) {
	tmpDir := t.TempDir() // No FOLLOW file = leader

	clusterCfg := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "node-1", Address: "localhost:8080"},
			{Name: "node-2", Address: "localhost:9090"},
		},
		ConfigPollInterval:    30 * time.Second,
		MetricsReportInterval: 10 * time.Second,
		Protocol:              "http",
	}

	// Listen on :7070 doesn't match any node
	identity, err := DetermineIdentity(tmpDir, ":7070", "", clusterCfg)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if identity.Role != RoleLeader {
		t.Errorf("Expected role to be Leader, got: %v", identity.Role)
	}

	if identity.Name != "" {
		t.Errorf("Expected empty name when no match found, got: %q", identity.Name)
	}

	if identity.Address != "" {
		t.Errorf("Expected empty address when no match found, got: %q", identity.Address)
	}
}

func TestExtractPort(t *testing.T) {
	tests := []struct {
		address      string
		expectedPort string
	}{
		{":8080", "8080"},
		{"localhost:8080", "8080"},
		{"192.168.1.10:8080", "8080"},
		{"0.0.0.0:9090", "9090"},
		{"node-1.example.com:3000", "3000"},
		{"no-port-here", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			got := extractPort(tt.address)
			if got != tt.expectedPort {
				t.Errorf("extractPort(%q) = %q, want %q", tt.address, got, tt.expectedPort)
			}
		})
	}
}

func TestNodeIdentity_String(t *testing.T) {
	tests := []struct {
		name     string
		identity NodeIdentity
		expected string
	}{
		{
			name: "leader with full info",
			identity: NodeIdentity{
				Role:    RoleLeader,
				Name:    "node-1",
				Address: "localhost:8080",
			},
			expected: "role=leader, name=node-1, address=localhost:8080",
		},
		{
			name: "follower with leader address",
			identity: NodeIdentity{
				Role:          RoleFollower,
				Name:          "node-2",
				Address:       "localhost:9090",
				LeaderAddress: "node-1:8080",
			},
			expected: "role=follower, name=node-2, address=localhost:9090, leader=node-1:8080",
		},
		{
			name: "minimal leader",
			identity: NodeIdentity{
				Role: RoleLeader,
			},
			expected: "role=leader",
		},
		{
			name: "minimal follower",
			identity: NodeIdentity{
				Role:          RoleFollower,
				LeaderAddress: "leader:8080",
			},
			expected: "role=follower, leader=leader:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.identity.String()
			if got != tt.expected {
				t.Errorf("NodeIdentity.String() = %q, want %q", got, tt.expected)
			}
		})
	}
}
