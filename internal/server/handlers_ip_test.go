package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"bot-detector/internal/blocker"
	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
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
	activityStore   map[store.Actor]*store.ActorActivity
	activityMutex   *sync.RWMutex
	nodeName        string
	nodeRole        string
	leaderAddr      string
	blocker         interface{}
	clusterNodes    interface{}
	clusterProtocol string
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
	if m.clusterProtocol != "" {
		return m.clusterProtocol
	}
	return "http"
}

func (m *mockIPProvider) GetClusterNodes() interface{} {
	return m.clusterNodes
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
func (m *mockIPProvider) GenerateMetricsReport() string      { return "" }
func (m *mockIPProvider) GenerateStepsMetricsReport() string { return "" }
func (m *mockIPProvider) GetMarshalledConfig() ([]byte, time.Time, error) {
	return nil, time.Time{}, nil
}
func (m *mockIPProvider) GetNodeStatus() NodeStatus                         { return NodeStatus{} }
func (m *mockIPProvider) GetMetricsSnapshot() MetricsSnapshot               { return MetricsSnapshot{} }
func (m *mockIPProvider) GetAggregatedMetrics() interface{}                 { return nil }
func (m *mockIPProvider) GetPersistenceState(ip string) (interface{}, bool) { return nil, false }
func (m *mockIPProvider) RemoveFromPersistence(ip string) error             { return nil }
func (m *mockIPProvider) GetIPStates() map[string]persistence.IPState {
	return make(map[string]persistence.IPState)
}
func (m *mockIPProvider) GetIPStatesModifiedSince(since time.Time) map[string]persistence.IPState {
	return make(map[string]persistence.IPState)
}
func (m *mockIPProvider) GetPersistenceMutex() *sync.Mutex { return &sync.Mutex{} }
func (m *mockIPProvider) GetClusterConfig() interface{}    { return nil }
func (m *mockIPProvider) GetStateSyncConfig() (bool, bool, time.Duration, bool) {
	return false, false, 0, false
}

func (m *mockIPProvider) GetStateSyncManager() interface{} {
	return nil
}
func (m *mockIPProvider) GetBadActorInfo(ip string) (interface{}, interface{})    { return nil, nil }
func (m *mockIPProvider) GetAllBadActors() ([]interface{}, error)                 { return nil, nil }
func (m *mockIPProvider) GetBadActorsPromotedSince(since time.Time) ([]interface{}, error) { return nil, nil }
func (m *mockIPProvider) RemoveBadActorsByReason(reason string) ([]string, error) { return nil, nil }
func (m *mockIPProvider) GetBlockedIPsByReason(reason string) ([]string, error)   { return nil, nil }
func (m *mockIPProvider) GetBadActorsThreshold() float64                          { return 0 }
func (m *mockIPProvider) GetRecentParseErrors() []string                          { return nil }

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

func TestIPLookupHandler_FollowerForwarding(t *testing.T) {
	// Setup mock leader server
	leaderResponse := "cluster_status: unknown\nnodes:\n  - name: leader\n    status: unknown\n"
	leaderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ip/192.168.1.1" {
			t.Errorf("Expected /ip/192.168.1.1, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(leaderResponse))
	}))
	defer leaderServer.Close()

	p := &mockIPProvider{
		activityStore:   make(map[store.Actor]*store.ActorActivity),
		activityMutex:   &sync.RWMutex{},
		nodeRole:        "follower",
		leaderAddr:      leaderServer.URL[7:], // Remove http://
		clusterProtocol: "http",
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
	if !strings.Contains(body, "cluster_status") {
		t.Errorf("Expected cluster response from leader, got: %s", body)
	}
}

func TestAPIIPLookupHandler_FollowerForwarding(t *testing.T) {
	// Setup mock leader server
	leaderResponse := ClusterIPAggregateResponse{
		ClusterStatus: "unknown",
		Nodes: []NodeIPStatusResponse{
			{Name: "leader", Status: "unknown"},
		},
	}
	leaderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ip/192.168.1.1" {
			t.Errorf("Expected /api/v1/ip/192.168.1.1, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(leaderResponse)
	}))
	defer leaderServer.Close()

	p := &mockIPProvider{
		activityStore:   make(map[store.Actor]*store.ActorActivity),
		activityMutex:   &sync.RWMutex{},
		nodeRole:        "follower",
		leaderAddr:      leaderServer.URL[7:], // Remove http://
		clusterProtocol: "http",
	}

	handler := apiIPLookupHandler(p)
	req := httptest.NewRequest("GET", "/api/v1/ip/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}

	var response ClusterIPAggregateResponse
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON: %v", err)
	}

	if response.ClusterStatus != "unknown" {
		t.Errorf("Expected cluster_status unknown, got %s", response.ClusterStatus)
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

	// Internal endpoint should not include node name
	if response.Node != "" {
		t.Errorf("Expected empty node name in internal response, got: %s", response.Node)
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

func (m *mockBlocker) Block(ipInfo utils.IPInfo, duration time.Duration, reason string) error {
	return nil
}

func (m *mockBlocker) Unblock(ipInfo utils.IPInfo, reason string) error {
	return nil
}

func (m *mockBlocker) IsIPBlocked(ipInfo utils.IPInfo) (bool, error) {
	return false, nil
}

func (m *mockBlocker) DumpBackends() ([]string, error) {
	return nil, nil
}

func (m *mockBlocker) CompareHAProxyBackends(expTolerance time.Duration) ([]blocker.SyncDiscrepancy, error) {
	return nil, nil
}

func (m *mockBlocker) GetCurrentState() (map[string]int, error) {
	return nil, nil
}

func (m *mockBlocker) Shutdown() {
}

func TestUnblockIPHandler_Success(t *testing.T) {
	blocker := &mockBlocker{}
	p := &mockIPProvider{
		activityStore: make(map[store.Actor]*store.ActorActivity),
		activityMutex: &sync.RWMutex{},
		blocker:       blocker,
	}

	handler := clearIPHandler(p)
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

	handler := clearIPHandler(p)
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

	handler := clearIPHandler(p)
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

	handler := clearIPHandler(p)
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

// TestForwardToLeader tests the forwardToLeader helper function
func TestForwardToLeader(t *testing.T) {
	// Setup test server to act as leader
	leaderResponse := "leader response"
	leaderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("Expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/test/path" {
			t.Errorf("Expected /test/path, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(leaderResponse))
	}))
	defer leaderServer.Close()

	provider := &mockIPProvider{
		nodeRole:        "follower",
		leaderAddr:      leaderServer.URL[7:], // Remove http://
		clusterProtocol: "http",
	}

	resp, err := forwardToLeader(provider, "DELETE", "/test/path", nil)
	if err != nil {
		t.Fatalf("forwardToLeader failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != leaderResponse {
		t.Errorf("Expected %q, got %q", leaderResponse, string(body))
	}
}

// TestForwardToLeader_NoLeader tests error handling when leader is not configured
func TestForwardToLeader_NoLeader(t *testing.T) {
	provider := &mockIPProvider{
		nodeRole:        "follower",
		leaderAddr:      "",
		clusterProtocol: "http",
	}

	_, err := forwardToLeader(provider, "DELETE", "/test/path", nil)
	if err == nil {
		t.Fatal("Expected error when leader address is empty")
	}
	if !strings.Contains(err.Error(), "leader address not configured") {
		t.Errorf("Expected 'leader address not configured' error, got: %v", err)
	}
}

// TestBroadcastToFollowers tests the broadcastToFollowers helper function
func TestBroadcastToFollowers(t *testing.T) {
	receivedRequests := make(map[string]int)
	var mu sync.Mutex

	// Setup test servers to act as followers
	follower1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedRequests["follower1"]++
		mu.Unlock()
		if r.Method != "POST" {
			t.Errorf("Expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/test/broadcast" {
			t.Errorf("Expected /test/broadcast, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer follower1.Close()

	follower2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedRequests["follower2"]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer follower2.Close()

	// Mock cluster nodes
	nodes := []struct {
		Name    string
		Address string
	}{
		{Name: "leader", Address: "localhost:8080"},
		{Name: "follower1", Address: follower1.URL[7:]},
		{Name: "follower2", Address: follower2.URL[7:]},
	}

	provider := &mockIPProvider{
		nodeName:        "leader",
		clusterNodes:    nodes,
		clusterProtocol: "http",
	}

	broadcastToFollowers(provider, "POST", "/test/broadcast", nil)

	// Wait for async broadcasts to complete
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if receivedRequests["follower1"] != 1 {
		t.Errorf("Expected follower1 to receive 1 request, got %d", receivedRequests["follower1"])
	}
	if receivedRequests["follower2"] != 1 {
		t.Errorf("Expected follower2 to receive 1 request, got %d", receivedRequests["follower2"])
	}
}

// TestBroadcastToFollowers_SkipsSelf tests that broadcast skips the current node
func TestBroadcastToFollowers_SkipsSelf(t *testing.T) {
	receivedRequests := 0
	var mu sync.Mutex

	selfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedRequests++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer selfServer.Close()

	nodes := []struct {
		Name    string
		Address string
	}{
		{Name: "node1", Address: selfServer.URL[7:]},
	}

	provider := &mockIPProvider{
		nodeName:        "node1",
		clusterNodes:    nodes,
		clusterProtocol: "http",
	}

	broadcastToFollowers(provider, "POST", "/test", nil)

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if receivedRequests != 0 {
		t.Errorf("Expected node to skip itself, but received %d requests", receivedRequests)
	}
}

func (m *mockIPProvider) GenerateWebsiteStatsReport() string {
	return ""
}
