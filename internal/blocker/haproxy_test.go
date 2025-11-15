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
func (m *mockHAProxyProvider) IncrementBlockerRetries()             { m.retries.Add(1) }
func (m *mockHAProxyProvider) IncrementCmdsPerBlocker(addr string) {
	val, _ := m.cmdsPerBlocker.LoadOrStore(addr, new(atomic.Int32))
	val.(*atomic.Int32).Add(1)
}

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

	err := h.blocker.Block(ipInfo, duration)
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
	h.mockProvider.durationTables[10*time.Minute] = "table_10m"
	h.mockProvider.blockTableNameFallback = "table_long"

	ipInfo := utils.NewIPInfo("192.0.2.1")

	err := h.blocker.Unblock(ipInfo)
	if err != nil {
		t.Fatalf("Unblock() failed unexpectedly: %v", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	expectedCmds := map[string]struct{}{
		"clear table table_10m_ipv4 key 192.0.2.1":  {},
		"clear table table_long_ipv4 key 192.0.2.1": {},
	}

	if len(h.commands) != len(expectedCmds) {
		t.Fatalf("Expected %d unblock commands, got %d", len(expectedCmds), len(h.commands))
	}

	for _, cmd := range h.commands {
		if _, ok := expectedCmds[cmd]; !ok {
			t.Errorf("Received unexpected unblock command: %s", cmd)
		}
	}
}

func TestHAProxyBlocker_Block_Fallback(t *testing.T) {
	h := newHAProxyTestHarness(t)
	h.mockProvider.durationTables[5*time.Minute] = "table_5m"
	h.mockProvider.blockTableNameFallback = "table_fallback"

	ipInfo := utils.NewIPInfo("192.0.2.5")
	unconfiguredDuration := 30 * time.Minute

	err := h.blocker.Block(ipInfo, unconfiguredDuration)
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
	h.mockProvider.durationTables[1*time.Minute] = "table_1m"

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

	err := h.blocker.Unblock(ipInfo)
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
			"show table":                 "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "0x1 key=1.1.1.1 use=0 exp=1000 gpc0=1\n",
		},
		"127.0.0.1:8081": {
			"show table":                 "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "", // Missing entry
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
			"show table":                 "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "0x1 key=1.1.1.1 use=0 exp=1000 gpc0=1\n",
		},
		"127.0.0.1:8081": {
			"show table":                 "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "0x1 key=1.1.1.1 use=0 exp=1000 gpc0=0\n", // Gpc0 mismatch
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
			"show table":                 "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "0x1 key=1.1.1.1 use=0 exp=10000 gpc0=1\n",
		},
		"127.0.0.1:8081": {
			"show table":                 "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "0x1 key=1.1.1.1 use=0 exp=5000 gpc0=1\n", // Expiration mismatch
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
	addresses := []string{"127.0.0.1:8080", "127.0.0.1:8081", "127.0.0.1:8082"}
	mockResponses := map[string]map[string]string{
		"127.0.0.1:8080": {
			"show table":                  "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\ntable: table_other_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4":  "0x1 key=1.1.1.1 use=0 exp=10000 gpc0=1\n0x2 key=1.1.1.2 use=0 exp=20000 gpc0=1\n",
			"show table table_other_ipv4": "0x3 key=2.2.2.2 use=0 exp=5000 gpc0=1\n",
		},
		"127.0.0.1:8081": {
			"show table":                 "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "0x1 key=1.1.1.1 use=0 exp=10000 gpc0=0\n0x2 key=1.1.1.2 use=0 exp=20000 gpc0=1\n", // 1.1.1.1 gpc0 mismatch
		},
		"127.0.0.1:8082": {
			"show table":                 "table: table_test_ipv4,type=ip,size=100000,expire=300000,uptime=100000\n",
			"show table table_test_ipv4": "0x1 key=1.1.1.1 use=0 exp=10000 gpc0=1\n", // 1.1.1.2 missing, 2.2.2.2 missing
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

	// Sort discrepancies for consistent checking
	sort.Slice(discrepancies, func(i, j int) bool {
		if discrepancies[i].IP != discrepancies[j].IP {
			return discrepancies[i].IP < discrepancies[j].IP
		}
		return discrepancies[i].Reason < discrepancies[j].Reason
	})

	// Check 1.1.1.1 Gpc0 Mismatch
	d1 := discrepancies[0]
	if d1.IP != "1.1.1.1" || d1.TableName != "table_test_ipv4" || d1.Reason != "Gpc0 Mismatch" {
		t.Errorf("Unexpected discrepancy 1: %+v", d1)
	}
	if d1.Details["gpc0_127.0.0.1:8080"] != "1" || d1.Details["gpc0_127.0.0.1:8081"] != "0" {
		t.Errorf("Unexpected details for d1: %+v", d1.Details)
	}

	// Check 1.1.1.2 Presence Mismatch (missing in 8082)
	d2 := discrepancies[1]
	if d2.IP != "1.1.1.2" || d2.TableName != "table_test_ipv4" || d2.Reason != "Presence Mismatch" {
		t.Errorf("Unexpected discrepancy 2: %+v", d2)
	}
	if d2.Details["present_in"] != "127.0.0.1:8080, 127.0.0.1:8081" || d2.Details["missing_in"] != "127.0.0.1:8082" {
		t.Errorf("Unexpected details for d2: %+v", d2.Details)
	}

	// Check 2.2.2.2 Presence Mismatch (missing in 8081, 8082)
	d3 := discrepancies[2]
	if d3.IP != "2.2.2.2" || d3.TableName != "table_other_ipv4" || d3.Reason != "Presence Mismatch" {
		t.Errorf("Unexpected discrepancy 3: %+v", d3)
	}
	if d3.Details["present_in"] != "127.0.0.1:8080" || d3.Details["missing_in"] != "127.0.0.1:8081, 127.0.0.1:8082" {
		t.Errorf("Unexpected details for d3: %+v", d3.Details)
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
