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
	blocker       interface{}
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

func (m *mockIPProvider) GetBlocker() interface{} {
	return m.blocker
}

func (m *mockIPProvider) GetDurationTables() map[time.Duration]string {
	return map[time.Duration]string{
		1 * time.Hour:    "one_hour_blocks",
		30 * time.Minute: "thirty_min_blocks",
	}
}

// Stub implementations for other Provider methods
func (m *mockIPProvider) GetListenConfigs() interface{}                                           { return []*struct{}{} }
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

func TestIPLookupHandler_MultipleActors(t *testing.T) {
	now := time.Now()
	activityStore := make(map[store.Actor]*store.ActorActivity)

	// Same IP with different user agents
	ip := utils.NewIPInfo("192.168.1.1")
	actor1 := store.Actor{IPInfo: ip, UA: "Bot1"}
	actor2 := store.Actor{IPInfo: ip, UA: "Bot2"}

	activityStore[actor1] = &store.ActorActivity{
		IsBlocked:    true,
		BlockedUntil: now.Add(1 * time.Hour),
		SkipInfo: store.SkipInfo{
			Type:   utils.SkipTypeBlocked,
			Source: "ChainA",
		},
	}

	activityStore[actor2] = &store.ActorActivity{
		IsBlocked:    true,
		BlockedUntil: now.Add(2 * time.Hour),
		SkipInfo: store.SkipInfo{
			Type:   utils.SkipTypeBlocked,
			Source: "ChainB",
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
	if !strings.Contains(body, "actors: 2") {
		t.Errorf("Expected 'actors: 2', got: %s", body)
	}
	if !strings.Contains(body, "ChainA") || !strings.Contains(body, "ChainB") {
		t.Errorf("Expected both chains in output, got: %s", body)
	}
}

func TestIPLookupHandler_FollowerHint(t *testing.T) {
	p := &mockIPProvider{
		activityStore: make(map[store.Actor]*store.ActorActivity),
		activityMutex: &sync.RWMutex{},
		nodeRole:      "follower",
		leaderAddr:    "leader.example.com:8080",
	}

	handler := ipLookupHandler(p)
	req := httptest.NewRequest("GET", "/ip/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "note: For cluster-wide view") {
		t.Errorf("Expected follower hint, got: %s", body)
	}
	if !strings.Contains(body, "leader.example.com:8080") {
		t.Errorf("Expected leader address in hint, got: %s", body)
	}
}

func TestAPIIPLookupHandler_FollowerHint(t *testing.T) {
	p := &mockIPProvider{
		activityStore: make(map[store.Actor]*store.ActorActivity),
		activityMutex: &sync.RWMutex{},
		nodeRole:      "follower",
		leaderAddr:    "leader.example.com:8080",
	}

	handler := apiIPLookupHandler(p)
	req := httptest.NewRequest("GET", "/api/v1/ip/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	var response IPStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON: %v", err)
	}

	if response.ClusterHint == "" {
		t.Errorf("Expected cluster hint in JSON response")
	}
	if !strings.Contains(response.ClusterHint, "leader.example.com:8080") {
		t.Errorf("Expected leader address in cluster hint, got: %s", response.ClusterHint)
	}
}

func TestClusterIPAggregateHandler_NotLeader(t *testing.T) {
	p := &mockIPProvider{
		activityStore: make(map[store.Actor]*store.ActorActivity),
		activityMutex: &sync.RWMutex{},
		nodeRole:      "follower",
	}

	handler := clusterIPAggregateHandler(p)
	req := httptest.NewRequest("GET", "/cluster/ip/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("Expected status 404 on follower, got %d", rr.Code)
	}
}

func TestClusterIPLookupHandler_Internal(t *testing.T) {
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
		nodeName:      "test-node",
	}

	handler := clusterIPLookupHandler(p)
	req := httptest.NewRequest("GET", "/cluster/internal/ip/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}

	var response IPStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON: %v", err)
	}

	// Internal endpoint should not include node name or cluster hint
	if response.Node != "" {
		t.Errorf("Expected empty node name in internal response, got: %s", response.Node)
	}
	if response.ClusterHint != "" {
		t.Errorf("Expected empty cluster hint in internal response, got: %s", response.ClusterHint)
	}
	if response.Status != "blocked" {
		t.Errorf("Expected status 'blocked', got '%s'", response.Status)
	}
}

func TestIPLookupHandler_ConcurrentAccess(t *testing.T) {
	activityStore := make(map[store.Actor]*store.ActorActivity)
	actor := store.Actor{IPInfo: utils.NewIPInfo("192.168.1.1")}
	activityStore[actor] = &store.ActorActivity{
		IsBlocked:       false,
		LastRequestTime: time.Now(),
	}

	p := &mockIPProvider{
		activityStore: activityStore,
		activityMutex: &sync.RWMutex{},
	}

	handler := ipLookupHandler(p)

	// Run multiple concurrent requests
	const numRequests = 10
	done := make(chan bool, numRequests)

	for i := 0; i < numRequests; i++ {
		go func() {
			req := httptest.NewRequest("GET", "/ip/192.168.1.1", nil)
			req.SetPathValue("ip", "192.168.1.1")
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Errorf("Expected status 200, got %d", rr.Code)
			}
			done <- true
		}()
	}

	// Wait for all requests to complete
	for i := 0; i < numRequests; i++ {
		<-done
	}
}

func TestExtractNodeInfo(t *testing.T) {
	// Test with nil
	nodes := extractNodeInfo(nil)
	if nodes != nil {
		t.Errorf("Expected nil for nil input, got %v", nodes)
	}

	// Test with empty slice would require creating actual cluster.NodeConfig instances
	// which would create an import cycle. This is tested implicitly through the
	// cluster aggregation handler tests.
}

// mockBlocker implements the ClearIP interface for testing
type mockBlocker struct {
	clearCalled bool
	clearIP     string
	clearError  error
	clearResult []interface{}
}

func (m *mockBlocker) ClearIP(ipInfo utils.IPInfo) ([]interface{}, error) {
	m.clearCalled = true
	m.clearIP = ipInfo.Address
	return m.clearResult, m.clearError
}

func TestUnblockIPHandler_Success(t *testing.T) {
	blocker := &mockBlocker{}
	p := &mockIPProvider{
		activityStore: make(map[store.Actor]*store.ActorActivity),
		activityMutex: &sync.RWMutex{},
		blocker:       blocker,
	}

	handler := unblockIPHandler(p)
	req := httptest.NewRequest("DELETE", "/ip/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}

	if !blocker.clearCalled {
		t.Error("Expected ClearIP to be called")
	}

	if blocker.clearIP != "192.168.1.1" {
		t.Errorf("Expected IP 192.168.1.1, got %s", blocker.clearIP)
	}
}

func TestUnblockIPHandler_InvalidIP(t *testing.T) {
	p := &mockIPProvider{
		activityStore: make(map[store.Actor]*store.ActorActivity),
		activityMutex: &sync.RWMutex{},
	}

	handler := unblockIPHandler(p)
	req := httptest.NewRequest("DELETE", "/ip/not-an-ip", nil)
	req.SetPathValue("ip", "not-an-ip")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", rr.Code)
	}
}

func TestUnblockIPHandler_IPv6(t *testing.T) {
	blocker := &mockBlocker{}
	p := &mockIPProvider{
		activityStore: make(map[store.Actor]*store.ActorActivity),
		activityMutex: &sync.RWMutex{},
		blocker:       blocker,
	}

	handler := unblockIPHandler(p)
	req := httptest.NewRequest("DELETE", "/ip/2001:db8::1", nil)
	req.SetPathValue("ip", "2001:db8::1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}

	if blocker.clearIP != "2001:db8::1" {
		t.Errorf("Expected canonical IPv6 2001:db8::1, got %s", blocker.clearIP)
	}
}

func TestUnblockIPHandler_NoBlocker(t *testing.T) {
	activityStore := make(map[store.Actor]*store.ActorActivity)
	actor := store.Actor{IPInfo: utils.NewIPInfo("192.168.1.1")}
	activityStore[actor] = &store.ActorActivity{
		IsBlocked:    true,
		BlockedUntil: time.Now().Add(1 * time.Hour),
	}

	p := &mockIPProvider{
		activityStore: activityStore,
		activityMutex: &sync.RWMutex{},
		blocker:       nil,
	}

	handler := unblockIPHandler(p)
	req := httptest.NewRequest("DELETE", "/ip/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d", rr.Code)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{
			name:     "zero duration",
			duration: 0,
			expected: "0s",
		},
		{
			name:     "only seconds",
			duration: 45 * time.Second,
			expected: "45s",
		},
		{
			name:     "minutes and seconds",
			duration: 3*time.Minute + 30*time.Second,
			expected: "3m 30s",
		},
		{
			name:     "hours, minutes, seconds",
			duration: 2*time.Hour + 15*time.Minute + 45*time.Second,
			expected: "2h 15m 45s",
		},
		{
			name:     "days and hours",
			duration: 3*24*time.Hour + 5*time.Hour,
			expected: "3d 5h",
		},
		{
			name:     "weeks, days, hours, minutes, seconds",
			duration: 2*7*24*time.Hour + 3*24*time.Hour + 4*time.Hour + 30*time.Minute + 15*time.Second,
			expected: "2w 3d 4h 30m 15s",
		},
		{
			name:     "exactly one week",
			duration: 7 * 24 * time.Hour,
			expected: "1w",
		},
		{
			name:     "one hour exactly",
			duration: time.Hour,
			expected: "1h",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatDuration(tt.duration)
			if result != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, result)
			}
		})
	}
}
