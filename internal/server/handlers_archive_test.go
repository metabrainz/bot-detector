package server

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/types"
)

// MockProvider is a mock implementation of the Provider interface for testing.
type MockProvider struct{}

func (m *MockProvider) GetListenAddr() string                                                   { return "" }
func (m *MockProvider) GetShutdownChannel() chan os.Signal                                      { return nil }
func (m *MockProvider) Log(level logging.LogLevel, tag string, format string, v ...interface{}) {}
func (m *MockProvider) GenerateHTMLMetricsReport() string                                       { return "" }
func (m *MockProvider) GenerateStepsMetricsReport() string                                      { return "" }
func (m *MockProvider) GetMarshalledConfig() ([]byte, time.Time, error) {
	return nil, time.Time{}, nil
}
func (m *MockProvider) GetConfigForArchive() ([]byte, time.Time, map[string]*types.FileDependency, string, error) {
	return nil, time.Time{}, nil, "", nil
}
func (m *MockProvider) GetNodeStatus() NodeStatus {
	return NodeStatus{Role: "leader", Name: "", Address: "", LeaderAddress: ""}
}
func (m *MockProvider) GetMetricsSnapshot() MetricsSnapshot {
	return MetricsSnapshot{
		Timestamp:       time.Now(),
		ProcessingStats: ProcessingStats{},
		ActorStats:      ActorStats{},
		ChainStats:      ChainStats{},
	}
}
func (m *MockProvider) GetAggregatedMetrics() interface{} { return nil }

// MockFileDependency and MockProvider for testing
type MockFileDependency struct {
	Path    string
	Content []string
	ModTime time.Time
}

type MockProviderWithArchive struct {
	*MockProvider // Embed the existing mock provider
	Deps          map[string]*MockFileDependency
	ConfigPath    string
	MainModTime   time.Time
	MainContent   []byte
}

func (m *MockProviderWithArchive) GenerateHTMLMetricsReport() string  { return "" }
func (m *MockProviderWithArchive) GenerateStepsMetricsReport() string { return "" }
func (m *MockProviderWithArchive) GetMarshalledConfig() ([]byte, time.Time, error) {
	return m.MainContent, m.MainModTime, nil
}

func (m *MockProviderWithArchive) GetConfigForArchive() ([]byte, time.Time, map[string]*types.FileDependency, string, error) {
	deps := make(map[string]*types.FileDependency)
	for path, mockDep := range m.Deps {
		deps[path] = &types.FileDependency{
			Path: path,
			CurrentStatus: &types.FileDependencyStatus{
				ModTime: mockDep.ModTime,
				Status:  1, // Loaded
			},
			Content: mockDep.Content,
		}
	}
	return m.MainContent, m.MainModTime, deps, m.ConfigPath, nil
}

func TestArchiveHandler(t *testing.T) {
	// -- Setup --
	mainContent := "version: '1.0'\n"
	dep1Content := "1.2.3.4\n5.6.7.8\n"
	configPath := "/etc/bot-detector/config.yaml"

	now := time.Now()
	mainModTime := now.Add(-10 * time.Minute)
	dep1ModTime := now.Add(-5 * time.Minute) // Newer than main config

	mockProvider := &MockProviderWithArchive{
		MockProvider: &MockProvider{}, // Initialize the embedded mock
		MainContent:  []byte(mainContent),
		MainModTime:  mainModTime,
		ConfigPath:   configPath,
		Deps: map[string]*MockFileDependency{
			"/etc/bot-detector/dependency1.txt": {
				Path:    "/etc/bot-detector/dependency1.txt",
				Content: []string{"1.2.3.4", "5.6.7.8"},
				ModTime: dep1ModTime,
			},
		},
	}

	req, err := http.NewRequest("GET", "/config/archive", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(archiveHandler(mockProvider))

	// -- Execution --
	handler.ServeHTTP(rr, req)

	// -- Verification --
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	// Check headers
	if ctype := rr.Header().Get("Content-Type"); ctype != "application/gzip" {
		t.Errorf("wrong Content-Type header: got %s want application/gzip", ctype)
	}
	expectedDisposition := `attachment; filename="config_archive.tar.gz"`
	if disp := rr.Header().Get("Content-Disposition"); disp != expectedDisposition {
		t.Errorf("wrong Content-Disposition header: got %s want %s", disp, expectedDisposition)
	}
	// The Last-Modified header should be the time of the newest file (dep1)
	expectedModTime := dep1ModTime.UTC().Format(http.TimeFormat)
	if modTime := rr.Header().Get("Last-Modified"); modTime != expectedModTime {
		t.Errorf("wrong Last-Modified header: got %s want %s", modTime, expectedModTime)
	}

	// Check archive content
	gr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatalf("Failed to create gzip reader: %v", err)
	}
	defer func() {
		if err := gr.Close(); err != nil {
			t.Fatalf("Failed to close gzip reader: %v", err)
		}
	}()

	tr := tar.NewReader(gr)
	filesInArchive := make(map[string]string)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break // End of archive
		}
		if err != nil {
			t.Fatalf("Failed to read tar header: %v", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("Failed to read content of %s: %v", hdr.Name, err)
		}
		filesInArchive[hdr.Name] = string(content)
	}

	// Verify main config
	if content, ok := filesInArchive["config.yaml"]; !ok {
		t.Error("archive does not contain config.yaml")
	} else if content != mainContent {
		t.Errorf("config.yaml content mismatch: got %s want %s", content, mainContent)
	}

	// Verify dependency
	relPath, _ := filepath.Rel(filepath.Dir(configPath), "/etc/bot-detector/dependency1.txt")
	if content, ok := filesInArchive[relPath]; !ok {
		t.Errorf("archive does not contain dependency file: %s", relPath)
	} else if content != dep1Content {
		t.Errorf("dependency content mismatch: got %s want %s", content, dep1Content)
	}
}
