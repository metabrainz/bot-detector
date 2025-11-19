package commandline

import (
	"os"
	"testing"

	"bot-detector/internal/cluster"
)

func TestParseEnv_Empty(t *testing.T) {
	// Ensure BOT_DETECTOR_NODES is not set
	_ = os.Unsetenv("BOT_DETECTOR_NODES")

	params, err := ParseEnv()
	if err != nil {
		t.Fatalf("ParseEnv() with no env vars should not error: %v", err)
	}

	if len(params.ClusterNodes) > 0 {
		t.Errorf("Expected no cluster nodes when BOT_DETECTOR_NODES is not set, got %d nodes", len(params.ClusterNodes))
	}
}

func TestParseClusterNodes_Valid_SingleNode(t *testing.T) {
	input := "leader:http://localhost:8080"
	expected := []cluster.NodeConfig{
		{Name: "leader", Address: "http://localhost:8080"},
	}

	result, err := parseClusterNodes(input)
	if err != nil {
		t.Fatalf("parseClusterNodes() failed: %v", err)
	}

	if !compareNodeConfigs(result, expected) {
		t.Errorf("parseClusterNodes() = %+v, want %+v", result, expected)
	}
}

func TestParseClusterNodes_Valid_MultipleNodes(t *testing.T) {
	input := "leader:http://localhost:8080;follower:http://localhost:9090"
	expected := []cluster.NodeConfig{
		{Name: "leader", Address: "http://localhost:8080"},
		{Name: "follower", Address: "http://localhost:9090"},
	}

	result, err := parseClusterNodes(input)
	if err != nil {
		t.Fatalf("parseClusterNodes() failed: %v", err)
	}

	if !compareNodeConfigs(result, expected) {
		t.Errorf("parseClusterNodes() = %+v, want %+v", result, expected)
	}
}

func TestParseClusterNodes_Valid_IPv6(t *testing.T) {
	input := "leader:http://[2001:db8::1]:8080;follower:http://[2001:db8::2]:9090"
	expected := []cluster.NodeConfig{
		{Name: "leader", Address: "http://[2001:db8::1]:8080"},
		{Name: "follower", Address: "http://[2001:db8::2]:9090"},
	}

	result, err := parseClusterNodes(input)
	if err != nil {
		t.Fatalf("parseClusterNodes() failed: %v", err)
	}

	if !compareNodeConfigs(result, expected) {
		t.Errorf("parseClusterNodes() = %+v, want %+v", result, expected)
	}
}

func TestParseClusterNodes_Valid_URLsWithPorts(t *testing.T) {
	input := "main:http://example.com:8080;backup:http://10.0.0.5:9090"
	expected := []cluster.NodeConfig{
		{Name: "main", Address: "http://example.com:8080"},
		{Name: "backup", Address: "http://10.0.0.5:9090"},
	}

	result, err := parseClusterNodes(input)
	if err != nil {
		t.Fatalf("parseClusterNodes() failed: %v", err)
	}

	if !compareNodeConfigs(result, expected) {
		t.Errorf("parseClusterNodes() = %+v, want %+v", result, expected)
	}
}

func TestParseClusterNodes_Valid_TrailingSemicolon(t *testing.T) {
	input := "leader:http://localhost:8080;follower:http://localhost:9090;"
	expected := []cluster.NodeConfig{
		{Name: "leader", Address: "http://localhost:8080"},
		{Name: "follower", Address: "http://localhost:9090"},
	}

	result, err := parseClusterNodes(input)
	if err != nil {
		t.Fatalf("parseClusterNodes() failed: %v", err)
	}

	if !compareNodeConfigs(result, expected) {
		t.Errorf("parseClusterNodes() = %+v, want %+v", result, expected)
	}
}

func TestParseClusterNodes_Valid_ExtraWhitespace(t *testing.T) {
	input := " leader : http://localhost:8080 ; follower : http://localhost:9090 "
	expected := []cluster.NodeConfig{
		{Name: "leader", Address: "http://localhost:8080"},
		{Name: "follower", Address: "http://localhost:9090"},
	}

	result, err := parseClusterNodes(input)
	if err != nil {
		t.Fatalf("parseClusterNodes() failed: %v", err)
	}

	if !compareNodeConfigs(result, expected) {
		t.Errorf("parseClusterNodes() = %+v, want %+v", result, expected)
	}
}

func TestParseClusterNodes_Error_EmptyString(t *testing.T) {
	input := ""
	_, err := parseClusterNodes(input)
	if err == nil {
		t.Error("parseClusterNodes() with empty string should return error")
	}
}

func TestParseClusterNodes_Error_MissingColon(t *testing.T) {
	input := "leader-localhost-8080"
	_, err := parseClusterNodes(input)
	if err == nil {
		t.Error("parseClusterNodes() with missing colon should return error")
	}
}

func TestParseClusterNodes_Error_EmptyNodeName(t *testing.T) {
	input := ":http://localhost:8080"
	_, err := parseClusterNodes(input)
	if err == nil {
		t.Error("parseClusterNodes() with empty node name should return error")
	}
}

func TestParseClusterNodes_Error_EmptyAddress(t *testing.T) {
	input := "leader:"
	_, err := parseClusterNodes(input)
	if err == nil {
		t.Error("parseClusterNodes() with empty address should return error")
	}
}

func TestParseClusterNodes_Error_OnlyWhitespace(t *testing.T) {
	input := "   ;  ;  "
	_, err := parseClusterNodes(input)
	if err == nil {
		t.Error("parseClusterNodes() with only whitespace should return error")
	}
}

func TestParseEnv_Integration(t *testing.T) {
	// Save original value to restore after test
	originalValue := os.Getenv("BOT_DETECTOR_NODES")
	defer func() {
		if originalValue != "" {
			if err := os.Setenv("BOT_DETECTOR_NODES", originalValue); err != nil {
				t.Fatalf("Failed to reset env var BOT_DETECTOR_NODES: %v", err)
			}
		} else {
			_ = os.Unsetenv("BOT_DETECTOR_NODES")
		}
	}()

	// Test with valid BOT_DETECTOR_NODES
	testValue := "leader:http://localhost:8080;follower:http://localhost:9090"
	if err := os.Setenv("BOT_DETECTOR_NODES", testValue); err != nil {
		t.Fatalf("Failed to set env var BOT_DETECTOR_NODES: %v", err)
	}

	params, err := ParseEnv()
	if err != nil {
		t.Fatalf("ParseEnv() failed: %v", err)
	}

	expected := []cluster.NodeConfig{
		{Name: "leader", Address: "http://localhost:8080"},
		{Name: "follower", Address: "http://localhost:9090"},
	}

	if !compareNodeConfigs(params.ClusterNodes, expected) {
		t.Errorf("ParseEnv().ClusterNodes = %+v, want %+v", params.ClusterNodes, expected)
	}
}

func TestParseEnv_Integration_Invalid(t *testing.T) {
	// Save original value to restore after test
	originalValue := os.Getenv("BOT_DETECTOR_NODES")
	defer func() {
		if originalValue != "" {
			if err := os.Setenv("BOT_DETECTOR_NODES", originalValue); err != nil {
				t.Fatalf("Failed to reset env var BOT_DETECTOR_NODES: %v", err)
			}
		} else {
			_ = os.Unsetenv("BOT_DETECTOR_NODES")
		}
	}()

	// Test with invalid BOT_DETECTOR_NODES
	testValue := "invalid-no-colon"
	if err := os.Setenv("BOT_DETECTOR_NODES", testValue); err != nil {
		t.Fatalf("Failed to set env var BOT_DETECTOR_NODES: %v", err)
	}

	_, err := ParseEnv()
	if err == nil {
		t.Error("ParseEnv() with invalid BOT_DETECTOR_NODES should return error")
	}
}

// compareNodeConfigs compares two slices of NodeConfig for equality.
func compareNodeConfigs(a, b []cluster.NodeConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Address != b[i].Address {
			return false
		}
	}
	return true
}
