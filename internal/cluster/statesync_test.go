package cluster

import (
	"testing"
)

func TestAddSourceNode(t *testing.T) {
	tests := []struct {
		name        string
		reason      string
		nodeName    string
		nodeAddress string
		want        string
	}{
		{
			name:        "add node name",
			reason:      "Login-Abuse[main_site]",
			nodeName:    "leader",
			nodeAddress: "192.168.1.1:8080",
			want:        "Login-Abuse[main_site] (leader)",
		},
		{
			name:        "add node address when name empty",
			reason:      "API-Abuse",
			nodeName:    "",
			nodeAddress: "192.168.1.2:8080",
			want:        "API-Abuse (192.168.1.2:8080)",
		},
		{
			name:        "already has source",
			reason:      "Login-Abuse[main_site] (leader)",
			nodeName:    "follower",
			nodeAddress: "192.168.1.2:8080",
			want:        "Login-Abuse[main_site] (leader)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AddSourceNode(tt.reason, tt.nodeName, tt.nodeAddress)
			if got != tt.want {
				t.Errorf("AddSourceNode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMergeReasons(t *testing.T) {
	tests := []struct {
		name      string
		existing  string
		newReason string
		want      string
	}{
		{
			name:      "empty existing",
			existing:  "",
			newReason: "Login-Abuse[main_site] (leader)",
			want:      "Login-Abuse[main_site] (leader)",
		},
		{
			name:      "empty new",
			existing:  "Login-Abuse[main_site] (leader)",
			newReason: "",
			want:      "Login-Abuse[main_site] (leader)",
		},
		{
			name:      "different reasons",
			existing:  "Login-Abuse[main_site] (leader)",
			newReason: "API-Abuse[api_site] (follower-1)",
			want:      "Login-Abuse[main_site] (leader) | API-Abuse[api_site] (follower-1)",
		},
		{
			name:      "duplicate reason - same base",
			existing:  "Login-Abuse[main_site] (leader)",
			newReason: "Login-Abuse[main_site] (follower-1)",
			want:      "Login-Abuse[main_site] (leader)",
		},
		{
			name:      "duplicate in chain",
			existing:  "Login-Abuse[main_site] (leader) | API-Abuse[api_site] (follower-1)",
			newReason: "Login-Abuse[main_site] (follower-2)",
			want:      "Login-Abuse[main_site] (leader) | API-Abuse[api_site] (follower-1)",
		},
		{
			name:      "add third unique reason",
			existing:  "Login-Abuse[main_site] (leader) | API-Abuse[api_site] (follower-1)",
			newReason: "SQL-Injection[admin_site] (follower-2)",
			want:      "Login-Abuse[main_site] (leader) | API-Abuse[api_site] (follower-1) | SQL-Injection[admin_site] (follower-2)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeReasons(tt.existing, tt.newReason)
			if got != tt.want {
				t.Errorf("MergeReasons() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractBaseReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{
			name:   "with source node",
			reason: "Login-Abuse[main_site] (leader)",
			want:   "Login-Abuse[main_site]",
		},
		{
			name:   "without source node",
			reason: "API-Abuse[api_site]",
			want:   "API-Abuse[api_site]",
		},
		{
			name:   "with IP address source",
			reason: "SQL-Injection (192.168.1.1:8080)",
			want:   "SQL-Injection",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractBaseReason(tt.reason)
			if got != tt.want {
				t.Errorf("extractBaseReason() = %q, want %q", got, tt.want)
			}
		})
	}
}
