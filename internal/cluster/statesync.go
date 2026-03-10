package cluster

import (
	"fmt"
	"strings"
	"time"

	"bot-detector/internal/persistence"
)

const (
	// StateSyncVersion is the current state sync protocol version.
	StateSyncVersion = "v1"

	// ReasonSeparator is used to separate multiple reasons in merged states.
	ReasonSeparator = " | "
)

// StateSyncResponse is the response format for state sync endpoints.
type StateSyncResponse struct {
	Version   string                         `json:"version"`
	Timestamp time.Time                      `json:"timestamp"`
	States    map[string]persistence.IPState `json:"states"`
}

// MergedStateResponse is the response format for merged cluster state.
type MergedStateResponse struct {
	Version      string                         `json:"version"`
	Timestamp    time.Time                      `json:"timestamp"`
	NodesQueried []string                       `json:"nodes_queried"`
	NodesFailed  []string                       `json:"nodes_failed"`
	States       map[string]persistence.IPState `json:"states"`
}

// StateSyncConfig holds configuration for state synchronization.
type StateSyncConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Interval    time.Duration `yaml:"interval"`
	Compression bool          `yaml:"compression"`
	Timeout     time.Duration `yaml:"timeout"`
	Incremental bool          `yaml:"incremental"`
}

// AddSourceNode adds source node attribution to a reason if not already present.
// Format: "reason (nodeName)" or "reason (nodeAddress)" if name is empty.
func AddSourceNode(reason, nodeName, nodeAddress string) string {
	// Check if reason already has source attribution
	if strings.Contains(reason, " (") && strings.HasSuffix(reason, ")") {
		return reason
	}

	source := nodeName
	if source == "" {
		source = nodeAddress
	}

	return fmt.Sprintf("%s (%s)", reason, source)
}

// MergeReasons combines two reasons without duplication.
// Extracts base reasons (without source nodes) and only adds if not already present.
// Uses ReasonSeparator to avoid conflicts with commas in reasons.
func MergeReasons(existing, newReason string) string {
	if existing == "" {
		return newReason
	}
	if newReason == "" {
		return existing
	}

	// Parse existing reasons into map (base reason -> true)
	reasonMap := make(map[string]bool)
	for _, part := range strings.Split(existing, ReasonSeparator) {
		// Extract reason without source node: "Login-Abuse[main_site] (leader)" -> "Login-Abuse[main_site]"
		baseReason := extractBaseReason(strings.TrimSpace(part))
		reasonMap[baseReason] = true
	}

	// Extract new base reason
	newBaseReason := extractBaseReason(newReason)

	// Only add if not already present
	if !reasonMap[newBaseReason] {
		return existing + ReasonSeparator + newReason
	}

	return existing
}

// extractBaseReason extracts the reason without source node attribution.
// "Login-Abuse[main_site] (leader)" -> "Login-Abuse[main_site]"
func extractBaseReason(reason string) string {
	if idx := strings.Index(reason, " ("); idx != -1 {
		return reason[:idx]
	}
	return reason
}
