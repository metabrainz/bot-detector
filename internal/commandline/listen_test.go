package commandline

import (
	"strings"
	"testing"
)

func TestParseListenFlag_ValidFormats(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantAddr  string
		wantRole  []string
		wantProto string
	}{
		{
			name:      "simple port",
			input:     ":8080",
			wantAddr:  ":8080",
			wantRole:  []string{},
			wantProto: "http",
		},
		{
			name:      "ipv4 with port",
			input:     "0.0.0.0:8080",
			wantAddr:  "0.0.0.0:8080",
			wantRole:  []string{},
			wantProto: "http",
		},
		{
			name:      "ipv6 with port",
			input:     "[::]:8080",
			wantAddr:  "[::]:8080",
			wantRole:  []string{},
			wantProto: "http",
		},
		{
			name:      "ipv6 localhost",
			input:     "[::1]:8080",
			wantAddr:  "[::1]:8080",
			wantRole:  []string{},
			wantProto: "http",
		},
		{
			name:      "localhost with port",
			input:     "localhost:8080",
			wantAddr:  "localhost:8080",
			wantRole:  []string{},
			wantProto: "http",
		},
		{
			name:      "specific ipv4",
			input:     "192.168.1.10:8080",
			wantAddr:  "192.168.1.10:8080",
			wantRole:  []string{},
			wantProto: "http",
		},
		{
			name:      "with single role",
			input:     ":8080,role=api",
			wantAddr:  ":8080",
			wantRole:  []string{"api"},
			wantProto: "http",
		},
		{
			name:      "with multiple roles using plus",
			input:     ":8080,role=api+metrics",
			wantAddr:  ":8080",
			wantRole:  []string{"api", "metrics"},
			wantProto: "http",
		},
		{
			name:      "with multiple roles using repeated key",
			input:     ":8080,role=api,role=metrics",
			wantAddr:  ":8080",
			wantRole:  []string{"api", "metrics"},
			wantProto: "http",
		},
		{
			name:      "with mixed role syntax",
			input:     ":8080,role=api+metrics,role=cluster",
			wantAddr:  ":8080",
			wantRole:  []string{"api", "metrics", "cluster"},
			wantProto: "http",
		},
		{
			name:      "with protocol",
			input:     ":8080,proto=http",
			wantAddr:  ":8080",
			wantRole:  []string{},
			wantProto: "http",
		},
		{
			name:      "with role and protocol",
			input:     ":8080,role=api,proto=http",
			wantAddr:  ":8080",
			wantRole:  []string{"api"},
			wantProto: "http",
		},
		{
			name:      "ipv6 with role",
			input:     "[::]:8080,role=cluster",
			wantAddr:  "[::]:8080",
			wantRole:  []string{"cluster"},
			wantProto: "http",
		},
		{
			name:      "all role",
			input:     ":8080,role=all",
			wantAddr:  ":8080",
			wantRole:  []string{"all"},
			wantProto: "http",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseListenFlag(tt.input)
			if err != nil {
				t.Fatalf("ParseListenFlag() error = %v", err)
			}

			if got.Address != tt.wantAddr {
				t.Errorf("Address = %q, want %q", got.Address, tt.wantAddr)
			}

			if got.Protocol != tt.wantProto {
				t.Errorf("Protocol = %q, want %q", got.Protocol, tt.wantProto)
			}

			if len(got.Roles) != len(tt.wantRole) {
				t.Errorf("Roles count = %d, want %d (got: %v, want: %v)",
					len(got.Roles), len(tt.wantRole), got.Roles, tt.wantRole)
			} else {
				// Check all expected roles are present (order doesn't matter)
				roleMap := make(map[string]bool)
				for _, r := range got.Roles {
					roleMap[r] = true
				}
				for _, r := range tt.wantRole {
					if !roleMap[r] {
						t.Errorf("Missing expected role %q in %v", r, got.Roles)
					}
				}
			}
		})
	}
}

func TestParseListenFlag_InvalidFormats(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantError string
	}{
		{
			name:      "empty string",
			input:     "",
			wantError: "cannot be empty",
		},
		{
			name:      "no port",
			input:     "localhost",
			wantError: "invalid address format",
		},
		{
			name:      "invalid port - non-numeric",
			input:     ":abc",
			wantError: "invalid port",
		},
		{
			name:      "invalid port - too low",
			input:     ":0",
			wantError: "must be between 1 and 65535",
		},
		{
			name:      "invalid port - too high",
			input:     ":65536",
			wantError: "must be between 1 and 65535",
		},
		{
			name:      "invalid role",
			input:     ":8080,role=invalid",
			wantError: "invalid role",
		},
		{
			name:      "empty role",
			input:     ":8080,role=",
			wantError: "role cannot be empty",
		},
		{
			name:      "invalid protocol",
			input:     ":8080,proto=https",
			wantError: "only proto=http is supported",
		},
		{
			name:      "unknown option",
			input:     ":8080,foo=bar",
			wantError: "unknown option",
		},
		{
			name:      "malformed key-value",
			input:     ":8080,noequals",
			wantError: "invalid key=value format",
		},
		{
			name:      "empty key",
			input:     ":8080,=value",
			wantError: "empty key",
		},
		{
			name:      "hostname with spaces",
			input:     "host name:8080",
			wantError: "invalid hostname",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseListenFlag(tt.input)
			if err == nil {
				t.Fatalf("ParseListenFlag() expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Errorf("Error = %q, want substring %q", err.Error(), tt.wantError)
			}
		})
	}
}

func TestListenConfig_HasRole(t *testing.T) {
	tests := []struct {
		name      string
		config    *ListenConfig
		checkRole string
		want      bool
	}{
		{
			name:      "no roles configured - serves all",
			config:    &ListenConfig{Roles: []string{}},
			checkRole: "api",
			want:      true,
		},
		{
			name:      "has specific role",
			config:    &ListenConfig{Roles: []string{"api"}},
			checkRole: "api",
			want:      true,
		},
		{
			name:      "does not have role",
			config:    &ListenConfig{Roles: []string{"api"}},
			checkRole: "metrics",
			want:      false,
		},
		{
			name:      "has multiple roles - check first",
			config:    &ListenConfig{Roles: []string{"api", "metrics"}},
			checkRole: "api",
			want:      true,
		},
		{
			name:      "has multiple roles - check second",
			config:    &ListenConfig{Roles: []string{"api", "metrics"}},
			checkRole: "metrics",
			want:      true,
		},
		{
			name:      "has multiple roles - check missing",
			config:    &ListenConfig{Roles: []string{"api", "metrics"}},
			checkRole: "cluster",
			want:      false,
		},
		{
			name:      "has 'all' role",
			config:    &ListenConfig{Roles: []string{"all"}},
			checkRole: "api",
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.HasRole(tt.checkRole)
			if got != tt.want {
				t.Errorf("HasRole(%q) = %v, want %v", tt.checkRole, got, tt.want)
			}
		})
	}
}

func TestListenConfig_String(t *testing.T) {
	tests := []struct {
		name   string
		config *ListenConfig
		want   string
	}{
		{
			name:   "no roles",
			config: &ListenConfig{Address: ":8080", Roles: []string{}},
			want:   ":8080",
		},
		{
			name:   "with single role",
			config: &ListenConfig{Address: ":8080", Roles: []string{"api"}},
			want:   ":8080 (roles: api)",
		},
		{
			name:   "with multiple roles",
			config: &ListenConfig{Address: ":8080", Roles: []string{"api", "metrics"}},
			want:   ":8080 (roles: api, metrics)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.String()
			if got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListenConfig_HasExplicitRoles(t *testing.T) {
	tests := []struct {
		name   string
		config *ListenConfig
		want   bool
	}{
		{
			name:   "no roles",
			config: &ListenConfig{Roles: []string{}},
			want:   false,
		},
		{
			name:   "with single role",
			config: &ListenConfig{Roles: []string{"api"}},
			want:   true,
		},
		{
			name:   "with multiple roles",
			config: &ListenConfig{Roles: []string{"api", "metrics"}},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.HasExplicitRoles()
			if got != tt.want {
				t.Errorf("HasExplicitRoles() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSplitListenFlag(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "simple address",
			input: ":8080",
			want:  []string{":8080"},
		},
		{
			name:  "address with one option",
			input: ":8080,role=api",
			want:  []string{":8080", "role=api"},
		},
		{
			name:  "address with multiple options",
			input: ":8080,role=api,proto=http",
			want:  []string{":8080", "role=api", "proto=http"},
		},
		{
			name:  "ipv6 address",
			input: "[::]:8080",
			want:  []string{"[::]:8080"},
		},
		{
			name:  "ipv6 address with options",
			input: "[::]:8080,role=api",
			want:  []string{"[::]:8080", "role=api"},
		},
		{
			name:  "ipv6 full address with options",
			input: "[2001:db8::1]:8080,role=cluster",
			want:  []string{"[2001:db8::1]:8080", "role=cluster"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitListenFlag(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("splitListenFlag() = %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitListenFlag()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
