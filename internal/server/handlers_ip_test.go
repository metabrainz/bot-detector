package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/store"
	"bot-detector/internal/types"
	"bot-detector/internal/utils"
)

func TestIPLookupHandler_Unknown(t *testing.T) {
	p := &mockIPProvider{
		activityStore: make(map[store.Actor]*store.ActorActivity),
		activityMutex: &sync.RWMutex{},
	}

	handler := ipLookupHandler(p)
	req := httptest.NewRequest("GET", "/ip/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "status: unknown") {
		t.Errorf("Expected 'status: unknown', got: %s", body)
	}
}

func TestIPLookupHandler_Blocked(t *testing.T) {
	now := time.Now()
	activityStore := make(map[store.Actor]*store.ActorActivity)
	actor := store.Actor{IPInfo: utils.NewIPInfo("192.168.1.1")}
	activityStore[actor] = &store.ActorActivity{
		IsBlocked:    true,
		BlockedUntil: now.Add(1 * time.Hour),
		SkipInfo: store.SkipInfo{
			Type:   utils.SkipTypeBlocked,
			Source: "TestChain",
		},
	}

	p := &mockIPProvider{
		activityStore: activityStore,
		activityMutex: &sync.RWMutex{},
	}

	handler := ipLookupHandler(p)
	req := httptest.NewRequest("GET", "/ip/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "status: blocked") {
		t.Errorf("Expected 'status: blocked', got: %s", body)
	}
	if !strings.Contains(body, "TestChain") {
		t.Errorf("Expected chain name 'TestChain', got: %s", body)
	}
}

func TestIPLookupHandler_Unblocked(t *testing.T) {
	now := time.Now()
	activityStore := make(map[store.Actor]*store.ActorActivity)
	actor := store.Actor{IPInfo: utils.NewIPInfo("192.168.1.1")}
	activityStore[actor] = &store.ActorActivity{
		IsBlocked:         false,
		LastRequestTime:   now.Add(-10 * time.Minute),
		LastUnblockTime:   now.Add(-5 * time.Minute),
		LastUnblockReason: "good-actor:test",
	}

	p := &mockIPProvider{
		activityStore: activityStore,
		activityMutex: &sync.RWMutex{},
	}

	handler := ipLookupHandler(p)
	req := httptest.NewRequest("GET", "/ip/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "status: unblocked") {
		t.Errorf("Expected 'status: unblocked', got: %s", body)
	}
	if !strings.Contains(body, "good-actor:test") {
		t.Errorf("Expected unblock reason, got: %s", body)
	}
}

func TestIPLookupHandler_InvalidIP(t *testing.T) {
	p := &mockIPProvider{
		activityStore: make(map[store.Actor]*store.ActorActivity),
		activityMutex: &sync.RWMutex{},
	}

	handler := ipLookupHandler(p)
	req := httptest.NewRequest("GET", "/ip/not-an-ip", nil)
	req.SetPathValue("ip", "not-an-ip")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", rr.Code)
	}
}

func TestIPLookupHandler_IPv6Canonical(t *testing.T) {
	activityStore := make(map[store.Actor]*store.ActorActivity)
	// Store with canonical form
	actor := store.Actor{IPInfo: utils.NewIPInfo("2001:db8::1")}
	activityStore[actor] = &store.ActorActivity{
		IsBlocked:       false,
		LastRequestTime: time.Now(),
	}

	p := &mockIPProvider{
		activityStore: activityStore,
		activityMutex: &sync.RWMutex{},
	}

	handler := ipLookupHandler(p)
	// Query with non-canonical form
	req := httptest.NewRequest("GET", "/ip/2001:0db8::1", nil)
	req.SetPathValue("ip", "2001:0db8::1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "status: unblocked") {
		t.Errorf("Expected to find IP with canonical form, got: %s", body)
	}
}

// mockIPProvider implements Provider interface for IP lookup tests
type mockIPProvider struct {
	activityStore map[store.Actor]*store.ActorActivity
	activityMutex *sync.RWMutex
	nodeName      string
	nodeRole      string
	leaderAddr    string
}

func (m *mockIPProvider) GetActivityStore() map[store.Actor]*store.ActorActivity {
	return m.activityStore
}

func (m *mockIPProvider) GetActivityMutex() *sync.RWMutex {
	return m.activityMutex
}

func (m *mockIPProvider) GetNodeName() string {
	return m.nodeName
}

func (m *mockIPProvider) GetNodeRole() string {
	return m.nodeRole
}

func (m *mockIPProvider) GetNodeLeaderAddress() string {
	return m.leaderAddr
}

func (m *mockIPProvider) GetClusterProtocol() string {
	return "http"
}

// Stub implementations for other Provider methods
func (m *mockIPProvider) GetListenAddr() string                                                   { return "" }
func (m *mockIPProvider) GetShutdownChannel() chan os.Signal                                      { return nil }
func (m *mockIPProvider) Log(level logging.LogLevel, tag string, format string, v ...interface{}) {}
func (m *mockIPProvider) GetConfigForArchive() ([]byte, time.Time, map[string]*types.FileDependency, string, error) {
	return nil, time.Time{}, nil, "", nil
}
func (m *mockIPProvider) GenerateHTMLMetricsReport() string  { return "" }
func (m *mockIPProvider) GenerateStepsMetricsReport() string { return "" }
func (m *mockIPProvider) GetMarshalledConfig() ([]byte, time.Time, error) {
	return nil, time.Time{}, nil
}
func (m *mockIPProvider) GetNodeStatus() NodeStatus           { return NodeStatus{} }
func (m *mockIPProvider) GetMetricsSnapshot() MetricsSnapshot { return MetricsSnapshot{} }
func (m *mockIPProvider) GetAggregatedMetrics() interface{}   { return nil }
func (m *mockIPProvider) GetClusterNodes() interface{}        { return nil }

func TestAPIIPLookupHandler_JSON(t *testing.T) {
	now := time.Now()
	activityStore := make(map[store.Actor]*store.ActorActivity)
	actor := store.Actor{IPInfo: utils.NewIPInfo("192.168.1.1")}
	activityStore[actor] = &store.ActorActivity{
		IsBlocked:    true,
		BlockedUntil: now.Add(1 * time.Hour),
		SkipInfo: store.SkipInfo{
			Type:   utils.SkipTypeBlocked,
			Source: "TestChain",
		},
	}

	p := &mockIPProvider{
		activityStore: activityStore,
		activityMutex: &sync.RWMutex{},
	}

	handler := apiIPLookupHandler(p)
	req := httptest.NewRequest("GET", "/api/v1/ip/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}

	contentType := rr.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Expected Content-Type application/json, got %s", contentType)
	}

	var response IPStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON: %v", err)
	}

	if response.Status != "blocked" {
		t.Errorf("Expected status 'blocked', got '%s'", response.Status)
	}

	if _, ok := response.Chains["TestChain"]; !ok {
		t.Errorf("Expected chain 'TestChain' in response")
	}
}

func TestAPIIPLookupHandler_InvalidIP(t *testing.T) {
	p := &mockIPProvider{
		activityStore: make(map[store.Actor]*store.ActorActivity),
		activityMutex: &sync.RWMutex{},
	}

	handler := apiIPLookupHandler(p)
	req := httptest.NewRequest("GET", "/api/v1/ip/invalid", nil)
	req.SetPathValue("ip", "invalid")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "error") {
		t.Errorf("Expected error in JSON response, got: %s", body)
	}
}
