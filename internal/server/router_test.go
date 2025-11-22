package server

import "testing"

// mockListenConfig implements the ListenConfig interface for testing.
type mockListenConfig struct {
	roles []string
}

func (m *mockListenConfig) HasRole(role string) bool {
	if len(m.roles) == 0 {
		return true
	}
	for _, r := range m.roles {
		if r == role || r == "all" {
			return true
		}
	}
	return false
}

func (m *mockListenConfig) HasExplicitRoles() bool {
	return len(m.roles) > 0
}

func TestShouldServeEndpoint_NoRolesAnywhere(t *testing.T) {
	// All listeners have no roles -> all serve all endpoints
	configs := []ListenConfig{
		&mockListenConfig{roles: []string{}},
		&mockListenConfig{roles: []string{}},
	}

	for i, cfg := range configs {
		for _, role := range []EndpointRole{RoleAPI, RoleMetrics, RoleCluster} {
			if !shouldServeEndpoint(configs, cfg, role) {
				t.Errorf("Listener %d should serve %s when no roles configured", i, role)
			}
		}
	}
}

func TestShouldServeEndpoint_RoleSpecificListener(t *testing.T) {
	// One listener with role=api should only serve API endpoints
	configs := []ListenConfig{
		&mockListenConfig{roles: []string{"api"}},
	}

	tests := []struct {
		role EndpointRole
		want bool
	}{
		{RoleAPI, true},
		{RoleMetrics, false},
		{RoleCluster, false},
	}

	for _, tt := range tests {
		got := shouldServeEndpoint(configs, configs[0], tt.role)
		if got != tt.want {
			t.Errorf("shouldServeEndpoint(role=%s) = %v, want %v", tt.role, got, tt.want)
		}
	}
}

func TestShouldServeEndpoint_MultipleRolesOnOneListener(t *testing.T) {
	// One listener with role=api+metrics
	configs := []ListenConfig{
		&mockListenConfig{roles: []string{"api", "metrics"}},
	}

	tests := []struct {
		role EndpointRole
		want bool
	}{
		{RoleAPI, true},
		{RoleMetrics, true},
		{RoleCluster, false},
	}

	for _, tt := range tests {
		got := shouldServeEndpoint(configs, configs[0], tt.role)
		if got != tt.want {
			t.Errorf("shouldServeEndpoint(role=%s) = %v, want %v", tt.role, got, tt.want)
		}
	}
}

func TestShouldServeEndpoint_AllRole(t *testing.T) {
	// Listener with role=all should serve everything
	configs := []ListenConfig{
		&mockListenConfig{roles: []string{"all"}},
	}

	for _, role := range []EndpointRole{RoleAPI, RoleMetrics, RoleCluster} {
		if !shouldServeEndpoint(configs, configs[0], role) {
			t.Errorf("Listener with role=all should serve %s", role)
		}
	}
}

func TestShouldServeEndpoint_MixedRoleAndNoRole(t *testing.T) {
	// Listener 0: role=api
	// Listener 1: no roles (should serve metrics and cluster, but not api)
	configs := []ListenConfig{
		&mockListenConfig{roles: []string{"api"}},
		&mockListenConfig{roles: []string{}},
	}

	tests := []struct {
		listenerIdx int
		role        EndpointRole
		want        bool
	}{
		// Listener 0 (role=api)
		{0, RoleAPI, true},
		{0, RoleMetrics, false},
		{0, RoleCluster, false},
		// Listener 1 (no roles)
		{1, RoleAPI, false},    // Claimed by listener 0
		{1, RoleMetrics, true}, // Not claimed
		{1, RoleCluster, true}, // Not claimed
	}

	for _, tt := range tests {
		got := shouldServeEndpoint(configs, configs[tt.listenerIdx], tt.role)
		if got != tt.want {
			t.Errorf("Listener %d, role=%s: got %v, want %v", tt.listenerIdx, tt.role, got, tt.want)
		}
	}
}

func TestShouldServeEndpoint_MultipleRoleSpecificListeners(t *testing.T) {
	// Listener 0: role=api
	// Listener 1: role=metrics
	// Listener 2: role=cluster
	configs := []ListenConfig{
		&mockListenConfig{roles: []string{"api"}},
		&mockListenConfig{roles: []string{"metrics"}},
		&mockListenConfig{roles: []string{"cluster"}},
	}

	tests := []struct {
		listenerIdx int
		role        EndpointRole
		want        bool
	}{
		{0, RoleAPI, true},
		{0, RoleMetrics, false},
		{0, RoleCluster, false},
		{1, RoleAPI, false},
		{1, RoleMetrics, true},
		{1, RoleCluster, false},
		{2, RoleAPI, false},
		{2, RoleMetrics, false},
		{2, RoleCluster, true},
	}

	for _, tt := range tests {
		got := shouldServeEndpoint(configs, configs[tt.listenerIdx], tt.role)
		if got != tt.want {
			t.Errorf("Listener %d, role=%s: got %v, want %v", tt.listenerIdx, tt.role, got, tt.want)
		}
	}
}

func TestShouldServeEndpoint_ComplexMix(t *testing.T) {
	// Listener 0: no roles
	// Listener 1: role=api+metrics
	// Listener 2: no roles
	configs := []ListenConfig{
		&mockListenConfig{roles: []string{}},
		&mockListenConfig{roles: []string{"api", "metrics"}},
		&mockListenConfig{roles: []string{}},
	}

	tests := []struct {
		listenerIdx int
		role        EndpointRole
		want        bool
	}{
		// Listeners without roles serve only cluster (not claimed by listener 1)
		{0, RoleAPI, false},
		{0, RoleMetrics, false},
		{0, RoleCluster, true},
		// Listener with roles serves only its roles
		{1, RoleAPI, true},
		{1, RoleMetrics, true},
		{1, RoleCluster, false},
		// Listeners without roles serve only cluster
		{2, RoleAPI, false},
		{2, RoleMetrics, false},
		{2, RoleCluster, true},
	}

	for _, tt := range tests {
		got := shouldServeEndpoint(configs, configs[tt.listenerIdx], tt.role)
		if got != tt.want {
			t.Errorf("Listener %d, role=%s: got %v, want %v", tt.listenerIdx, tt.role, got, tt.want)
		}
	}
}
