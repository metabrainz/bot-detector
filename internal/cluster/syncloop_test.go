package cluster

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testDB creates an in-memory SQLite database populated with the given states.
func testDB(t *testing.T, states map[string]persistence.IPState) *sql.DB {
	t.Helper()
	db, err := persistence.OpenDB("", true)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	t.Cleanup(func() { persistence.CloseDB(db) })
	for ip, state := range states {
		if err := persistence.UpsertIPState(db, ip, state.State, state.ExpireTime, state.Reason, state.ModifiedAt, state.FirstBlockedAt); err != nil {
			t.Fatalf("Failed to insert test state for %s: %v", ip, err)
		}
	}
	return db
}

func TestStateSyncManager_LeaderCollectsFromFollower(t *testing.T) {
	// Setup follower node with some blocked IPs
	followerStates := map[string]persistence.IPState{
		"1.2.3.4": {
			Reason:     "SQL-Injection",
			ExpireTime: time.Now().Add(1 * time.Hour),
		},
		"5.6.7.8": {
			Reason:     "Brute-Force",
			ExpireTime: time.Now().Add(2 * time.Hour),
		},
	}
	followerMutex := &sync.Mutex{}

	// Create mock follower HTTP server
	followerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cluster/internal/persistence/state" {
			http.NotFound(w, r)
			return
		}

		followerMutex.Lock()
		defer followerMutex.Unlock()

		resp := struct {
			Version string                         `json:"version"`
			States  map[string]persistence.IPState `json:"states"`
		}{
			Version: "v1",
			States:  followerStates,
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer followerServer.Close()

	// Setup leader
	leaderStates := map[string]persistence.IPState{
		"9.10.11.12": {
			Reason:     "Port-Scan",
			ExpireTime: time.Now().Add(3 * time.Hour),
		},
	}
	leaderMutex := &sync.Mutex{}

	config := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "leader", Address: "localhost:8080"},
			{Name: "follower", Address: followerServer.URL[7:]}, // Strip "http://"
		},
		Protocol: "http",
		StateSync: StateSyncConfig{
			Enabled:     true,
			Interval:    100 * time.Millisecond,
			Compression: false,
			Timeout:     5 * time.Second,
			Incremental: false,
		},
	}

	logFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		t.Logf("[%s] %s: "+format, append([]interface{}{level, tag}, args...)...)
	}

	syncMgr := NewStateSyncManager(
		config,
		"leader",
		"leader",
		"localhost:8080",
		testDB(t, leaderStates),
		leaderMutex,
		logFunc,
	)

	// Trigger one collection cycle
	syncMgr.collectAndCacheMergedState()

	// Verify merged state contains all IPs
	mergedStates, _ := syncMgr.GetMergedStateCache()

	if len(mergedStates) != 3 {
		t.Errorf("Expected 3 IPs in merged state, got %d", len(mergedStates))
	}

	// Check leader's own IP
	if state, ok := mergedStates["9.10.11.12"]; !ok {
		t.Error("Leader's IP not in merged state")
	} else if state.Reason != "Port-Scan (leader)" {
		t.Errorf("Expected reason 'Port-Scan (leader)', got '%s'", state.Reason)
	}

	// Check follower's IPs
	if state, ok := mergedStates["1.2.3.4"]; !ok {
		t.Error("Follower's IP 1.2.3.4 not in merged state")
	} else if state.Reason != "SQL-Injection (follower)" {
		t.Errorf("Expected reason 'SQL-Injection (follower)', got '%s'", state.Reason)
	}

	if state, ok := mergedStates["5.6.7.8"]; !ok {
		t.Error("Follower's IP 5.6.7.8 not in merged state")
	} else if state.Reason != "Brute-Force (follower)" {
		t.Errorf("Expected reason 'Brute-Force (follower)', got '%s'", state.Reason)
	}
}

func TestStateSyncManager_FollowerFetchesFromLeader(t *testing.T) {
	// Setup leader with merged state
	leaderMergedStates := map[string]persistence.IPState{
		"1.2.3.4": {
			Reason:     "SQL-Injection (follower1)",
			ExpireTime: time.Now().Add(1 * time.Hour),
		},
		"5.6.7.8": {
			Reason:     "Brute-Force (leader)",
			ExpireTime: time.Now().Add(2 * time.Hour),
		},
	}

	// Create mock leader HTTP server
	leaderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cluster/state/merged" {
			http.NotFound(w, r)
			return
		}

		resp := struct {
			Version string                         `json:"version"`
			States  map[string]persistence.IPState `json:"states"`
		}{
			Version: "v1",
			States:  leaderMergedStates,
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer leaderServer.Close()

	// Setup follower with local state
	followerStates := map[string]persistence.IPState{
		"9.10.11.12": {
			Reason:     "Port-Scan",
			ExpireTime: time.Now().Add(3 * time.Hour),
		},
	}
	followerMutex := &sync.Mutex{}

	config := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "leader", Address: leaderServer.URL[7:]}, // Strip "http://"
			{Name: "follower", Address: "localhost:8081"},
		},
		Protocol: "http",
		StateSync: StateSyncConfig{
			Enabled:     true,
			Interval:    100 * time.Millisecond,
			Compression: false,
			Timeout:     5 * time.Second,
			Incremental: false,
		},
	}

	logFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		t.Logf("[%s] %s: "+format, append([]interface{}{level, tag}, args...)...)
	}

	followerDB := testDB(t, followerStates)

	syncMgr := NewStateSyncManager(
		config,
		"follower",
		"follower",
		"localhost:8081",
		followerDB,
		followerMutex,
		logFunc,
	)

	// Trigger one fetch cycle
	syncMgr.fetchAndMergeFromLeader()

	// Verify follower now has all IPs
	followerMutex.Lock()
	allStates, _ := persistence.GetAllIPStates(followerDB)
	followerMutex.Unlock()

	if len(allStates) != 3 {
		t.Errorf("Expected 3 IPs in follower state, got %d", len(allStates))
	}

	// Check follower's own IP
	if state, ok := allStates["9.10.11.12"]; !ok {
		t.Error("Follower's own IP not in state")
	} else if state.Reason != "Port-Scan" {
		t.Errorf("Expected reason 'Port-Scan', got '%s'", state.Reason)
	}

	// Check IPs from leader
	if _, ok := allStates["1.2.3.4"]; !ok {
		t.Error("Leader's IP 1.2.3.4 not in follower state")
	}

	if _, ok := allStates["5.6.7.8"]; !ok {
		t.Error("Leader's IP 5.6.7.8 not in follower state")
	}
}

func TestStateSyncManager_ConflictResolution(t *testing.T) {
	now := time.Now()

	// Setup follower with overlapping IP
	followerStates := map[string]persistence.IPState{
		"1.2.3.4": {
			Reason:     "SQL-Injection",
			ExpireTime: now.Add(1 * time.Hour),
		},
	}
	followerMutex := &sync.Mutex{}

	// Create mock follower HTTP server
	followerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followerMutex.Lock()
		defer followerMutex.Unlock()

		resp := struct {
			Version string                         `json:"version"`
			States  map[string]persistence.IPState `json:"states"`
		}{
			Version: "v1",
			States:  followerStates,
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer followerServer.Close()

	// Setup leader with same IP but different reason and longer expiry
	leaderStates := map[string]persistence.IPState{
		"1.2.3.4": {
			Reason:     "Brute-Force",
			ExpireTime: now.Add(2 * time.Hour), // Longer expiry
		},
	}
	leaderMutex := &sync.Mutex{}

	config := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "leader", Address: "localhost:8080"},
			{Name: "follower", Address: followerServer.URL[7:]},
		},
		Protocol: "http",
		StateSync: StateSyncConfig{
			Enabled:     true,
			Interval:    100 * time.Millisecond,
			Compression: false,
			Timeout:     5 * time.Second,
			Incremental: false,
		},
	}

	logFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		t.Logf("[%s] %s: "+format, append([]interface{}{level, tag}, args...)...)
	}

	syncMgr := NewStateSyncManager(
		config,
		"leader",
		"leader",
		"localhost:8080",
		testDB(t, leaderStates),
		leaderMutex,
		logFunc,
	)

	// Trigger collection
	syncMgr.collectAndCacheMergedState()

	// Verify merged state
	mergedStates, _ := syncMgr.GetMergedStateCache()

	if len(mergedStates) != 1 {
		t.Errorf("Expected 1 IP in merged state, got %d", len(mergedStates))
	}

	state, ok := mergedStates["1.2.3.4"]
	if !ok {
		t.Fatal("IP 1.2.3.4 not in merged state")
	}

	// Should have merged reasons
	if state.Reason != "Brute-Force (leader) | SQL-Injection (follower)" {
		t.Errorf("Expected merged reason, got '%s'", state.Reason)
	}

	// Should keep longest expiry (2 hours from leader)
	expectedExpiry := now.Add(2 * time.Hour)
	if state.ExpireTime.Sub(expectedExpiry) > time.Second {
		t.Errorf("Expected expiry ~%v, got %v", expectedExpiry, state.ExpireTime)
	}
}

func TestStateSyncManager_ExpiredEntriesFiltered(t *testing.T) {
	now := time.Now()

	// Setup follower with expired and valid IPs
	followerStates := map[string]persistence.IPState{
		"1.2.3.4": {
			Reason:     "SQL-Injection",
			ExpireTime: now.Add(-1 * time.Hour), // Expired
		},
		"5.6.7.8": {
			Reason:     "Brute-Force",
			ExpireTime: now.Add(1 * time.Hour), // Valid
		},
	}
	followerMutex := &sync.Mutex{}

	// Create mock follower HTTP server
	followerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followerMutex.Lock()
		defer followerMutex.Unlock()

		resp := struct {
			Version string                         `json:"version"`
			States  map[string]persistence.IPState `json:"states"`
		}{
			Version: "v1",
			States:  followerStates,
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer followerServer.Close()

	// Setup leader with no local state
	leaderStates := make(map[string]persistence.IPState)
	leaderMutex := &sync.Mutex{}

	config := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "leader", Address: "localhost:8080"},
			{Name: "follower", Address: followerServer.URL[7:]},
		},
		Protocol: "http",
		StateSync: StateSyncConfig{
			Enabled:     true,
			Interval:    100 * time.Millisecond,
			Compression: false,
			Timeout:     5 * time.Second,
			Incremental: false,
		},
	}

	logFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		t.Logf("[%s] %s: "+format, append([]interface{}{level, tag}, args...)...)
	}

	syncMgr := NewStateSyncManager(
		config,
		"leader",
		"leader",
		"localhost:8080",
		testDB(t, leaderStates),
		leaderMutex,
		logFunc,
	)

	// Trigger collection
	syncMgr.collectAndCacheMergedState()

	// Verify only valid IP is in merged state
	mergedStates, _ := syncMgr.GetMergedStateCache()

	if len(mergedStates) != 1 {
		t.Errorf("Expected 1 IP in merged state (expired filtered), got %d", len(mergedStates))
	}

	if _, ok := mergedStates["1.2.3.4"]; ok {
		t.Error("Expired IP should not be in merged state")
	}

	if _, ok := mergedStates["5.6.7.8"]; !ok {
		t.Error("Valid IP should be in merged state")
	}
}

func TestStateSyncManager_StartStop(t *testing.T) {
	states := make(map[string]persistence.IPState)
	mutex := &sync.Mutex{}

	config := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "leader", Address: "localhost:8080"},
		},
		Protocol: "http",
		StateSync: StateSyncConfig{
			Enabled:     true,
			Interval:    50 * time.Millisecond,
			Compression: false,
			Timeout:     5 * time.Second,
			Incremental: false,
		},
	}

	logFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		t.Logf("[%s] %s: "+format, append([]interface{}{level, tag}, args...)...)
	}

	syncMgr := NewStateSyncManager(
		config,
		"leader",
		"leader",
		"localhost:8080",
		testDB(t, states),
		mutex,
		logFunc,
	)

	ctx, cancel := context.WithCancel(context.Background())

	// Start sync loop
	syncMgr.Start(ctx)

	// Let it run for a bit
	time.Sleep(150 * time.Millisecond)

	// Stop it
	cancel()

	// Give it time to stop
	time.Sleep(100 * time.Millisecond)

	// Test passes if no panic or deadlock
}

func TestStateSyncManager_IncrementalSync(t *testing.T) {
	now := time.Now()
	oldTime := now.Add(-2 * time.Hour)

	// Setup follower with old and new states
	followerStates := map[string]persistence.IPState{
		"1.2.3.4": {
			Reason:     "Old-Block",
			ExpireTime: now.Add(1 * time.Hour),
			ModifiedAt: oldTime, // Old modification
		},
		"5.6.7.8": {
			Reason:     "New-Block",
			ExpireTime: now.Add(2 * time.Hour),
			ModifiedAt: now.Add(-5 * time.Minute), // Recent modification
		},
	}
	followerMutex := &sync.Mutex{}

	lastSync := now.Add(-1 * time.Hour)

	// Create mock follower HTTP server
	followerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check for since parameter
		sinceParam := r.URL.Query().Get("since")
		if sinceParam == "" {
			t.Error("Expected 'since' parameter for incremental sync")
		}

		since, err := time.Parse(time.RFC3339, sinceParam)
		if err != nil {
			t.Errorf("Invalid since parameter: %v", err)
		}

		followerMutex.Lock()
		defer followerMutex.Unlock()

		// Filter by modification time
		filtered := make(map[string]persistence.IPState)
		for ip, state := range followerStates {
			if state.ModifiedAt.After(since) {
				filtered[ip] = state
			}
		}

		resp := struct {
			Version string                         `json:"version"`
			States  map[string]persistence.IPState `json:"states"`
		}{
			Version: "v1",
			States:  filtered,
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer followerServer.Close()

	// Setup leader
	leaderStates := make(map[string]persistence.IPState)
	leaderMutex := &sync.Mutex{}

	config := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "leader", Address: "localhost:8080"},
			{Name: "follower", Address: followerServer.URL[7:]},
		},
		Protocol: "http",
		StateSync: StateSyncConfig{
			Enabled:     true,
			Interval:    100 * time.Millisecond,
			Compression: false,
			Timeout:     5 * time.Second,
			Incremental: true, // Enable incremental sync
		},
	}

	logFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		t.Logf("[%s] %s: "+format, append([]interface{}{level, tag}, args...)...)
	}

	syncMgr := NewStateSyncManager(
		config,
		"leader",
		"leader",
		"localhost:8080",
		testDB(t, leaderStates),
		leaderMutex,
		logFunc,
	)

	// Set last sync time
	syncMgr.lastSyncTime = lastSync

	// Trigger collection
	syncMgr.collectAndCacheMergedState()

	// Verify only recent IP is in merged state
	mergedStates, _ := syncMgr.GetMergedStateCache()

	if _, ok := mergedStates["1.2.3.4"]; ok {
		t.Error("Old IP should not be in incremental sync result")
	}

	if _, ok := mergedStates["5.6.7.8"]; !ok {
		t.Error("Recent IP should be in incremental sync result")
	}
}

func TestStateSyncManager_VersionMismatch(t *testing.T) {
	// Setup follower with wrong version
	followerStates := map[string]persistence.IPState{
		"1.2.3.4": {
			Reason:     "Test-Block",
			ExpireTime: time.Now().Add(1 * time.Hour),
		},
	}
	followerMutex := &sync.Mutex{}

	versionWarningLogged := false

	// Create mock follower HTTP server with wrong version
	followerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followerMutex.Lock()
		defer followerMutex.Unlock()

		resp := struct {
			Version string                         `json:"version"`
			States  map[string]persistence.IPState `json:"states"`
		}{
			Version: "v2", // Wrong version
			States:  followerStates,
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer followerServer.Close()

	// Setup leader
	leaderStates := make(map[string]persistence.IPState)
	leaderMutex := &sync.Mutex{}

	config := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "leader", Address: "localhost:8080"},
			{Name: "follower", Address: followerServer.URL[7:]},
		},
		Protocol: "http",
		StateSync: StateSyncConfig{
			Enabled:     true,
			Interval:    100 * time.Millisecond,
			Compression: false,
			Timeout:     5 * time.Second,
			Incremental: false,
		},
	}

	logFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		if strings.Contains(msg, "Version mismatch") {
			versionWarningLogged = true
		}
		t.Logf("[%s] %s: "+format, append([]interface{}{level, tag}, args...)...)
	}

	syncMgr := NewStateSyncManager(
		config,
		"leader",
		"leader",
		"localhost:8080",
		testDB(t, leaderStates),
		leaderMutex,
		logFunc,
	)

	// Trigger collection
	syncMgr.collectAndCacheMergedState()

	// Verify warning was logged
	if !versionWarningLogged {
		t.Error("Expected version mismatch warning to be logged")
	}

	// Verify state was still merged (backward compatibility)
	mergedStates, _ := syncMgr.GetMergedStateCache()
	if _, ok := mergedStates["1.2.3.4"]; !ok {
		t.Error("State should still be merged despite version mismatch")
	}
}

func TestStateSyncManager_ModifiedAtPreserved(t *testing.T) {
	now := time.Now()
	oldModified := now.Add(-2 * time.Hour)
	newModified := now.Add(-1 * time.Hour)

	// Setup follower with newer modification time
	followerStates := map[string]persistence.IPState{
		"1.2.3.4": {
			Reason:     "SQL-Injection",
			ExpireTime: now.Add(1 * time.Hour),
			ModifiedAt: newModified, // Newer
		},
	}
	followerMutex := &sync.Mutex{}

	// Create mock follower HTTP server
	followerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followerMutex.Lock()
		defer followerMutex.Unlock()

		resp := struct {
			Version string                         `json:"version"`
			States  map[string]persistence.IPState `json:"states"`
		}{
			Version: "v1",
			States:  followerStates,
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer followerServer.Close()

	// Setup leader with older modification time for same IP
	leaderStates := map[string]persistence.IPState{
		"1.2.3.4": {
			Reason:     "Brute-Force",
			ExpireTime: now.Add(2 * time.Hour), // Longer expiry
			ModifiedAt: oldModified,            // Older
		},
	}
	leaderMutex := &sync.Mutex{}

	config := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "leader", Address: "localhost:8080"},
			{Name: "follower", Address: followerServer.URL[7:]},
		},
		Protocol: "http",
		StateSync: StateSyncConfig{
			Enabled:     true,
			Interval:    100 * time.Millisecond,
			Compression: false,
			Timeout:     5 * time.Second,
			Incremental: false,
		},
	}

	logFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		t.Logf("[%s] %s: "+format, append([]interface{}{level, tag}, args...)...)
	}

	syncMgr := NewStateSyncManager(
		config,
		"leader",
		"leader",
		"localhost:8080",
		testDB(t, leaderStates),
		leaderMutex,
		logFunc,
	)

	// Trigger collection
	syncMgr.collectAndCacheMergedState()

	// Verify ModifiedAt is the newer one
	mergedStates, _ := syncMgr.GetMergedStateCache()
	state, ok := mergedStates["1.2.3.4"]
	if !ok {
		t.Fatal("IP not in merged state")
	}

	if !state.ModifiedAt.Equal(newModified) {
		t.Errorf("Expected ModifiedAt %v, got %v", newModified, state.ModifiedAt)
	}

	// Verify ExpireTime is the longer one
	expectedExpiry := now.Add(2 * time.Hour)
	if state.ExpireTime.Sub(expectedExpiry) > time.Second {
		t.Errorf("Expected ExpireTime ~%v, got %v", expectedExpiry, state.ExpireTime)
	}
}

func TestStateSyncManager_StateFieldPreserved(t *testing.T) {
	now := time.Now()

	// Setup follower with blocked state, shorter expiry
	followerStates := map[string]persistence.IPState{
		"1.2.3.4": {
			State:      persistence.BlockStateBlocked,
			Reason:     "SQL-Injection",
			ExpireTime: now.Add(1 * time.Hour),
			ModifiedAt: now,
		},
	}
	followerMutex := &sync.Mutex{}

	// Create mock follower HTTP server
	followerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followerMutex.Lock()
		defer followerMutex.Unlock()

		resp := struct {
			Version string                         `json:"version"`
			States  map[string]persistence.IPState `json:"states"`
		}{
			Version: "v1",
			States:  followerStates,
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer followerServer.Close()

	// Setup leader with unblocked state, longer expiry
	leaderStates := map[string]persistence.IPState{
		"1.2.3.4": {
			State:      persistence.BlockStateUnblocked,
			Reason:     "Brute-Force",
			ExpireTime: now.Add(2 * time.Hour), // Longer expiry
			ModifiedAt: now,
		},
	}
	leaderMutex := &sync.Mutex{}

	config := &ClusterConfig{
		Nodes: []NodeConfig{
			{Name: "leader", Address: "localhost:8080"},
			{Name: "follower", Address: followerServer.URL[7:]},
		},
		Protocol: "http",
		StateSync: StateSyncConfig{
			Enabled:     true,
			Interval:    100 * time.Millisecond,
			Compression: false,
			Timeout:     5 * time.Second,
			Incremental: false,
		},
	}

	logFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		t.Logf("[%s] %s: "+format, append([]interface{}{level, tag}, args...)...)
	}

	syncMgr := NewStateSyncManager(
		config,
		"leader",
		"leader",
		"localhost:8080",
		testDB(t, leaderStates),
		leaderMutex,
		logFunc,
	)

	// Trigger collection
	syncMgr.collectAndCacheMergedState()

	// Verify State is from entry with longest expiry (leader's unblocked state)
	mergedStates, _ := syncMgr.GetMergedStateCache()
	state, ok := mergedStates["1.2.3.4"]
	if !ok {
		t.Fatal("IP not in merged state")
	}

	if state.State != persistence.BlockStateUnblocked {
		t.Errorf("Expected State %v (from longest expiry), got %v", persistence.BlockStateUnblocked, state.State)
	}
}
