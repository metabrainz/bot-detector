package server

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/store"
	"bot-detector/internal/types"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

type LogFunc func(level logging.LogLevel, tag string, format string, v ...interface{})

// MockProvider is a mock implementation of the Provider interface for testing.
type MockProvider struct {
	config    []byte
	modTime   time.Time
	deps      map[string]*types.FileDependency
	configDir string
	logFunc   LogFunc
}

func (m *MockProvider) GetConfigForArchive() ([]byte, time.Time, map[string]*types.FileDependency, string, error) {
	return m.config, m.modTime, m.deps, m.configDir, nil
}

func (m *MockProvider) Log(level logging.LogLevel, tag string, format string, v ...interface{}) {
	if m.logFunc != nil {
		m.logFunc(level, tag, format, v...)
	}
}

// Implement other Provider methods with dummy implementations as needed for this test.
func (m *MockProvider) GetListenConfigs() interface{}                          { return []*struct{}{} }
func (m *MockProvider) GetShutdownChannel() chan os.Signal                     { return nil }
func (m *MockProvider) GetNodeStatus() NodeStatus                              { return NodeStatus{} }
func (m *MockProvider) GetMetricsSnapshot() MetricsSnapshot                    { return MetricsSnapshot{} }
func (m *MockProvider) GetAggregatedMetrics() interface{}                      { return nil }
func (m *MockProvider) GenerateMetricsReport() string                      { return "" }
func (m *MockProvider) GenerateStepsMetricsReport() string                     { return "" }
func (m *MockProvider) GetMarshalledConfig() ([]byte, time.Time, error)        { return nil, time.Time{}, nil }
func (m *MockProvider) GetActivityStore() map[store.Actor]*store.ActorActivity { return nil }
func (m *MockProvider) GetActivityMutex() *sync.RWMutex                        { return nil }
func (m *MockProvider) GetNodeName() string                                    { return "" }
func (m *MockProvider) GetNodeRole() string                                    { return "" }
func (m *MockProvider) GetNodeLeaderAddress() string                           { return "" }
func (m *MockProvider) GetClusterNodes() interface{}                           { return nil }
func (m *MockProvider) GetClusterProtocol() string                             { return "http" }
func (m *MockProvider) GetBlocker() interface{}                                { return nil }
func (m *MockProvider) GetDurationTables() map[time.Duration]string            { return nil }
func (m *MockProvider) GetPersistenceState(ip string) (interface{}, bool)      { return nil, false }
func (m *MockProvider) RemoveFromPersistence(ip string) error                  { return nil }

func TestArchiveHandler_StableETag(t *testing.T) {
	// Common setup
	modTime := time.Now()
	logFunc := func(level logging.LogLevel, tag string, format string, v ...interface{}) {
		t.Logf("[%s] %s", tag, fmt.Sprintf(format, v...))
	}

	// Create two different dependency maps to simulate non-deterministic map iteration.
	deps1 := map[string]*types.FileDependency{
		"/data/file1.txt": {
			Content: []string{"line1", "line2"},
			CurrentStatus: &types.FileDependencyStatus{
				Status:   types.FileStatusLoaded,
				ModTime:  modTime,
				Checksum: "dummy-checksum",
				Error:    nil,
			},
		},
		"/data/file2.txt": {
			Content: []string{"abc", "def"},
			CurrentStatus: &types.FileDependencyStatus{
				Status:   types.FileStatusLoaded,
				ModTime:  modTime,
				Checksum: "dummy-checksum",
				Error:    nil,
			},
		},
	}

	deps2 := map[string]*types.FileDependency{
		"/data/file2.txt": {
			Content: []string{"abc", "def"},
			CurrentStatus: &types.FileDependencyStatus{
				Status:   types.FileStatusLoaded,
				ModTime:  modTime,
				Checksum: "dummy-checksum",
				Error:    nil,
			},
		},
		"/data/file1.txt": {
			Content: []string{"line1", "line2"},
			CurrentStatus: &types.FileDependencyStatus{
				Status:   types.FileStatusLoaded,
				ModTime:  modTime,
				Checksum: "dummy-checksum",
				Error:    nil,
			},
		},
	}

	// Create mock providers
	provider1 := &MockProvider{
		config:    []byte("main config content"),
		modTime:   modTime,
		deps:      deps1,
		configDir: "/config",
		logFunc:   logFunc,
	}

	provider2 := &MockProvider{
		config:    []byte("main config content"),
		modTime:   modTime,
		deps:      deps2,
		configDir: "/config",
		logFunc:   logFunc,
	}

	// Create handlers
	handler1 := archiveHandler(provider1)
	handler2 := archiveHandler(provider2)

	// Perform requests
	req := httptest.NewRequest("GET", "/config/archive", nil)

	rr1 := httptest.NewRecorder()
	handler1.ServeHTTP(rr1, req)

	rr2 := httptest.NewRecorder()
	handler2.ServeHTTP(rr2, req)

	// Check status codes
	if status := rr1.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}
	if status := rr2.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	// Compare ETags
	etag1 := rr1.Header().Get("ETag")
	etag2 := rr2.Header().Get("ETag")

	if etag1 == "" || etag2 == "" {
		t.Fatal("ETag header should not be empty")
	}

	if etag1 != etag2 {
		t.Errorf("ETags should be identical for same content with different map order, but got ETag1: %s, ETag2: %s", etag1, etag2)
	}

	// Now, let's test that changing content changes the ETag
	provider3 := &MockProvider{
		config:    []byte("different main config"),
		modTime:   modTime,
		deps:      deps1,
		configDir: "/config",
		logFunc:   logFunc,
	}
	handler3 := archiveHandler(provider3)
	rr3 := httptest.NewRecorder()
	handler3.ServeHTTP(rr3, req)
	etag3 := rr3.Header().Get("ETag")

	if etag1 == etag3 {
		t.Errorf("ETag should have changed when config content changed, but it remained the same: %s", etag1)
	}
}

func (m *MockProvider) GenerateWebsiteStatsReport() string {
	return ""
}
