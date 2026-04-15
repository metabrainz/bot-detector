package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"bot-detector/internal/persistence"
	"bot-detector/internal/store"
)

// mockBadActorsProvider is a minimal mock for bad actors and help endpoint tests.
type mockBadActorsProvider struct {
	mockIPProvider
	badActors       []interface{}
	removedByReason []string
}

func (m *mockBadActorsProvider) GetAllBadActors() ([]interface{}, error) { return m.badActors, nil }
func (m *mockBadActorsProvider) RemoveBadActorsByReason(reason string) ([]string, error) {
	return m.removedByReason, nil
}

func newBadActorsProvider(actors []persistence.BadActorInfo) *mockBadActorsProvider {
	ifaces := make([]interface{}, len(actors))
	for i, a := range actors {
		ifaces[i] = a
	}
	return &mockBadActorsProvider{
		mockIPProvider: mockIPProvider{
			activityStore: make(map[store.Actor]*store.ActorActivity),
			activityMutex: &sync.RWMutex{},
		},
		badActors: ifaces,
	}
}

func TestBadActorsListHandler(t *testing.T) {
	p := newBadActorsProvider([]persistence.BadActorInfo{
		{IP: "1.2.3.4", PromotedAt: time.Now(), TotalScore: 5.0, BlockCount: 3},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/bad-actors", nil)
	badActorsListHandler(p).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}
	var result []map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("Invalid JSON: %v", err)
	}
	if len(result) != 1 || result[0]["ip"] != "1.2.3.4" {
		t.Errorf("Unexpected response: %v", result)
	}
}

func TestBadActorsExportHandler(t *testing.T) {
	p := newBadActorsProvider([]persistence.BadActorInfo{
		{IP: "1.2.3.4", PromotedAt: time.Now()},
		{IP: "5.6.7.8", PromotedAt: time.Now()},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/bad-actors/export", nil)
	badActorsExportHandler(p).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !contains(body, "1.2.3.4") || !contains(body, "5.6.7.8") {
		t.Errorf("Expected both IPs in export, got: %s", body)
	}
}

func TestBadActorsStatsHandler(t *testing.T) {
	p := newBadActorsProvider([]persistence.BadActorInfo{
		{IP: "1.2.3.4", PromotedAt: time.Now(), TotalScore: 6.0, BlockCount: 2,
			HistoryJSON: `[{"ts":"2026-01-01T00:00:00Z","r":"chain-a"}]`},
		{IP: "5.6.7.8", PromotedAt: time.Now(), TotalScore: 4.0, BlockCount: 1,
			HistoryJSON: `[{"ts":"2026-01-01T00:00:00Z","r":"chain-a"},{"ts":"2026-01-02T00:00:00Z","r":"chain-b"}]`},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/bad-actors/stats", nil)
	badActorsStatsHandler(p).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("Invalid JSON: %v", err)
	}
	if result["total"] != float64(2) {
		t.Errorf("Expected total=2, got %v", result["total"])
	}
	byReason, ok := result["by_reason"].(map[string]interface{})
	if !ok {
		t.Fatalf("by_reason not a map")
	}
	if byReason["chain-a"] != float64(2) {
		t.Errorf("Expected chain-a=2, got %v", byReason["chain-a"])
	}
	if byReason["chain-b"] != float64(1) {
		t.Errorf("Expected chain-b=1, got %v", byReason["chain-b"])
	}
}

func TestBadActorsDeleteByReasonHandler(t *testing.T) {
	p := newBadActorsProvider(nil)
	p.removedByReason = []string{"1.2.3.4", "5.6.7.8"}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/v1/bad-actors?reason=chain-a", nil)
	badActorsDeleteByReasonHandler(p).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("Invalid JSON: %v", err)
	}
	if result["removed"] != float64(2) {
		t.Errorf("Expected removed=2, got %v", result["removed"])
	}
}

func TestBadActorsDeleteByReasonHandler_MissingReason(t *testing.T) {
	p := newBadActorsProvider(nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/v1/bad-actors", nil)
	badActorsDeleteByReasonHandler(p).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d", rr.Code)
	}
}

func TestHelpHandler_Text(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/help", nil)
	helpHandler("", false).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !contains(ct, "text/plain") {
		t.Errorf("Expected text/plain, got %s", ct)
	}
	body := rr.Body.String()
	if !contains(body, "/stats") || !contains(body, "/api/v1/help") {
		t.Errorf("Help output missing expected endpoints: %s", body)
	}
}

func TestHelpHandler_JSON(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/help", nil)
	helpHandler("api", true).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !contains(ct, "application/json") {
		t.Errorf("Expected application/json, got %s", ct)
	}
	var result []map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("Invalid JSON: %v", err)
	}
	// All entries should have role=api
	for _, e := range result {
		if e["role"] != "api" {
			t.Errorf("Expected role=api, got %v", e["role"])
		}
	}
}

func TestAPIUnblockIPHandler_NoBlocker(t *testing.T) {
	p := newBadActorsProvider(nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/ip/192.168.1.1/unblock", nil)
	req.SetPathValue("ip", "192.168.1.1")
	apiUnblockIPHandler(p).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected 503, got %d", rr.Code)
	}
}

func TestAPIClearIPHandler_NoBlocker(t *testing.T) {
	p := newBadActorsProvider(nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/ip/192.168.1.1/clear", nil)
	req.SetPathValue("ip", "192.168.1.1")
	apiClearIPHandler(p).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected 503, got %d", rr.Code)
	}
}

func TestAPIClearIPHandler_InvalidIP(t *testing.T) {
	p := newBadActorsProvider(nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/ip/not-an-ip/clear", nil)
	req.SetPathValue("ip", "not-an-ip")
	apiClearIPHandler(p).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d", rr.Code)
	}
	var result map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("Expected JSON error, got: %s", rr.Body.String())
	}
	if result["error"] == "" {
		t.Error("Expected error field in JSON response")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestUnblockByReasonHandler_MissingReason(t *testing.T) {
	p := newBadActorsProvider(nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/blocks/unblock", nil)
	unblockByReasonHandler(p).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d", rr.Code)
	}
}

func TestUnblockByReasonHandler_NoMatches(t *testing.T) {
	p := newBadActorsProvider(nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/blocks/unblock?reason=nonexistent", nil)
	unblockByReasonHandler(p).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("Invalid JSON: %v", err)
	}
	if result["matched"] != float64(0) {
		t.Errorf("Expected matched=0, got %v", result["matched"])
	}
}
