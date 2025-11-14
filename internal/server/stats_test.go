package server

import (
	"bot-detector/internal/logging"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// mockMetricsProvider is a mock implementation of the MetricsProvider interface for testing.
type mockMetricsProvider struct {
	listenAddr         string
	reportContent      string
	stepsReportContent string // New field for step metrics report
	shutdownCh         chan os.Signal
	readyCh            chan struct{} // Closed when the server is ready to accept connections.
	logMutex           sync.Mutex
	logs               []string
}

func newMockProvider(addr, report string) *mockMetricsProvider {
	return &mockMetricsProvider{
		listenAddr:    addr,
		reportContent: report,
		shutdownCh:    make(chan os.Signal, 1),
		readyCh:       make(chan struct{}),
	}
}

func (m *mockMetricsProvider) GetListenAddr() string {
	return m.listenAddr
}

func (m *mockMetricsProvider) GenerateHTMLMetricsReport() string {
	return m.reportContent
}

func (m *mockMetricsProvider) GenerateStepsMetricsReport() string {
	return m.stepsReportContent
}

func (m *mockMetricsProvider) GetShutdownChannel() chan os.Signal {
	return m.shutdownCh
}

func (m *mockMetricsProvider) Log(level logging.LogLevel, tag string, format string, v ...interface{}) {
	logMsg := fmt.Sprintf(format, v...)
	m.logMutex.Lock()
	m.logs = append(m.logs, logMsg)
	m.logMutex.Unlock()

	// If this is the startup message, signal that the server is ready.
	if strings.HasPrefix(logMsg, "Starting web server") {
		close(m.readyCh)
	}
}

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

	expected_string := "TEST STATS REPORT"
	mockProvider := newMockProvider(addr, expected_string)

	var wg sync.WaitGroup
	wg.Add(1)

	// --- Act 1: Start the server ---
	go func() {
		defer wg.Done()
		Start(mockProvider)
	}()

	// Wait for the server to be ready.
	select {
	case <-mockProvider.readyCh:
		// Server is ready.
	case <-time.After(2 * time.Second):
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

	// Wait for the server to be ready.
	select {
	case <-mockProvider.readyCh:
		// Server is ready.
	case <-time.After(2 * time.Second):
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

// TestServer_Disabled verifies that the server does not start if the listen address is empty.
func TestServer_Disabled(t *testing.T) {
	mockProvider := newMockProvider("", "") // Empty address

	// This call should be non-blocking and return immediately.
	Start(mockProvider)

	if len(mockProvider.logs) != 1 || !strings.Contains(mockProvider.logs[0], "HTTP server is disabled") {
		t.Errorf("Expected a 'server disabled' log message, but got: %v", mockProvider.logs)
	}
}
