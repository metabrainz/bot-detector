package server

// EndpointRole represents the purpose/category of an HTTP endpoint.
type EndpointRole string

const (
	// RoleAPI represents configuration and IP lookup endpoints.
	RoleAPI EndpointRole = "api"

	// RoleMetrics represents metrics and stats endpoints.
	RoleMetrics EndpointRole = "metrics"

	// RoleCluster represents cluster communication endpoints.
	RoleCluster EndpointRole = "cluster"
)

// ListenConfig is a minimal interface for accessing listen configuration.
// This avoids importing the commandline package.
type ListenConfig interface {
	HasRole(role string) bool
	HasExplicitRoles() bool
	GetAddress() string
	String() string
}

// shouldServeEndpoint determines if a specific listener should serve an endpoint
// based on role-based routing rules.
//
// Rules:
// 1. If no roles are specified on ANY listener -> all listeners serve all endpoints
// 2. If roles are specified on at least one listener:
//   - Listeners WITH roles serve only their specified endpoints
//   - Listeners WITHOUT roles serve endpoints not claimed by role-specific listeners
//
// Parameters:
//   - allConfigs: All configured listeners
//   - currentConfig: The listener being evaluated
//   - endpointRole: The role of the endpoint being checked
//
// Returns true if the listener should serve the endpoint.
func shouldServeEndpoint(allConfigs []ListenConfig, currentConfig ListenConfig, endpointRole EndpointRole) bool {
	// Check if any listener has roles configured
	anyHasRoles := false
	for _, cfg := range allConfigs {
		if cfg.HasExplicitRoles() {
			anyHasRoles = true
			break
		}
	}

	// Rule 1: No roles anywhere -> all serve all
	if !anyHasRoles {
		return true
	}

	// Rule 2: Role-based routing is active
	if currentConfig.HasExplicitRoles() {
		// This listener has roles -> serve only if it matches
		return currentConfig.HasRole(string(endpointRole)) || currentConfig.HasRole("all")
	}

	// This listener has no roles -> serve if endpoint is not claimed by any role-specific listener
	for _, cfg := range allConfigs {
		if cfg.HasExplicitRoles() && (cfg.HasRole(string(endpointRole)) || cfg.HasRole("all")) {
			// Another listener claims this endpoint
			return false
		}
	}

	// No role-specific listener claims this endpoint -> this listener serves it
	return true
}
