package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
	"bot-detector/internal/store"
	"bot-detector/internal/types"
)

// mockProvider is a mock implementation of the Provider interface for testing.
type mockProvider struct {
	listenAddr         string
	reportContent      string
	stepsReportContent string // New field for step metrics report
	configFilePath     string // New field for the config file path
	shutdownCh         chan os.Signal
	logMutex           sync.Mutex
	logs               []string
}

func newMockProvider(addr, report string) *mockProvider {
	return &mockProvider{
		listenAddr:    addr,
		reportContent: report,
		shutdownCh:    make(chan os.Signal, 1),
	}
}

func (m *mockProvider) GetListenConfigs() interface{} {
	if m.listenAddr == "" {
		return []ListenConfig{}
	}
	// Return a slice of ListenConfig interface
	return []ListenConfig{&mockListenConfig{address: m.listenAddr}}
}

func (m *mockProvider) GenerateMetricsReport() string {
	return m.reportContent
}

func (m *mockProvider) GenerateStepsMetricsReport() string {
	return m.stepsReportContent
}

func (m *mockProvider) GetShutdownChannel() chan os.Signal {
	return m.shutdownCh
}

func (m *mockProvider) GetMarshalledConfig() ([]byte, time.Time, error) {
	var modtime time.Time
	if m.configFilePath == "" {
		return nil, modtime, errors.New("config path not set in mock")
	}
	file, err := os.Open(m.configFilePath)
	defer func() {
		_ = file.Close()
	}()
	if err == nil {
		stat, err := file.Stat()
		if err == nil {
			modtime = stat.ModTime()
			buf := make([]byte, stat.Size())
			_, err = bufio.NewReader(file).Read(buf)
			if err == nil {
				return buf, modtime, nil
			}
		}
	}
	return nil, modtime, err
}

func (m *mockProvider) GetConfigForArchive() ([]byte, time.Time, map[string]*types.FileDependency, string, error) {
	// This is a mock implementation for the archive test, not used by other tests in this file.
	return nil, time.Time{}, nil, "", nil
}

func (m *mockProvider) Log(level logging.LogLevel, tag string, format string, v ...interface{}) {
	logMsg := fmt.Sprintf(format, v...)
	m.logMutex.Lock()
	m.logs = append(m.logs, logMsg)
	m.logMutex.Unlock()
}

func (m *mockProvider) GetNodeStatus() NodeStatus {
	return NodeStatus{
		Role:          "leader",
		Name:          "test-node",
		Address:       "localhost:8080",
		LeaderAddress: "",
	}
}

func (m *mockProvider) GetMetricsSnapshot() MetricsSnapshot {
	return MetricsSnapshot{
		Timestamp: time.Now(),
		ProcessingStats: ProcessingStats{
			LinesProcessed: 100,
			EntriesChecked: 10,
		},
		ActorStats: ActorStats{},
		ChainStats: ChainStats{},
	}
}

func (m *mockProvider) GetAggregatedMetrics() interface{} {
	return nil // Mock returns nil (not a leader)
}

func (m *mockProvider) GetActivityStore() map[store.Actor]*store.ActorActivity {
	return nil
}

func (m *mockProvider) GetActivityMutex() *sync.RWMutex {
	return nil
}

func (m *mockProvider) GetNodeName() string {
	return ""
}

func (m *mockProvider) GetNodeRole() string {
	return ""
}

func (m *mockProvider) GetNodeLeaderAddress() string {
	return ""
}

func (m *mockProvider) GetClusterNodes() interface{} {
	return nil
}

func (m *mockProvider) GetClusterProtocol() string {
	return "http"
}

func (m *mockProvider) GetBlocker() interface{} {
	return nil
}

func (m *mockProvider) GetDurationTables() map[time.Duration]string {
	return nil
}

func (m *mockProvider) GetPersistenceState(ip string) (interface{}, bool) {
	return nil, false
}

func (m *mockProvider) RemoveFromPersistence(ip string) error {
	return nil
}

func (m *mockProvider) GetIPStates() map[string]persistence.IPState {
	return make(map[string]persistence.IPState)
}

func (m *mockProvider) GetPersistenceMutex() *sync.Mutex {
	return &sync.Mutex{}
}

func (m *mockProvider) GetClusterConfig() interface{} {
	return nil
}

func (m *mockProvider) GetStateSyncConfig() (bool, bool, time.Duration, bool) {
	return false, false, 0, false
}

func (m *mockProvider) GetStateSyncManager() interface{} {
	return nil
}
func (m *mockProvider) GetBadActorInfo(ip string) (interface{}, interface{})    { return nil, nil }
func (m *mockProvider) GetAllBadActors() ([]interface{}, error)                 { return nil, nil }
func (m *mockProvider) RemoveBadActorsByReason(reason string) ([]string, error) { return nil, nil }
func (m *mockProvider) GetBlockedIPsByReason(reason string) ([]string, error)   { return nil, nil }
func (m *mockProvider) GetBadActorsThreshold() float64                          { return 0 }

// TestServer_StartAndShutdown verifies the full lifecycle of the stats server.
func TestServer_StartAndShutdown(t *testing.T) {
	// --- Setup ---
	// Use a listener on port 0 to get a random free port from the OS.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to listen on a free port: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close() // Close it immediately; the server will re-bind it.

	expected_string := "/help"
	mockProvider := newMockProvider(addr, expected_string)

	var wg sync.WaitGroup
	wg.Add(1)

	// --- Act 1: Start the server ---
	go func() {
		defer wg.Done()
		Start(mockProvider)
	}()

	// Wait for the server to be ready by polling.
	var connected bool
	for i := 0; i < 20; i++ { // Poll for up to 2 seconds
		conn, dialErr := net.Dial("tcp", addr)
		if dialErr == nil {
			if closeErr := conn.Close(); closeErr != nil {
				t.Logf("Error closing connection: %v", closeErr)
			}
			connected = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !connected {
		t.Fatal("Timed out waiting for server to start.")
	}

	// --- Assert 1: Server is responding ---
	resp, err := http.Get("http://" + addr)
	if err != nil {
		t.Fatalf("Failed to make GET request to http server: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), expected_string) {
		t.Errorf("Response body did not contain the expected report. Got:\n%s", string(body))
	}

	// --- Act 2: Shutdown the server ---
	mockProvider.shutdownCh <- syscall.SIGTERM

	// Wait for the server goroutine to exit.
	wg.Wait()
}

// TestServer_StepsEndpoint verifies that the /stats/steps endpoint works correctly.
func TestServer_StepsEndpoint(t *testing.T) {
	// --- Setup ---
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to listen on a free port: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	expectedStepsReport := "STEP1: 10\nSTEP2: 5\n"
	mockProvider := newMockProvider(addr, "MAIN REPORT")
	mockProvider.stepsReportContent = expectedStepsReport // Set the steps report content

	var wg sync.WaitGroup
	wg.Add(1)

	// --- Act 1: Start the server ---
	go func() {
		defer wg.Done()
		Start(mockProvider)
	}()

	// Wait for the server to be ready by polling.
	var connected bool
	for i := 0; i < 20; i++ { // Poll for up to 2 seconds
		conn, dialErr := net.Dial("tcp", addr)
		if dialErr == nil {
			if closeErr := conn.Close(); closeErr != nil {
				t.Logf("Error closing connection: %v", closeErr)
			}
			connected = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !connected {
		t.Fatal("Timed out waiting for server to start.")
	}

	// --- Assert 1: Server is responding to /stats/steps ---
	resp, err := http.Get("http://" + addr + "/stats/steps")
	if err != nil {
		t.Fatalf("Failed to make GET request to /stats/steps: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code 200 for /stats/steps, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), expectedStepsReport) {
		t.Errorf("Response body for /stats/steps did not contain the expected report. Got:\n%s", string(body))
	}

	// --- Act 2: Shutdown the server ---
	mockProvider.shutdownCh <- syscall.SIGTERM
	wg.Wait()
}

// TestServer_ConfigEndpoint verifies the /config endpoint.
func TestServer_ConfigEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to listen on a free port: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	mockProvider := newMockProvider(addr, "")
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		Start(mockProvider)
	}()

	// Wait for the server to be ready by polling.
	var connected bool
	for i := 0; i < 20; i++ { // Poll for up to 2 seconds
		conn, dialErr := net.Dial("tcp", addr)
		if dialErr == nil {
			if closeErr := conn.Close(); closeErr != nil {
				t.Logf("Error closing connection: %v", closeErr)
			}
			connected = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !connected {
		t.Fatal("Timed out waiting for server to start.")
	}

	// --- Test Case 1: Successful config retrieval ---
	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "config.yaml")
	expectedConfig := "version: 1.0\nchains: []"
	if err := os.WriteFile(configFile, []byte(expectedConfig), 0600); err != nil {
		t.Fatalf("Failed to write temp config file: %v", err)
	}
	mockProvider.configFilePath = configFile

	resp, err := http.Get("http://" + addr + "/config")
	if err != nil {
		t.Fatalf("Failed to make GET request to /config: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code 200 for /config, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != expectedConfig {
		t.Errorf("Expected config '%s', got '%s'", expectedConfig, string(body))
	}

	// --- Test Case 2: Error retrieving config (file not found) ---
	mockProvider.configFilePath = filepath.Join(tempDir, "non-existent.yaml")

	resp, err = http.Get("http://" + addr + "/config")
	if err != nil {
		t.Fatalf("Failed to make GET request to /config: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("Expected status code 500 for /config error, got %d", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "failed to read config file") {
		t.Errorf("Expected error message, got '%s'", string(body))
	}

	mockProvider.shutdownCh <- syscall.SIGTERM
	wg.Wait()
}

// TestServer_Disabled verifies that the server does not start if the listen address is empty.
func TestServer_Disabled(t *testing.T) {
	mockProvider := newMockProvider("", "") // Empty address

	// This call should be non-blocking and return immediately.
	Start(mockProvider)

	if len(mockProvider.logs) != 1 || !strings.Contains(mockProvider.logs[0], "Server is disabled") {
		t.Errorf("Expected disabled server log message, but got: %v", mockProvider.logs)
	}
}

func (m *mockProvider) GenerateWebsiteStatsReport() string {
	return ""
}
