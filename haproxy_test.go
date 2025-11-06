package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Test Utilities for HAProxy Mocking ---

// setupMockExecutor replaces the global haproxyCommandExecutor with a mock function
// and restores the original function when the test completes, ensuring test isolation.
func setupMockExecutor(t *testing.T, mock HAProxyExecutor) {
	originalExecutor := haproxyCommandExecutor
	haproxyCommandExecutor = mock
	t.Cleanup(func() {
		haproxyCommandExecutor = originalExecutor
	})
}

// setTestTimeouts sets aggressive timeout/retry settings for tests
// that rely on the real network implementation and restores the original values using t.Cleanup.
func setTestTimeouts(t *testing.T) {
	originalMaxRetries := maxRetries
	originalRetryDelay := retryDelay
	originalDialTimeout := dialTimeout

	// Set very short, aggressive settings for testing
	maxRetries = 1 // Only 1 attempt to fail fast
	retryDelay = 1 * time.Millisecond
	dialTimeout = 100 * time.Millisecond

	t.Cleanup(func() {
		maxRetries = originalMaxRetries
		retryDelay = originalRetryDelay
		dialTimeout = originalDialTimeout
	})
}

// --- Test Cases for BlockIP/UnblockIP (Mocked Flow Control) ---

// TestBlockAndUnblockIP_SuccessFlow tests the complete, successful path of HAProxy command execution.
// This ensures that BlockIP and UnblockIP generate the correct commands for the specified tables and IP version.
func TestBlockAndUnblockIP_SuccessFlow(t *testing.T) {
	resetGlobalState()
	processor := &Processor{
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
		Config: &AppConfig{
			HAProxyAddresses: []string{"127.0.0.1:9999"},
			DurationToTableName: map[time.Duration]string{
				10 * time.Minute: "table_10m",
			},
			BlockTableNameFallback: "table_long",
		},
		ChainMutex: &sync.RWMutex{},
	}

	var mu sync.Mutex
	var commandsReceived []string
	expectedBlockCommand := "set table table_10m_ipv4 key 192.0.2.1 data.gpc0 1"
	// Unblock must target all configured tables.
	expectedUnblockCommands := map[string]struct{}{
		"clear table table_10m_ipv4 key 192.0.2.1":  {},
		"clear table table_long_ipv4 key 192.0.2.1": {},
	}

	mockExecutor := func(addr, ip, command string) error {
		mu.Lock()
		commandsReceived = append(commandsReceived, strings.TrimSpace(command))
		mu.Unlock()
		return nil
	}
	setupMockExecutor(t, mockExecutor)

	ipInfo := NewIPInfo("192.0.2.1")
	duration := 10 * time.Minute

	// Test BlockIP
	if err := processor.BlockIP(ipInfo, duration); err != nil {
		t.Fatalf("BlockIP failed unexpectedly: %v", err)
	}

	// Assertions for BlockIP
	mu.Lock()
	if len(commandsReceived) != 1 || commandsReceived[0] != expectedBlockCommand {
		t.Fatalf("BlockIP: Expected ['%s'], got %v", expectedBlockCommand, commandsReceived)
	}
	commandsReceived = nil
	mu.Unlock()

	// Test UnblockIP
	if err := processor.UnblockIP(ipInfo); err != nil {
		t.Fatalf("UnblockIP failed unexpectedly: %v", err)
	}

	// Assertions for UnblockIP
	mu.Lock()
	receivedUnblockCmds := commandsReceived
	mu.Unlock()

	if len(receivedUnblockCmds) != len(expectedUnblockCommands) {
		t.Fatalf("UnblockIP: Expected %d commands, got %d. Received: %v",
			len(expectedUnblockCommands), len(receivedUnblockCmds), receivedUnblockCmds)
	}

	for _, cmd := range receivedUnblockCmds {
		if _, ok := expectedUnblockCommands[cmd]; !ok {
			t.Fatalf("UnblockIP: Received unexpected command: %s. Expected commands: %v", cmd, expectedUnblockCommands)
		}
	}
}

// TestUnblockIP_ErrorTolerance_Mocked ensures that an execution error on one HAProxy instance
// does not prevent the unblock attempt on other configured instances/tables.
func TestUnblockIP_ErrorTolerance_Mocked(t *testing.T) {
	resetGlobalState()
	const workingAddr = "127.0.0.1:9999"
	const failedAddr = "127.0.0.1:65535"

	processor := &Processor{
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
		Config: &AppConfig{
			HAProxyAddresses: []string{workingAddr, failedAddr},
			DurationToTableName: map[time.Duration]string{
				1 * time.Minute: "table_1m",
			},
			BlockTableNameFallback: "table_fallback",
		},
		ChainMutex: &sync.RWMutex{},
	}

	ipInfo := NewIPInfo("2001:db8::1")
	successfulCmds := 0

	// Mock executor simulates success on 'workingAddr' and a connection failure on 'failedAddr'.
	mockExecutor := func(addr, ip, command string) error {
		if addr == workingAddr {
			successfulCmds++
			return nil
		}
		return fmt.Errorf("failed to connect to HAProxy instance %s: %w", addr, errors.New("dial tcp: connection refused"))
	}
	setupMockExecutor(t, mockExecutor)

	// Execute UnblockIP
	err := processor.UnblockIP(ipInfo)
	if err != nil {
		t.Fatalf("UnblockIP returned an unexpected error: %v", err)
	}

	// Unblock is attempted on two tables (1m and fallback) for two addresses.
	// Since one address fails for both tables, we expect 2 successful commands.
	if successfulCmds != 2 {
		t.Errorf("Expected 2 successful command executions (on the working mock), got %d", successfulCmds)
	}
}

// TestUnblockIP_NoAddresses tests that the function exits gracefully when no HAProxy addresses are configured.
func TestUnblockIP_NoAddresses(t *testing.T) {
	resetGlobalState()
	processor := &Processor{
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
		Config: &AppConfig{
			HAProxyAddresses:       nil,
			DurationToTableName:    map[time.Duration]string{time.Minute: "t1"},
			BlockTableNameFallback: "t_fall",
		},
		ChainMutex: &sync.RWMutex{},
	}

	// The mock executor should fail the test if called.
	mockExecutor := func(addr, ip, command string) error {
		t.Fatal("HAProxy executor was called when addresses were empty")
		return nil
	}
	setupMockExecutor(t, mockExecutor)

	ipInfo := NewIPInfo("192.0.2.1")
	err := processor.UnblockIP(ipInfo)
	if err != nil {
		t.Fatalf("UnblockIP returned an unexpected error when no addresses were configured: %v", err)
	}
}

// TestUnblockIP_NoTables tests that the function exits gracefully when no HAProxy tables are configured.
func TestUnblockIP_NoTables(t *testing.T) {
	resetGlobalState()
	processor := &Processor{
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
		Config: &AppConfig{
			HAProxyAddresses: []string{"127.0.0.1:9999"},
		},
		ChainMutex: &sync.RWMutex{},
	}

	// The mock executor should fail the test if called.
	mockExecutor := func(addr, ip, command string) error {
		t.Fatal("HAProxy executor was called when no tables were configured")
		return nil
	}
	setupMockExecutor(t, mockExecutor)

	ipInfo := NewIPInfo("192.0.2.1")
	err := processor.UnblockIP(ipInfo)
	if err != nil {
		t.Fatalf("UnblockIP returned an unexpected error: %v", err)
	}
}

// TestBlockIP_InvalidVersion tests that blocking an IP with an unrecognized version is skipped
// and no HAProxy command is attempted.
func TestBlockIP_InvalidVersion(t *testing.T) {
	resetGlobalState()
	processor := &Processor{
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
		Config: &AppConfig{
			HAProxyAddresses:    []string{"127.0.0.1:9999"},
			DurationToTableName: map[time.Duration]string{time.Minute: "table_1m"},
		},
		ChainMutex: &sync.RWMutex{},
	}

	// The mock executor should fail the test if called.
	mockExecutor := func(addr, ip, command string) error {
		t.Fatal("HAProxy executor was called when IP version was invalid")
		return nil
	}
	setupMockExecutor(t, mockExecutor)

	ipInfo := NewIPInfo("invalid-ip-string")
	duration := 1 * time.Minute

	// BlockIP with an invalid IP version (0)
	err := processor.BlockIP(ipInfo, duration)
	if err != nil {
		t.Fatalf("BlockIP failed unexpectedly for invalid version: %v", err)
	}
}

// TestUnblockIP_InvalidVersion tests that unblocking an IP with an unrecognized version is skipped
// and no HAProxy command is attempted.
func TestUnblockIP_InvalidVersion(t *testing.T) {
	resetGlobalState()
	processor := &Processor{
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
		Config: &AppConfig{
			HAProxyAddresses:    []string{"127.0.0.1:9999"},
			DurationToTableName: map[time.Duration]string{time.Minute: "table_1m"},
		},
		ChainMutex: &sync.RWMutex{},
	}

	// The mock executor should fail the test if called.
	mockExecutor := func(addr, ip, command string) error {
		t.Fatal("HAProxy executor was called when IP version was invalid")
		return nil
	}
	setupMockExecutor(t, mockExecutor)

	ipInfo := NewIPInfo("invalid-ip-string")

	// UnblockIP with an invalid IP version (0)
	err := processor.UnblockIP(ipInfo)
	if err != nil {
		t.Fatalf("UnblockIP returned an unexpected error: %v", err)
	}
}

// --- Test Cases for executeHAProxyCommandImpl (Real Networking Logic) ---

// startMockServer sets up a temporary TCP server on a random port for testing executeHAProxyCommandImpl.
// It accepts one connection and hands it to the handler function.
func startMockServer(t *testing.T, handler func(net.Conn)) (net.Listener, string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}
	addr := l.Addr().String()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := l.Accept()
		if err != nil {
			// Expected when the listener is closed externally.
			if !strings.Contains(err.Error(), "use of closed network connection") {
				return
			}
			return
		}
		defer conn.Close()
		handler(conn)
	}()

	return l, addr, func() {
		l.Close()
		wg.Wait()
	}
}

// TestExecuteHAProxyCommandImpl_Success tests the happy path of a single command execution:
// successful connection, write, and a response indicating success (empty or newline).
func TestExecuteHAProxyCommandImpl_Success(t *testing.T) {
	setTestTimeouts(t)
	// Start a minimal TCP server that returns an empty response (success).
	_, addr, closeFn := startMockServer(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		_, _ = reader.ReadString('\n')
		// Write the success response (newline).
		_, _ = conn.Write([]byte(" \n"))
	})
	defer closeFn()

	err := executeHAProxyCommand(addr, "192.0.2.1", "set table foo key bar\n")
	if err != nil {
		t.Fatalf("Expected success, got error: %v", err)
	}
}

// TestExecuteHAProxyCommandImpl_HAProxyError tests command execution where the HAProxy instance
// successfully responds with a non-empty string, indicating a command-level error.
func TestExecuteHAProxyCommandImpl_HAProxyError(t *testing.T) {
	setTestTimeouts(t)
	// Start a minimal TCP server that returns a mock HAProxy error response.
	_, addr, closeFn := startMockServer(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		_, _ = reader.ReadString('\n')
		// Write the error response.
		_, _ = conn.Write([]byte("No such table: foo\n"))
	})
	defer closeFn()

	err := executeHAProxyCommand(addr, "192.0.2.1", "set table foo key bar\n")
	if err == nil {
		t.Fatal("Expected an HAProxy command execution error, got nil")
	}
	expectedErrMsg := "HAProxy command execution failed for IP 192.0.2.1 (Response: No such table: foo)"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Error mismatch.\nExpected error containing: %s\nGot: %v", expectedErrMsg, err)
	}
}

// TestExecuteHAProxyCommandImpl_ConnectError tests a failure during the connection attempt (e.g., connection refused).
// The test verifies the retry mechanism is used and the final error reflects the connection failure.
func TestExecuteHAProxyCommandImpl_ConnectError(t *testing.T) {
	setTestTimeouts(t)
	// Use an address that is guaranteed to fail connection (e.g., high port).
	addr := "127.0.0.1:65535"

	// The function will retry maxRetries times, returning the last connection error.
	err := executeHAProxyCommand(addr, "192.0.2.1", "set table foo key bar\n")
	if err == nil {
		t.Fatal("Expected a connection error after retries, got nil")
	}
	expectedErrMsg := "failed to connect to HAProxy instance"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Error mismatch.\nExpected error containing: %s\nGot: %v", expectedErrMsg, err)
	}
}

// TestExecuteHAProxyCommandImpl_WriteError tests a failure when the HAProxy server closes the connection
// immediately after the dial. This verifies the client handles the subsequent read error/timeout correctly after retries.
func TestExecuteHAProxyCommandImpl_WriteError(t *testing.T) {
	setTestTimeouts(t)
	// Start a server that closes the connection immediately after accept.
	_, addr, closeFn := startMockServer(t, func(conn net.Conn) {
		// Close the connection before the client can write.
		conn.Close()
	})
	defer closeFn()

	// The function will retry maxRetries times.
	err := executeHAProxyCommand(addr, "192.0.2.1", "set table foo key bar\n")
	if err == nil {
		t.Fatal("Expected an error after retries, got nil")
	}

	// We check for the error string returned by the `executeHAProxyCommandImpl` on a read failure
	// (the connection closing leads to a read error after write succeeds due to buffering).
	expectedErrMsg := "HAProxy response read error from"

	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Error mismatch.\nExpected error containing: %s\nGot: %v", expectedErrMsg, err)
	}
}

// TestExecuteHAProxyCommandImpl_MalformedResponse tests the case where HAProxy
// returns something unexpected that is neither a standard error nor a clean success.
func TestExecuteHAProxyCommandImpl_MalformedResponse(t *testing.T) {
	resetGlobalState()
	setTestTimeouts(t)

	// Mock server that returns a non-standard response (e.g., garbage string)
	_, addr, closeFn := startMockServer(t, func(conn net.Conn) {
		// Handler logic (wait for command, then respond with garbage)
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		reader := bufio.NewReader(conn)
		// Read the command
		_, err := reader.ReadString('\n')
		if err != nil {
			// Connection closed or error before command read
			return
		}

		// Respond with a non-HAProxy-standard string
		conn.Write([]byte("UNEXPECTED_GARBAGE_RESPONSE\n"))
	})
	defer closeFn()

	// Use the real implementation for this test
	originalExecutor := haproxyCommandExecutor
	haproxyCommandExecutor = executeHAProxyCommandImpl
	defer func() { haproxyCommandExecutor = originalExecutor }()

	// Act: Execute a command (retries are internal to the function)
	err := executeHAProxyCommand(addr, "192.0.2.1", "set table foo key bar\n")

	// Assert: It should detect the non-success/non-error response and report a failure
	// that includes the malformed response string.
	if err == nil {
		t.Fatal("Expected an error for malformed HAProxy response, got nil")
	}

	// Check for the error message that indicates command execution failed due to a bad response.
	expectedErrMsg := "HAProxy command execution failed for IP"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Error mismatch.\nExpected error containing: %s\nGot: %v", expectedErrMsg, err)
	}
}
