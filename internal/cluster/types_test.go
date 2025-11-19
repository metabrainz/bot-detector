package cluster

import (
	"testing"
	"time"
)

func TestNodeConfig_Validate(t *testing.T) {
	tests := []struct {
		name      string
		config    NodeConfig
		expectErr bool
	}{
		{
			name:      "valid node config",
			config:    NodeConfig{Name: "node-1", Address: "localhost:8080"},
			expectErr: false,
		},
		{
			name:      "missing name",
			config:    NodeConfig{Name: "", Address: "localhost:8080"},
			expectErr: true,
		},
		{
			name:      "missing address",
			config:    NodeConfig{Name: "node-1", Address: ""},
			expectErr: true,
		},
		{
			name:      "both missing",
			config:    NodeConfig{Name: "", Address: ""},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.expectErr && err == nil {
				t.Errorf("Expected error, got nil")
			}
			if !tt.expectErr && err != nil {
				t.Errorf("Expected no error, got: %v", err)
			}
		})
	}
}

func TestClusterConfig_Validate(t *testing.T) {
	tests := []struct {
		name      string
		config    ClusterConfig
		expectErr bool
		errMsg    string
	}{
		{
			name: "valid cluster config",
			config: ClusterConfig{
				Nodes: []NodeConfig{
					{Name: "node-1", Address: "localhost:8080"},
					{Name: "node-2", Address: "localhost:9090"},
				},
				ConfigPollInterval:    30 * time.Second,
				MetricsReportInterval: 10 * time.Second,
				Protocol:              "http",
			},
			expectErr: false,
		},
		{
			name: "no nodes (valid when BOT_DETECTOR_NODES will be used)",
			config: ClusterConfig{
				Nodes:                 []NodeConfig{},
				ConfigPollInterval:    30 * time.Second,
				MetricsReportInterval: 10 * time.Second,
				Protocol:              "http",
			},
			expectErr: false,
		},
		{
			name: "zero config poll interval",
			config: ClusterConfig{
				Nodes: []NodeConfig{
					{Name: "node-1", Address: "localhost:8080"},
				},
				ConfigPollInterval:    0,
				MetricsReportInterval: 10 * time.Second,
				Protocol:              "http",
			},
			expectErr: true,
			errMsg:    "must be greater than zero",
		},
		{
			name: "zero metrics interval",
			config: ClusterConfig{
				Nodes: []NodeConfig{
					{Name: "node-1", Address: "localhost:8080"},
				},
				ConfigPollInterval:    30 * time.Second,
				MetricsReportInterval: 0,
				Protocol:              "http",
			},
			expectErr: true,
			errMsg:    "must be greater than zero",
		},
		{
			name: "invalid http protocol",
			config: ClusterConfig{
				Nodes: []NodeConfig{
					{Name: "node-1", Address: "localhost:8080"},
				},
				ConfigPollInterval:    30 * time.Second,
				MetricsReportInterval: 10 * time.Second,
				Protocol:              "ftp",
			},
			expectErr: true,
			errMsg:    "protocol must be",
		},
		{
			name: "duplicate node names",
			config: ClusterConfig{
				Nodes: []NodeConfig{
					{Name: "node-1", Address: "localhost:8080"},
					{Name: "node-1", Address: "localhost:9090"},
				},
				ConfigPollInterval:    30 * time.Second,
				MetricsReportInterval: 10 * time.Second,
				Protocol:              "http",
			},
			expectErr: true,
			errMsg:    "duplicate node name",
		},
		{
			name: "duplicate node addresses",
			config: ClusterConfig{
				Nodes: []NodeConfig{
					{Name: "node-1", Address: "localhost:8080"},
					{Name: "node-2", Address: "localhost:8080"},
				},
				ConfigPollInterval:    30 * time.Second,
				MetricsReportInterval: 10 * time.Second,
				Protocol:              "http",
			},
			expectErr: true,
			errMsg:    "duplicate node address",
		},
		{
			name: "invalid node config",
			config: ClusterConfig{
				Nodes: []NodeConfig{
					{Name: "node-1", Address: "localhost:8080"},
					{Name: "", Address: "localhost:9090"},
				},
				ConfigPollInterval:    30 * time.Second,
				MetricsReportInterval: 10 * time.Second,
				Protocol:              "http",
			},
			expectErr: true,
			errMsg:    "node name cannot be empty",
		},
		{
			name: "https protocol",
			config: ClusterConfig{
				Nodes: []NodeConfig{
					{Name: "node-1", Address: "localhost:8080"},
				},
				ConfigPollInterval:    30 * time.Second,
				MetricsReportInterval: 10 * time.Second,
				Protocol:              "https",
			},
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.expectErr {
				if err == nil {
					t.Errorf("Expected error containing %q, got nil", tt.errMsg)
				} else if tt.errMsg != "" && !contains(err.Error(), tt.errMsg) {
					t.Errorf("Expected error containing %q, got: %v", tt.errMsg, err)
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error, got: %v", err)
				}
			}
		})
	}
}

func TestClusterConfig_FindNodeByAddress(t *testing.T) {
	config := ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "node-1", Address: "localhost:8080"},
			{Name: "node-2", Address: "localhost:9090"},
			{Name: "node-3", Address: "192.168.1.10:8080"},
		},
	}

	tests := []struct {
		address     string
		expectFound bool
		expectName  string
	}{
		{"localhost:8080", true, "node-1"},
		{"localhost:9090", true, "node-2"},
		{"192.168.1.10:8080", true, "node-3"},
		{"localhost:7070", false, ""},
		{"unknown:8080", false, ""},
		{"", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			node, found := config.FindNodeByAddress(tt.address)
			if found != tt.expectFound {
				t.Errorf("FindNodeByAddress(%q): expected found=%v, got found=%v", tt.address, tt.expectFound, found)
			}
			if found && node.Name != tt.expectName {
				t.Errorf("FindNodeByAddress(%q): expected name=%q, got name=%q", tt.address, tt.expectName, node.Name)
			}
		})
	}
}

func TestClusterConfig_FindNodeByName(t *testing.T) {
	config := ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "node-1", Address: "localhost:8080"},
			{Name: "node-2", Address: "localhost:9090"},
			{Name: "node-3", Address: "192.168.1.10:8080"},
		},
	}

	tests := []struct {
		name          string
		expectFound   bool
		expectAddress string
	}{
		{"node-1", true, "localhost:8080"},
		{"node-2", true, "localhost:9090"},
		{"node-3", true, "192.168.1.10:8080"},
		{"node-4", false, ""},
		{"unknown", false, ""},
		{"", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node, found := config.FindNodeByName(tt.name)
			if found != tt.expectFound {
				t.Errorf("FindNodeByName(%q): expected found=%v, got found=%v", tt.name, tt.expectFound, found)
			}
			if found && node.Address != tt.expectAddress {
				t.Errorf("FindNodeByName(%q): expected address=%q, got address=%q", tt.name, tt.expectAddress, node.Address)
			}
		})
	}
}

func TestNodeRole_String(t *testing.T) {
	tests := []struct {
		role     NodeRole
		expected string
	}{
		{RoleLeader, "leader"},
		{RoleFollower, "follower"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			got := tt.role.String()
			if got != tt.expected {
				t.Errorf("NodeRole.String() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
