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

// --- Test Cases for BlockIP/UnblockIP (Mocked Flow Control) ---

// TestBlockAndUnblockIP_SuccessFlow tests the complete, successful path of HAProxy command execution.
// This ensures that BlockIP and UnblockIP generate the correct commands for the specified tables and IP version.
func TestBlockAndUnblockIP_SuccessFlow(t *testing.T) {
	resetGlobalState()
	processor := &Processor{
		ChainMutex: &sync.RWMutex{},
		Config: &AppConfig{
			HAProxyAddresses: []string{"127.0.0.1:9999"},
			DurationToTableName: map[time.Duration]string{
				10 * time.Minute: "table_10m",
			},
			BlockTableNameFallback: "table_long",
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
	}
	blocker := &HAProxyBlocker{P: processor}
	processor.Blocker = blocker

	var mu sync.Mutex
	var commandsReceived []string
	expectedBlockCommand := "set table table_10m_ipv4 key 192.0.2.1 data.gpc0 1"
	// Unblock must target all configured tables.
	expectedUnblockCommands := map[string]struct{}{
		"clear table table_10m_ipv4 key 192.0.2.1":  {},
		"clear table table_long_ipv4 key 192.0.2.1": {},
	}

	processor.CommandExecutor = func(p *Processor, addr, ip, command string) error {
		mu.Lock()
		commandsReceived = append(commandsReceived, strings.TrimSpace(command))
		mu.Unlock()
		return nil
	}

	ipInfo := NewIPInfo("192.0.2.1")
	duration := 10 * time.Minute

	// Test BlockIP
	if err := blocker.Block(ipInfo, duration); err != nil {
		t.Fatalf("Block failed unexpectedly: %v", err)
	}

	// Assertions for BlockIP
	mu.Lock()
	if len(commandsReceived) != 1 || commandsReceived[0] != expectedBlockCommand {
		t.Fatalf("BlockIP: Expected ['%s'], got %v", expectedBlockCommand, commandsReceived)
	}
	commandsReceived = nil
	mu.Unlock()

	// Test UnblockIP
	if err := blocker.Unblock(ipInfo); err != nil {
		t.Fatalf("Unblock failed unexpectedly: %v", err)
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

// TestBlockIP_FallbackTable verifies that if a duration is not found in the duration_tables map,
// the command falls back to using the table with the longest configured duration.
func TestBlockIP_FallbackTable(t *testing.T) {
	resetGlobalState()
	processor := &Processor{
		ChainMutex: &sync.RWMutex{},
		Config: &AppConfig{
			HAProxyAddresses: []string{"127.0.0.1:9999"},
			DurationToTableName: map[time.Duration]string{
				5 * time.Minute: "table_5m",
				1 * time.Hour:   "table_1h", // This will be the fallback
			},
			BlockTableNameFallback: "table_1h", // Set explicitly as LoadChainsFromYAML would
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
	}
	blocker := &HAProxyBlocker{P: processor}
	processor.Blocker = blocker

	var commandReceived string
	processor.CommandExecutor = func(p *Processor, addr, ip, command string) error {
		commandReceived = strings.TrimSpace(command)
		return nil
	}
	ipInfo := NewIPInfo("192.0.2.5")
	// Use a duration that is NOT in the map.
	unconfiguredDuration := 30 * time.Minute

	// --- Act ---
	blocker.Block(ipInfo, unconfiguredDuration)

	// --- Assert ---
	expectedCommand := "set table table_1h_ipv4 key 192.0.2.5 data.gpc0 1"
	if commandReceived != expectedCommand {
		t.Errorf("Expected command to use fallback table '%s', but got: '%s'", expectedCommand, commandReceived)
	}
}

// TestBlockIP_NoTableFound verifies that if no table matches the duration and no fallback is set,
// the block is skipped and a warning is logged.
func TestBlockIP_NoTableFound(t *testing.T) {
	resetGlobalState()

	// Capture log output
	var capturedLog string
	logCaptureFunc := func(level LogLevel, tag string, format string, args ...interface{}) {
		if tag == "SKIP_BLOCK" {
			capturedLog = fmt.Sprintf(format, args...)
		}
	}

	processor := &Processor{
		ChainMutex: &sync.RWMutex{},
		Config: &AppConfig{
			HAProxyAddresses:    []string{"127.0.0.1:9999"},
			DurationToTableName: make(map[time.Duration]string), // No tables configured
			// No BlockTableNameFallback configured
		},
		LogFunc: logCaptureFunc,
	}
	blocker := &HAProxyBlocker{P: processor}
	processor.Blocker = blocker

	// The mock executor should fail the test if it's ever called.
	processor.CommandExecutor = func(p *Processor, addr, ip, command string) error {
		t.Fatal("HAProxy executor was called when no table was found.")
		return nil
	}
	ipInfo := NewIPInfo("192.0.2.10")

	// --- Act ---
	blocker.Block(ipInfo, 5*time.Minute)

	// --- Assert ---
	expectedLog := "No HAProxy table found"
	if !strings.Contains(capturedLog, expectedLog) {
		t.Errorf("Expected a 'SKIP_BLOCK' log containing '%s', but got: '%s'", expectedLog, capturedLog)
	}
}

// TestUnblockIP_ErrorTolerance_Mocked ensures that an execution error on one HAProxy instance
// does not prevent the unblock attempt on other configured instances/tables.
func TestUnblockIP_ErrorTolerance_Mocked(t *testing.T) {
	resetGlobalState()
	const workingAddr = "127.0.0.1:9999"
	const failedAddr = "127.0.0.1:65535"

	processor := &Processor{
		ChainMutex: &sync.RWMutex{},
		Config: &AppConfig{
			HAProxyAddresses: []string{workingAddr, failedAddr},
			DurationToTableName: map[time.Duration]string{
				1 * time.Minute: "table_1m",
			},
			BlockTableNameFallback: "table_fallback",
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
	}
	blocker := &HAProxyBlocker{P: processor}
	processor.Blocker = blocker

	ipInfo := NewIPInfo("2001:db8::1")
	successfulCmds := 0

	// The executor simulates success on 'workingAddr' and a connection failure on 'failedAddr'.
	processor.CommandExecutor = func(p *Processor, addr, ip, command string) error {
		if addr == workingAddr {
			successfulCmds++
			return nil
		}
		return fmt.Errorf("failed to connect to HAProxy instance %s: %w", addr, errors.New("dial tcp: connection refused"))
	}

	// Execute UnblockIP
	err := blocker.Unblock(ipInfo)
	// With the error propagation fix, we now EXPECT an error here because one of the instances failed.
	if err == nil {
		t.Fatal("UnblockIP was expected to return an error due to a failed instance, but it returned nil.")
	}
	if !strings.Contains(err.Error(), "2 HAProxy commands failed") {
		t.Errorf("Expected error message to indicate 2 failures, but got: %v", err)
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
		ChainMutex: &sync.RWMutex{},
		Config: &AppConfig{
			HAProxyAddresses:       nil,
			DurationToTableName:    map[time.Duration]string{time.Minute: "t1"},
			BlockTableNameFallback: "t_fall",
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
	}
	blocker := &HAProxyBlocker{P: processor}
	processor.Blocker = blocker

	// The mock executor should fail the test if called.
	processor.CommandExecutor = func(p *Processor, addr, ip, command string) error {
		t.Fatal("HAProxy executor was called when addresses were empty")
		return nil
	}
	ipInfo := NewIPInfo("192.0.2.1")
	err := blocker.Unblock(ipInfo)
	if err != nil {
		t.Fatalf("UnblockIP returned an unexpected error when no addresses were configured: %v", err)
	}
}

// TestUnblockIP_NoTables tests that the function exits gracefully when no HAProxy tables are configured.
func TestUnblockIP_NoTables(t *testing.T) {
	resetGlobalState()
	processor := &Processor{
		ChainMutex: &sync.RWMutex{},
		Config: &AppConfig{
			HAProxyAddresses: []string{"127.0.0.1:9999"},
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
	}
	blocker := &HAProxyBlocker{P: processor}
	processor.Blocker = blocker

	// The mock executor should fail the test if called.
	processor.CommandExecutor = func(p *Processor, addr, ip, command string) error {
		t.Fatal("HAProxy executor was called when no tables were configured")
		return nil
	}
	ipInfo := NewIPInfo("192.0.2.1")
	err := blocker.Unblock(ipInfo)
	if err != nil {
		t.Fatalf("UnblockIP returned an unexpected error: %v", err)
	}
}

// TestBlockIP_InvalidVersion tests that blocking an IP with an unrecognized version is skipped
// and no HAProxy command is attempted.
func TestBlockIP_InvalidVersion(t *testing.T) {
	resetGlobalState()
	processor := &Processor{
		ChainMutex: &sync.RWMutex{},
		Config: &AppConfig{
			HAProxyAddresses:    []string{"127.0.0.1:9999"},
			DurationToTableName: map[time.Duration]string{time.Minute: "table_1m"},
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
	}
	blocker := &HAProxyBlocker{P: processor}
	processor.Blocker = blocker

	// The mock executor should fail the test if called.
	processor.CommandExecutor = func(p *Processor, addr, ip, command string) error {
		t.Fatal("HAProxy executor was called when IP version was invalid")
		return nil
	}
	ipInfo := NewIPInfo("invalid-ip-string")
	duration := 1 * time.Minute

	// BlockIP with an invalid IP version (0)
	err := blocker.Block(ipInfo, duration)
	if err != nil {
		t.Fatalf("BlockIP failed unexpectedly for invalid version: %v", err)
	}
}

// TestUnblockIP_InvalidVersion tests that unblocking an IP with an unrecognized version is skipped
// and no HAProxy command is attempted.
func TestUnblockIP_InvalidVersion(t *testing.T) {
	resetGlobalState()
	processor := &Processor{
		ChainMutex: &sync.RWMutex{},
		Config: &AppConfig{
			HAProxyAddresses:    []string{"127.0.0.1:9999"},
			DurationToTableName: map[time.Duration]string{time.Minute: "table_1m"},
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
	}
	blocker := &HAProxyBlocker{P: processor}
	processor.Blocker = blocker

	// The mock executor should fail the test if called.
	processor.CommandExecutor = func(p *Processor, addr, ip, command string) error {
		t.Fatal("HAProxy executor was called when IP version was invalid")
		return nil
	}
	ipInfo := NewIPInfo("invalid-ip-string")

	// UnblockIP with an invalid IP version (0)
	err := blocker.Unblock(ipInfo)
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
	// Start a minimal TCP server that returns an empty response (success).
	_, addr, closeFn := startMockServer(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		_, _ = reader.ReadString('\n')
		// Write the success response (newline).
		_, _ = conn.Write([]byte(" \n"))
	})
	defer closeFn()

	processor := &Processor{
		Config: &AppConfig{
			HAProxyMaxRetries:  1,
			HAProxyRetryDelay:  1 * time.Millisecond,
			HAProxyDialTimeout: 100 * time.Millisecond,
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {},
	}
	err := executeCommandImpl(processor, addr, "192.0.2.1", "set table foo key bar\n")
	if err != nil {
		t.Fatalf("Expected success, got error: %v", err)
	}
}

// TestExecuteHAProxyCommandImpl_HAProxyError tests command execution where the HAProxy instance
// successfully responds with a non-empty string, indicating a command-level error.
func TestExecuteHAProxyCommandImpl_HAProxyError(t *testing.T) {
	// Start a minimal TCP server that returns a mock HAProxy error response.
	_, addr, closeFn := startMockServer(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		_, _ = reader.ReadString('\n')
		// Write the error response.
		_, _ = conn.Write([]byte("No such table: foo\n"))
	})
	defer closeFn()

	processor := &Processor{
		Config: &AppConfig{
			HAProxyMaxRetries:  1,
			HAProxyRetryDelay:  1 * time.Millisecond,
			HAProxyDialTimeout: 100 * time.Millisecond,
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {},
	}
	err := executeCommandImpl(processor, addr, "192.0.2.1", "set table foo key bar\n")
	if err == nil {
		t.Fatal("Expected an HAProxy command execution error, got nil")
	}
	expectedErrMsg := "HAProxy command execution failed for IP 192.0.2.1 (Response: No such table: foo)"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Error mismatch.\nExpected error containing: %s\nGot: %v", expectedErrMsg, err)
	}
}

// TestExecuteHAProxyCommandImpl_EOFWithData tests the edge case where the server sends
// a response without a trailing newline and immediately closes the connection.
func TestExecuteHAProxyCommandImpl_EOFWithData(t *testing.T) {
	// Start a server that writes a response and then closes the connection,
	// triggering an io.EOF on the client's ReadString call.
	_, addr, closeFn := startMockServer(t, func(conn net.Conn) {
		// Read the command from the client to unblock the write.
		reader := bufio.NewReader(conn)
		_, _ = reader.ReadString('\n')
		// Write a response WITHOUT a trailing newline.
		_, _ = conn.Write([]byte("some data"))
		// The handler will close the connection upon returning.
	})
	defer closeFn()

	processor := &Processor{
		Config: &AppConfig{
			HAProxyMaxRetries:  1,
			HAProxyRetryDelay:  1 * time.Millisecond,
			HAProxyDialTimeout: 100 * time.Millisecond,
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {},
	}
	err := executeCommandImpl(processor, addr, "192.0.2.1", "set table foo key bar\n")
	if err == nil {
		t.Fatal("Expected an error due to non-empty response with EOF, but got nil")
	}

	expectedErrMsg := "HAProxy command execution failed for IP 192.0.2.1 (Response: some data)"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Error mismatch.\nExpected error containing: %s\nGot: %v", expectedErrMsg, err)
	}
}

// TestUnblockIP_WithFallbackOnly verifies that UnblockIP correctly targets the fallback table
// even if it's the only table configured.
func TestUnblockIP_WithFallbackOnly(t *testing.T) {
	resetGlobalState()
	processor := &Processor{
		ChainMutex: &sync.RWMutex{},
		Config: &AppConfig{
			HAProxyAddresses:       []string{"127.0.0.1:9999"},
			DurationToTableName:    make(map[time.Duration]string), // No main tables
			BlockTableNameFallback: "fallback_table",               // Only a fallback table
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
	}
	blocker := &HAProxyBlocker{P: processor}
	processor.Blocker = blocker

	var commandReceived string
	processor.CommandExecutor = func(p *Processor, addr, ip, command string) error {
		commandReceived = strings.TrimSpace(command)
		return nil
	}
	ipInfo := NewIPInfo("192.0.2.20")

	// --- Act ---
	err := blocker.Unblock(ipInfo)
	if err != nil {
		t.Fatalf("UnblockIP returned an unexpected error: %v", err)
	}

	// --- Assert ---
	expectedCommand := "clear table fallback_table_ipv4 key 192.0.2.20"
	if commandReceived != expectedCommand {
		t.Errorf("Expected command to target fallback table '%s', but got: '%s'",
			expectedCommand, commandReceived)
	}
}

// TestExecuteHAProxyCommandImpl_ConnectError tests a failure during the connection attempt (e.g., connection refused).
// The test verifies the retry mechanism is used and the final error reflects the connection failure.
func TestExecuteHAProxyCommandImpl_ConnectError(t *testing.T) {
	addr := "127.0.0.1:65535"
	processor := &Processor{
		Config: &AppConfig{
			HAProxyMaxRetries:  2,
			HAProxyRetryDelay:  1 * time.Millisecond,
			HAProxyDialTimeout: 100 * time.Millisecond,
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {},
	}

	err := executeCommandImpl(processor, addr, "192.0.2.1", "set table foo key bar\n")
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
	// Start a server that closes the connection immediately after accept.
	_, addr, closeFn := startMockServer(t, func(conn net.Conn) {
		// Close the connection before the client can write.
		conn.Close()
	})
	defer closeFn()
	processor := &Processor{
		Config: &AppConfig{
			HAProxyMaxRetries:  2,
			HAProxyRetryDelay:  1 * time.Millisecond,
			HAProxyDialTimeout: 100 * time.Millisecond,
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {},
	}

	err := executeCommandImpl(processor, addr, "192.0.2.1", "set table foo key bar\n")
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

	processor := &Processor{
		Config: &AppConfig{
			HAProxyMaxRetries:  1,
			HAProxyRetryDelay:  1 * time.Millisecond,
			HAProxyDialTimeout: 100 * time.Millisecond,
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {},
	}
	// Act: Execute a command (retries are internal to the function)
	err := executeCommandImpl(processor, addr, "192.0.2.1", "set table foo key bar\n")

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

// TestExecuteHAProxyCommandImpl_RetryLogging verifies that a retry attempt is correctly logged.
func TestExecuteHAProxyCommandImpl_RetryLogging(t *testing.T) {
	// --- Setup ---
	// Use a channel to signal when the server should start listening.
	startListening := make(chan bool, 1)
	var listener net.Listener
	var serverErr error

	// Start a goroutine that will create the listener only after being signaled.
	go func() {
		<-startListening // Wait for the signal
		listener, serverErr = net.Listen("tcp", "127.0.0.1:0")
		if serverErr != nil {
			return
		}
		// Accept one connection and handle it.
		conn, _ := listener.Accept()
		if conn != nil {
			conn.Close()
		}
	}()

	// Capture log output
	var capturedLog string
	logReceived := make(chan bool, 1)
	processor := &Processor{
		Config: &AppConfig{
			HAProxyMaxRetries:  2, // Allow at least one retry
			HAProxyRetryDelay:  1 * time.Millisecond,
			HAProxyDialTimeout: 100 * time.Millisecond,
		},
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {
			if tag == "HAPROXY_RETRY" {
				capturedLog = fmt.Sprintf(format, args...)
				logReceived <- true
			}
		},
	}

	// --- Act ---
	// The first connection attempt will fail because the server isn't listening yet.
	// Then we signal the server to start, so the retry will succeed.
	startListening <- true
	// We need the address *after* the listener is created.
	time.Sleep(20 * time.Millisecond) // Give the server goroutine time to start listening.
	if serverErr != nil {
		t.Fatalf("Mock server failed to start: %v", serverErr)
	}
	defer listener.Close()
	addr := listener.Addr().String()

	// This call will trigger the retry logic.
	executeCommandImpl(processor, addr, "192.0.2.1", "test command\n")

	// --- Assert ---
	<-logReceived // Wait for the retry log to be captured.
	if !strings.Contains(capturedLog, "Retrying HAProxy command") {
		t.Errorf("Expected a 'HAPROXY_RETRY' log message, but got: '%s'", capturedLog)
	}
}
