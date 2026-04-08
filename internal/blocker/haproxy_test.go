package blocker_test

import (
	"bot-detector/internal/blocker"
	"bot-detector/internal/logging"
	"bot-detector/internal/utils" // Added for IPInfo
	"bufio"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockHAProxyProvider implements the HAProxyProvider interface for testing.
type mockHAProxyProvider struct {
	blockerAddresses       []string
	durationTables         map[time.Duration]string
	blockTableNameFallback string
	blockerMaxRetries      int
	blockerRetryDelay      time.Duration
	blockerDialTimeout     time.Duration
	retries                atomic.Int32
	cmdsPerBlocker         sync.Map
	logs                   []string
	logMutex               sync.Mutex
	// For CompareHAProxyBackends tests
	mockHAProxyResponses map[string]map[string]string // addr -> command -> response
}

func (m *mockHAProxyProvider) Log(level logging.LogLevel, tag string, format string, v ...interface{}) {
	m.logMutex.Lock()
	defer m.logMutex.Unlock()
	m.logs = append(m.logs, fmt.Sprintf(format, v...))
}
func (m *mockHAProxyProvider) GetBlockerAddresses() []string { return m.blockerAddresses }
func (m *mockHAProxyProvider) GetDurationTables() map[time.Duration]string {
	return m.durationTables
}
func (m *mockHAProxyProvider) GetBlockTableNameFallback() string    { return m.blockTableNameFallback }
func (m *mockHAProxyProvider) GetBlockerMaxRetries() int            { return m.blockerMaxRetries }
func (m *mockHAProxyProvider) GetBlockerRetryDelay() time.Duration  { return m.blockerRetryDelay }
func (m *mockHAProxyProvider) GetBlockerDialTimeout() time.Duration { return m.blockerDialTimeout }
func (m *mockHAProxyProvider) GetMaxCommandsPerBatch() int          { return 500 }
func (m *mockHAProxyProvider) IncrementBlockerRetries()             { m.retries.Add(1) }
func (m *mockHAProxyProvider) IncrementCmdsPerBlocker(addr string) {
	val, _ := m.cmdsPerBlocker.LoadOrStore(addr, new(atomic.Int32))
	val.(*atomic.Int32).Add(1)
}
func (m *mockHAProxyProvider) IncrementBackendResyncs()    {}
func (m *mockHAProxyProvider) IncrementBackendRestarts()   {}
func (m *mockHAProxyProvider) IncrementBackendRecoveries() {}

func newMockHAProxyProvider() *mockHAProxyProvider {
	return &mockHAProxyProvider{
		blockerAddresses:     []string{"127.0.0.1:9999"},
		durationTables:       make(map[time.Duration]string),
		blockerMaxRetries:    1,
		blockerRetryDelay:    1 * time.Millisecond,
		blockerDialTimeout:   100 * time.Millisecond,
		mockHAProxyResponses: make(map[string]map[string]string),
	}
}

// haproxyTestHarness encapsulates the setup for HAProxy blocker tests.
type haproxyTestHarness struct {
	t            *testing.T
	mockProvider *mockHAProxyProvider
	blocker      *blocker.HAProxyBlocker
	mu           sync.Mutex
	commands     []string
}

func newHAProxyTestHarness(t *testing.T) *haproxyTestHarness {
	t.Helper()
	h := &haproxyTestHarness{t: t}
	h.mockProvider = newMockHAProxyProvider()
	h.blocker = blocker.NewHAProxyBlocker(h.mockProvider, false)

	// Override the executor to capture commands instead of making network calls.
	h.blocker.Executor = func(addr, ip, command string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.commands = append(h.commands, strings.TrimSpace(command))
		return nil
	}
	return h
}

func TestHAProxyBlocker_Block(t *testing.T) {
	h := newHAProxyTestHarness(t)
	h.mockProvider.durationTables[10*time.Minute] = "table_10m"

	ipInfo := utils.NewIPInfo("192.0.2.1")
	duration := 10 * time.Minute

	err := h.blocker.Block(ipInfo, duration, "test-reason")
	if err != nil {
		t.Fatalf("Block() failed unexpectedly: %v", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(h.commands))
	}
	expectedCmd := "set table table_10m_ipv4 key 192.0.2.1 data.gpc0 1"
	if h.commands[0] != expectedCmd {
		t.Errorf("Expected command '%s', got '%s'", expectedCmd, h.commands[0])
	}
}

func TestHAProxyBlocker_Unblock(t *testing.T) {
	h := newHAProxyTestHarness(t)

	// Simulate HAProxy reporting these tables via "show table"
	h.blocker.ExecuteHAProxyCommandFunc = func(addr, command string) (string, error) {
		return "# table: table_10m_ipv4\n# table: table_10m_ipv6\n# table: table_long_ipv4\n# table: table_long_ipv6\n", nil
	}

	ipInfo := utils.NewIPInfo("192.0.2.1")

	err := h.blocker.Unblock(ipInfo, "test-unblock")
	if err != nil {
		t.Fatalf("Unblock() failed unexpectedly: %v", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Commands are now batched with semicolons
	if len(h.commands) != 1 {
		t.Fatalf("Expected 1 batched command, got %d", len(h.commands))
	}

	// Check that both ipv4 commands are in the batched command
	batchedCmd := h.commands[0]
	expectedCmds := []string{
		"set table table_10m_ipv4 key 192.0.2.1 data.gpc0 0",
		"set table table_long_ipv4 key 192.0.2.1 data.gpc0 0",
	}

	for _, expected := range expectedCmds {
		if !strings.Contains(batchedCmd, expected) {
			t.Errorf("Batched command missing expected command '%s'. Got: %s", expected, batchedCmd)
		}
	}

	// Verify no ipv6 tables were targeted for an ipv4 address
	if strings.Contains(batchedCmd, "ipv6") {
		t.Errorf("Batched command should not contain ipv6 tables for an ipv4 address. Got: %s", batchedCmd)
	}
}

func TestHAProxyBlocker_Block_Fallback(t *testing.T) {
	h := newHAProxyTestHarness(t)
	h.mockProvider.durationTables[5*time.Minute] = "table_5m"
	h.mockProvider.blockTableNameFallback = "table_fallback"

	ipInfo := utils.NewIPInfo("192.0.2.5")
	unconfiguredDuration := 30 * time.Minute

	err := h.blocker.Block(ipInfo, unconfiguredDuration, "fallback-reason")
	if err != nil {
		t.Fatalf("Block() failed unexpectedly: %v", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(h.commands))
	}
	expectedCmd := "set table table_fallback_ipv4 key 192.0.2.5 data.gpc0 1"
	if h.commands[0] != expectedCmd {
		t.Errorf("Expected command to use fallback table '%s', but got: '%s'", expectedCmd, h.commands[0])
	}
}

func TestHAProxyBlocker_ErrorTolerance(t *testing.T) {
	h := newHAProxyTestHarness(t)
	h.mockProvider.blockerAddresses = []string{"working:9999", "failing:9999"}

	// Simulate HAProxy reporting this table via "show table"
	h.blocker.ExecuteHAProxyCommandFunc = func(addr, command string) (string, error) {
		return "# table: table_1m_ipv6\n", nil
	}

	// Override executor to simulate one failure.
	h.blocker.Executor = func(addr, ip, command string) error {
		if strings.HasPrefix(addr, "failing") {
			return fmt.Errorf("connection refused")
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.commands = append(h.commands, command)
		return nil
	}

	ipInfo := utils.NewIPInfo("2001:db8::1")

	err := h.blocker.Unblock(ipInfo, "tolerance-test")
	if err == nil {
		t.Fatal("Expected an error due to a failed instance, but got nil.")
	}
	if !strings.Contains(err.Error(), "1 HAProxy 'unblock' commands failed") {
		t.Errorf("Expected error message about 1 failure, but got: %v", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.commands) != 1 {
		t.Errorf("Expected 1 successful command, but got %d", len(h.commands))
	}
}

// --- Low-Level Network Tests for executeCommandImpl ---

// startMockServer sets up a temporary TCP server on a random port for testing.
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
			return // Expected when the listener is closed.
		}
		defer func() {
			_ = conn.Close()
		}()
		handler(conn)
	}()

	return l, addr, func() {
		_ = l.Close()
		wg.Wait()
	}
}

func TestExecuteCommandImpl_Success(t *testing.T) {
	// Start a server that returns an empty response (success).
	_, addr, closeFn := startMockServer(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		_, _ = reader.ReadString('\n')
		_, _ = conn.Write([]byte(" \n")) // Success response
	})
	defer closeFn()

	h := newNetworkTestHarness(t)
	h.mockProvider.blockerAddresses = []string{addr}

	// We are testing the real implementation via the Executor field.
	err := h.blocker.Executor(addr, "192.0.2.1", "test command\n")
	if err != nil {
		t.Fatalf("Expected success, got error: %v", err)
	}
}

func TestExecuteCommandImpl_HAProxyError(t *testing.T) {
	// Start a server that returns a specific HAProxy error message.
	_, addr, closeFn := startMockServer(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		_, _ = reader.ReadString('\n')
		_, _ = conn.Write([]byte("No such table: foo\n"))
	})
	defer closeFn()

	h := newNetworkTestHarness(t)
	h.mockProvider.blockerAddresses = []string{addr}

	err := h.blocker.Executor(addr, "192.0.2.1", "test command\n")
	if err == nil {
		t.Fatal("Expected an HAProxy command execution error, got nil")
	}
	expectedErrMsg := "HAProxy command execution failed for IP 192.0.2.1 (Response: No such table: foo)"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Error mismatch.\nExpected: %s\nGot: %v", expectedErrMsg, err)
	}
}

func TestExecuteCommandImpl_ConnectErrorWithRetry(t *testing.T) {
	// Use an unroutable address to guarantee a connection error.
	addr := "127.0.0.1:65535"

	h := newNetworkTestHarness(t)
	h.mockProvider.blockerAddresses = []string{addr}
	h.mockProvider.blockerMaxRetries = 3 // Set retries for the test

	err := h.blocker.Executor(addr, "192.0.2.1", "test command\n")

	if err == nil {
		t.Fatal("Expected a connection error after retries, got nil")
	}
	expectedErrMsg := "failed to connect to HAProxy instance"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Error mismatch.\nExpected: %s\nGot: %v", expectedErrMsg, err)
	}

	// Verify that retries were attempted.
	if h.mockProvider.retries.Load() != int32(h.mockProvider.blockerMaxRetries-1) {
		t.Errorf("Expected %d retries, but got %d", h.mockProvider.blockerMaxRetries-1, h.mockProvider.retries.Load())
	}
}

func TestExecuteCommandImpl_WriteError(t *testing.T) {
	// Start a server that closes the connection immediately after accept.
	_, addr, closeFn := startMockServer(t, func(conn net.Conn) {
		_ = conn.Close()
	})
	defer closeFn()

	h := newNetworkTestHarness(t)
	h.mockProvider.blockerAddresses = []string{addr}

	err := h.blocker.Executor(addr, "192.0.2.1", "test command\n")
	if err == nil {
		t.Fatal("Expected an error after retries, got nil")
	}
}

// newNetworkTestHarness creates a harness specifically for testing the network implementation.
// It does NOT override the command executor.
func newNetworkTestHarness(t *testing.T) *haproxyTestHarness {
	t.Helper()
	h := &haproxyTestHarness{t: t}
	h.mockProvider = newMockHAProxyProvider()
	h.blocker = blocker.NewHAProxyBlocker(h.mockProvider, false)
	return h
}

// setupMockHAProxyForComparison configures a mock HAProxy environment for comparison tests.
func setupMockHAProxyForComparison(t *testing.T, addresses []string, mockResponses map[string]map[string]string) *blocker.HAProxyBlocker {
	t.Helper()
	mockP := newMockHAProxyProvider()
	mockP.blockerAddresses = addresses
	mockP.mockHAProxyResponses = mockResponses

	b := blocker.NewHAProxyBlocker(mockP, false)
	b.Executor = func(addr, ip, command string) error {
		if responses, ok := mockP.mockHAProxyResponses[addr]; ok {
			if response, ok := responses[strings.TrimSpace(command)]; ok {
				if strings.HasPrefix(response, "ERROR:") {
					return fmt.Errorf("%s", strings.TrimPrefix(response, "ERROR:"))
				}
				// Simulate HAProxy response by writing to a buffer and reading from it.
				// This is a bit of a hack but allows the parsing logic to be tested.
				// The actual executeHAProxyCommand expects a newline-terminated response.
				return fmt.Errorf("HAProxy command execution failed on %s (Response: %s)", addr, response)
			}
		}
		return fmt.Errorf("no mock response for addr %s, command %s", addr, command)
	}

	// Override executeHAProxyCommand to use mock responses
	b.ExecuteHAProxyCommandFunc = func(addr, command string) (string, error) {
		if responses, ok := mockP.mockHAProxyResponses[addr]; ok {
			if response, ok := responses[strings.TrimSpace(command)]; ok {
				if strings.HasPrefix(response, "ERROR:") {
					return "", fmt.Errorf("%s", strings.TrimPrefix(response, "ERROR:"))
				}
				return response, nil
			}
		}
		return "", fmt.Errorf("no mock response for addr %s, command %s", addr, command)
	}

	return b
}

func TestCompareHAProxyBackends_SingleBackend(t *testing.T) {
	addresses := []string{"127.0.0.1:8080"}
	mockResponses := map[string]map[string]string{
		"127.0.0.1:8080": {
			"show table":                 "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "0x1 key=1.1.1.1 use=0 exp=1000 gpc0=1\n",
		},
	}
	b := setupMockHAProxyForComparison(t, addresses, mockResponses)

	_, err := b.CompareHAProxyBackends(0)
	if err == nil {
		t.Fatal("Expected an error for single backend comparison, got nil")
	}
	expectedErrMsg := "at least two HAProxy addresses are required for comparison"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Expected error message '%s', got '%v'", expectedErrMsg, err)
	}
}

func TestCompareHAProxyBackends_NoDiscrepancy(t *testing.T) {
	addresses := []string{"127.0.0.1:8080", "127.0.0.1:8081"}
	mockResponses := map[string]map[string]string{
		"127.0.0.1:8080": {
			"show table":                 "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "0x1 key=1.1.1.1 use=0 exp=1000 gpc0=1\n",
		},
		"127.0.0.1:8081": {
			"show table":                 "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "0x1 key=1.1.1.1 use=0 exp=1000 gpc0=1\n",
		},
	}
	b := setupMockHAProxyForComparison(t, addresses, mockResponses)

	discrepancies, err := b.CompareHAProxyBackends(0)
	if err != nil {
		t.Fatalf("CompareHAProxyBackends failed: %v", err)
	}
	if len(discrepancies) != 0 {
		t.Errorf("Expected no discrepancies, got %d: %+v", len(discrepancies), discrepancies)
	}
}

func TestCompareHAProxyBackends_MissingEntry(t *testing.T) {
	addresses := []string{"127.0.0.1:8080", "127.0.0.1:8081"}
	mockResponses := map[string]map[string]string{
		"127.0.0.1:8080": {
			"show table":                 "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n0x1 key=1.1.1.1 use=0 exp=1000 gpc0=1\n",
		},
		"127.0.0.1:8081": {
			"show table":                 "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n", // Missing entry
		},
	}
	b := setupMockHAProxyForComparison(t, addresses, mockResponses)

	discrepancies, err := b.CompareHAProxyBackends(0)
	if err != nil {
		t.Fatalf("CompareHAProxyBackends failed: %v", err)
	}
	if len(discrepancies) != 1 {
		t.Fatalf("Expected 1 discrepancy, got %d: %+v", len(discrepancies), discrepancies)
	}

	d := discrepancies[0]
	if d.IP != "1.1.1.1" || d.TableName != "table_test_ipv4" || d.Reason != "Presence Mismatch" {
		t.Errorf("Unexpected discrepancy: %+v", d)
	}
	if d.Details["present_in"] != "127.0.0.1:8080" || d.Details["missing_in"] != "127.0.0.1:8081" {
		t.Errorf("Unexpected details for missing entry: %+v", d.Details)
	}
}

func TestCompareHAProxyBackends_Gpc0Mismatch(t *testing.T) {
	addresses := []string{"127.0.0.1:8080", "127.0.0.1:8081"}
	mockResponses := map[string]map[string]string{
		"127.0.0.1:8080": {
			"show table":                 "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n0x1 key=1.1.1.1 use=0 exp=1000 gpc0=1\n",
		},
		"127.0.0.1:8081": {
			"show table":                 "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n0x1 key=1.1.1.1 use=0 exp=1000 gpc0=0\n", // Gpc0 mismatch
		},
	}
	b := setupMockHAProxyForComparison(t, addresses, mockResponses)

	discrepancies, err := b.CompareHAProxyBackends(0)
	if err != nil {
		t.Fatalf("CompareHAProxyBackends failed: %v", err)
	}
	if len(discrepancies) != 1 {
		t.Fatalf("Expected 1 discrepancy, got %d: %+v", len(discrepancies), discrepancies)
	}

	d := discrepancies[0]
	if d.IP != "1.1.1.1" || d.TableName != "table_test_ipv4" || d.Reason != "Gpc0 Mismatch" {
		t.Errorf("Unexpected discrepancy: %+v", d)
	}
	if d.Details["gpc0_127.0.0.1:8080"] != "1" || d.Details["gpc0_127.0.0.1:8081"] != "0" {
		t.Errorf("Unexpected details for gpc0 mismatch: %+v", d.Details)
	}
}

func TestCompareHAProxyBackends_ExpirationMismatch(t *testing.T) {
	addresses := []string{"127.0.0.1:8080", "127.0.0.1:8081"}
	mockResponses := map[string]map[string]string{
		"127.0.0.1:8080": {
			"show table":                 "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n0x1 key=1.1.1.1 use=0 exp=10000 gpc0=1\n",
		},
		"127.0.0.1:8081": {
			"show table":                 "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n0x1 key=1.1.1.1 use=0 exp=5000 gpc0=1\n", // Expiration mismatch
		},
	}
	b := setupMockHAProxyForComparison(t, addresses, mockResponses)

	// Test with tolerance that should catch the mismatch
	discrepancies, err := b.CompareHAProxyBackends(1 * time.Second) // 1000ms tolerance
	if err != nil {
		t.Fatalf("CompareHAProxyBackends failed: %v", err)
	}
	if len(discrepancies) != 1 {
		t.Fatalf("Expected 1 discrepancy, got %d: %+v", len(discrepancies), discrepancies)
	}

	d := discrepancies[0]
	if d.IP != "1.1.1.1" || d.TableName != "table_test_ipv4" || d.Reason != "Expiration Mismatch" {
		t.Errorf("Unexpected discrepancy: %+v", d)
	}
	if d.DiffMillis != 5000 {
		t.Errorf("Expected DiffMillis 5000, got %d", d.DiffMillis)
	}
	if d.Details["exp_127.0.0.1:8080"] != "10000" || d.Details["exp_127.0.0.1:8081"] != "5000" || d.Details["diff_millis"] != "5000" || d.Details["tolerance_millis"] != "1000" {
		t.Errorf("Unexpected details for expiration mismatch: %+v", d.Details)
	}

	// Test with tolerance that should NOT catch the mismatch
	discrepancies, err = b.CompareHAProxyBackends(10 * time.Second) // 10000ms tolerance
	if err != nil {
		t.Fatalf("CompareHAProxyBackends failed: %v", err)
	}
	if len(discrepancies) != 0 {
		t.Errorf("Expected no discrepancies with high tolerance, got %d: %+v", len(discrepancies), discrepancies)
	}
}

func TestCompareHAProxyBackends_MixedDiscrepancies(t *testing.T) {
	// Define a local struct to avoid exporting the main one just for this test.
	type discrepancy struct {
		IP        string
		TableName string
		Reason    string
		Details   map[string]string
	}

	addresses := []string{"127.0.0.1:8080", "127.0.0.1:8081", "127.0.0.1:8082"}
	mockResponses := map[string]map[string]string{
		"127.0.0.1:8080": {
			"show table":                  "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n# table: table_other_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4":  "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n0x1 key=1.1.1.1 use=0 exp=10000 gpc0=1\n0x2 key=1.1.1.2 use=0 exp=20000 gpc0=1\n",
			"show table table_other_ipv4": "# table: table_other_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n0x3 key=2.2.2.2 use=0 exp=5000 gpc0=1\n",
		},
		"127.0.0.1:8081": {
			"show table":                 "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n0x1 key=1.1.1.1 use=0 exp=10000 gpc0=0\n0x2 key=1.1.1.2 use=0 exp=20000 gpc0=1\n", // 1.1.1.1 gpc0 mismatch
		},
		"127.0.0.1:8082": {
			"show table":                 "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "# table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n0x1 key=1.1.1.1 use=0 exp=10000 gpc0=1\n", // 1.1.1.2 missing, 2.2.2.2 missing
		},
	}
	b := setupMockHAProxyForComparison(t, addresses, mockResponses)

	discrepancies, err := b.CompareHAProxyBackends(0) // No expiration tolerance
	if err != nil {
		t.Fatalf("CompareHAProxyBackends failed: %v", err)
	}

	if len(discrepancies) != 3 {
		t.Fatalf("Expected 3 discrepancies, got %d: %+v", len(discrepancies), discrepancies)
	}

	// Use a map for order-independent checking
	expected := map[string]discrepancy{
		"1.1.1.1_Gpc0 Mismatch": {
			IP: "1.1.1.1", TableName: "table_test_ipv4", Reason: "Gpc0 Mismatch",
			Details: map[string]string{"gpc0_127.0.0.1:8080": "1", "gpc0_127.0.0.1:8081": "0"},
		},
		"1.1.1.2_Presence Mismatch": {
			IP: "1.1.1.2", TableName: "table_test_ipv4", Reason: "Presence Mismatch",
			Details: map[string]string{"present_in": "127.0.0.1:8080, 127.0.0.1:8081", "missing_in": "127.0.0.1:8082"},
		},
		"2.2.2.2_Presence Mismatch": {
			IP: "2.2.2.2", TableName: "table_other_ipv4", Reason: "Presence Mismatch",
			Details: map[string]string{"present_in": "127.0.0.1:8080", "missing_in": "127.0.0.1:8081, 127.0.0.1:8082"},
		},
	}

	for _, d := range discrepancies {
		key := fmt.Sprintf("%s_%s", d.IP, d.Reason)
		e, ok := expected[key]
		if !ok {
			t.Fatalf("Unexpected discrepancy found: %+v", d)
		}

		if d.TableName != e.TableName {
			t.Errorf("For %s, expected table '%s', got '%s'", key, e.TableName, d.TableName)
		}

		// Order-independent check for details
		for k, v := range e.Details {
			// Special handling for comma-separated fields
			if k == "present_in" || k == "missing_in" {
				gotParts := strings.Split(d.Details[k], ", ")
				expectedParts := strings.Split(v, ", ")
				sort.Strings(gotParts)
				sort.Strings(expectedParts)
				if strings.Join(gotParts, ", ") != strings.Join(expectedParts, ", ") {
					t.Errorf("For %s, detail '%s' mismatch. Expected '%s', got '%s'", key, k, v, d.Details[k])
				}
			} else if d.Details[k] != v {
				t.Errorf("For %s, detail '%s' mismatch. Expected '%s', got '%s'", key, k, v, d.Details[k])
			}
		}
		delete(expected, key) // Mark as found
	}

	if len(expected) > 0 {
		t.Errorf("Not all expected discrepancies were found. Missing: %+v", expected)
	}
}

func TestCompareHAProxyBackends_ErrorDuringCollection(t *testing.T) {
	addresses := []string{"127.0.0.1:8080", "127.0.0.1:8081"}
	mockResponses := map[string]map[string]string{
		"127.0.0.1:8080": {
			"show table":                 "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "0x1 key=1.1.1.1 use=0 exp=1000 gpc0=1\n",
		},
		"127.0.0.1:8081": {
			"show table": "ERROR:connection refused", // Simulate error fetching tables
		},
	}
	b := setupMockHAProxyForComparison(t, addresses, mockResponses)

	_, err := b.CompareHAProxyBackends(0)

	if err == nil {

		t.Fatal("Expected an error during data collection, got nil")

	}

	expectedErrMsg := "errors occurred during data collection"

	if !strings.Contains(err.Error(), expectedErrMsg) {

		t.Errorf("Expected error message '%s', got '%v'", expectedErrMsg, err)

	}

}

func TestHAProxyBlocker_DumpBackends(t *testing.T) {
	addresses := []string{"127.0.0.1:8080", "127.0.0.1:8081"}
	mockResponses := map[string]map[string]string{
		"127.0.0.1:8080": {
			"show table": "# table: table_1m_ipv4\n# table: table_1h_ipv4\n",
			"show table table_1m_ipv4": "# table: table_1m_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n0x1 key=1.1.1.1 use=0 exp=1000 gpc0=1\n" + // blocked
				"0x2 key=2.2.2.2 use=0 exp=2000 gpc0=0\n", // unblocked
			"show table table_1h_ipv4": "# table: table_1h_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n0x3 key=3.3.3.3 use=0 exp=3000 gpc0=1\n", // blocked
		},
		"127.0.0.1:8081": {
			"show table": "# table: table_1m_ipv4\n",
			"show table table_1m_ipv4": "# table: table_1m_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n0x4 key=1.1.1.1 use=0 exp=4000 gpc0=1\n" + // Duplicate, blocked
				"0x5 key=4.4.4.4 use=0 exp=5000 gpc0=1\n", // blocked
		},
	}
	b := setupMockHAProxyForComparison(t, addresses, mockResponses)

	// Act
	results, err := b.DumpBackends()
	if err != nil {
		t.Fatalf("DumpBackends() failed: %v", err)
	}

	expected := []string{
		"1.1.1.1|B",
		"2.2.2.2|U",
		"3.3.3.3|B",
		"4.4.4.4|B",
	}

	sort.Strings(results)
	sort.Strings(expected)

	if len(results) != len(expected) {
		t.Fatalf("Expected %d results, got %d: %v", len(expected), len(results), results)
	}

	for i, line := range results {
		if line != expected[i] {
			t.Errorf("Expected result '%s' at index %d, but got '%s'", expected[i], i, line)
		}
	}
}

func TestHAProxyBlocker_DumpBackends_MultipleFormats(t *testing.T) {
	addresses := []string{"127.0.0.1:8080"}
	mockResponses := map[string]map[string]string{
		"127.0.0.1:8080": {
			"show table": "# table: table_1m_ipv4\n",
			"show table table_1m_ipv4": "# table: table_1m_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n" +
				// With shard
				"0x1 shard=1 key=1.1.1.1 use=0 exp=1000 gpc0=1\n" +
				// Without shard
				"0x2 key=2.2.2.2 use=0 exp=2000 gpc0=0\n" +
				// Different order
				"0x3 use=0 exp=3000 gpc0=1 key=3.3.3.3\n",
		},
	}
	b := setupMockHAProxyForComparison(t, addresses, mockResponses)

	// Act
	results, err := b.DumpBackends()
	if err != nil {
		t.Fatalf("DumpBackends() failed: %v", err)
	}

	expected := []string{
		"1.1.1.1|B",
		"2.2.2.2|U",
		"3.3.3.3|B",
	}

	sort.Strings(results)
	sort.Strings(expected)

	if len(results) != len(expected) {
		t.Fatalf("Expected %d results, got %d: %v", len(expected), len(results), results)
	}

	for i, line := range results {
		if line != expected[i] {
			t.Errorf("Expected result '%s' at index %d, but got '%s'", expected[i], i, line)
		}
	}
}

func TestHAProxyBlocker_GetHAProxyUptime(t *testing.T) {
	tests := []struct {
		name          string
		response      string
		expectedValue int64
		expectError   bool
	}{
		{
			name:          "valid uptime",
			response:      "Name: HAProxy\nVersion: 2.4.0\nUptime_sec: 188\nCurrConns: 10\n",
			expectedValue: 188,
			expectError:   false,
		},
		{
			name:          "uptime with spaces",
			response:      "Name: HAProxy\nUptime_sec:   42  \nCurrConns: 5\n",
			expectedValue: 42,
			expectError:   false,
		},
		{
			name:          "missing uptime",
			response:      "Name: HAProxy\nVersion: 2.4.0\nCurrConns: 10\n",
			expectedValue: 0,
			expectError:   true,
		},
		{
			name:          "invalid uptime value",
			response:      "Name: HAProxy\nUptime_sec: invalid\nCurrConns: 10\n",
			expectedValue: 0,
			expectError:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockProvider := newMockHAProxyProvider()
			b := blocker.NewHAProxyBlocker(mockProvider, false)

			// Mock the ExecuteHAProxyCommandFunc
			b.ExecuteHAProxyCommandFunc = func(addr, command string) (string, error) {
				return tt.response, nil
			}

			uptime, err := b.GetHAProxyUptime("127.0.0.1:9999")

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if uptime != tt.expectedValue {
					t.Errorf("Expected uptime %d, got %d", tt.expectedValue, uptime)
				}
			}
		})
	}
}

func TestHAProxyBlocker_IsIPBlocked_DirectKeyLookup(t *testing.T) {
	tests := []struct {
		name          string
		ip            string
		tableResponse string
		keyResponse   string
		expectBlocked bool
	}{
		{
			name:          "blocked IP",
			ip:            "1.1.251.183",
			tableResponse: "# table: table_1w_ipv4\n",
			keyResponse:   "# table: table_1w_ipv4, type: ip, size:2048000, used:806504\n0x56413f5a02d8: key=1.1.251.183 use=0 exp=576608059 gpc0=1",
			expectBlocked: true,
		},
		{
			name:          "unblocked IP",
			ip:            "2.2.2.2",
			tableResponse: "# table: table_1w_ipv4\n",
			keyResponse:   "# table: table_1w_ipv4, type: ip, size:2048000, used:806504\n0x56413f5a02d8: key=2.2.2.2 use=0 exp=1000 gpc0=0",
			expectBlocked: false,
		},
		{
			name:          "IP not found",
			ip:            "3.3.3.3",
			tableResponse: "# table: table_1w_ipv4\n",
			keyResponse:   "# table: table_1w_ipv4, type: ip, size:2048000, used:806504\n",
			expectBlocked: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockProvider := newMockHAProxyProvider()
			b := blocker.NewHAProxyBlocker(mockProvider, false)

			b.ExecuteHAProxyCommandFunc = func(addr, command string) (string, error) {
				if strings.Contains(command, "show table table_1w_ipv4 key") {
					return tt.keyResponse, nil
				}
				return tt.tableResponse, nil
			}

			ipInfo := utils.NewIPInfo(tt.ip)
			blocked, err := b.IsIPBlocked(ipInfo)

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if blocked != tt.expectBlocked {
				t.Errorf("Expected blocked=%v, got %v", tt.expectBlocked, blocked)
			}
		})
	}
}

func TestHAProxyBlocker_GetIPDetails(t *testing.T) {
	tests := []struct {
		name          string
		ip            string
		tableResponse string
		keyResponse   string
		expectCount   int
		expectGpc0    int
	}{
		{
			name:          "blocked IP in table",
			ip:            "1.1.1.1",
			tableResponse: "# table: table_1h_ipv4\n",
			keyResponse:   "# table: table_1h_ipv4, type: ip, size:100000, used:1\n0x1: key=1.1.1.1 use=0 exp=3600000 gpc0=1",
			expectCount:   1,
			expectGpc0:    1,
		},
		{
			name:          "unblocked IP in table",
			ip:            "2.2.2.2",
			tableResponse: "# table: table_1h_ipv4\n",
			keyResponse:   "# table: table_1h_ipv4, type: ip, size:100000, used:1\n0x2: key=2.2.2.2 use=0 exp=1800000 gpc0=0",
			expectCount:   1,
			expectGpc0:    0,
		},
		{
			name:          "IP not found",
			ip:            "3.3.3.3",
			tableResponse: "# table: table_1h_ipv4\n",
			keyResponse:   "# table: table_1h_ipv4, type: ip, size:100000, used:0\n",
			expectCount:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockProvider := newMockHAProxyProvider()
			b := blocker.NewHAProxyBlocker(mockProvider, false)

			b.ExecuteHAProxyCommandFunc = func(addr, command string) (string, error) {
				if strings.Contains(command, "key") {
					return tt.keyResponse, nil
				}
				return tt.tableResponse, nil
			}

			ipInfo := utils.NewIPInfo(tt.ip)
			details, err := b.GetIPDetails(ipInfo)

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if len(details) != tt.expectCount {
				t.Errorf("Expected %d details, got %d", tt.expectCount, len(details))
			}

			if tt.expectCount > 0 && details[0].Gpc0 != tt.expectGpc0 {
				t.Errorf("Expected gpc0=%d, got %d", tt.expectGpc0, details[0].Gpc0)
			}
		})
	}
}
