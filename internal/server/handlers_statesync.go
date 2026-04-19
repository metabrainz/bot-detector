package server

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
)

const (
	// StateSyncVersion is the current state sync protocol version.
	StateSyncVersion = "v1"

	// ReasonSeparator is used to separate multiple reasons in merged states.
	ReasonSeparator = " | "
)

// clusterPersistenceStateHandler exposes the node's local IPStates for state sync.
// GET /api/v1/cluster/internal/persistence/state?since=<timestamp>
func clusterPersistenceStateHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse optional "since" parameter for incremental sync
		sinceStr := r.URL.Query().Get("since")
		var since time.Time
		incremental := false
		if sinceStr != "" {
			var err error
			since, err = time.Parse(time.RFC3339, sinceStr)
			if err != nil {
				jsonError(w, "Invalid 'since' timestamp format", http.StatusBadRequest)
				return
			}
			incremental = true
		}

		// Collect local IPStates
		p.GetPersistenceMutex().Lock()
		states := make(map[string]persistence.IPState)
		ipStates := p.GetIPStates()

		for ip, state := range ipStates {
			// Skip expired blocked entries, but keep unblocked entries (good actors)
			if state.State == persistence.BlockStateBlocked {
				if !state.ExpireTime.IsZero() && time.Now().After(state.ExpireTime) {
					continue
				}
			}
			// For incremental sync, only include modified after 'since'
			if incremental && !state.ModifiedAt.IsZero() && !state.ModifiedAt.After(since) {
				continue
			}
			states[ip] = state
		}
		p.GetPersistenceMutex().Unlock()

		// Collect bad actors
		badActors, _ := p.GetAllBadActors()
		var baList []persistence.BadActorInfo
		for _, a := range badActors {
			if ba, ok := a.(persistence.BadActorInfo); ok {
				baList = append(baList, ba)
			}
		}

		// Build response
		response := StateSyncResponse{
			Version:   StateSyncVersion,
			Timestamp: time.Now(),
			States:    states,
			BadActors: baList,
		}

		// Set content type
		w.Header().Set("Content-Type", "application/json")

		// Apply compression if requested and enabled
		_, useCompression, _, _ := p.GetStateSyncConfig()
		acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")

		if useCompression && acceptsGzip {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			defer func() { _ = gz.Close() }()
			if err := json.NewEncoder(gz).Encode(response); err != nil {
				p.Log(logging.LevelError, "STATE_SYNC", "Failed to encode state response: %v", err)
			}
		} else {
			if err := json.NewEncoder(w).Encode(response); err != nil {
				p.Log(logging.LevelError, "STATE_SYNC", "Failed to encode state response: %v", err)
			}
		}
	}
}

const mergedStateCachePrefix = "bot-detector-merged-state-"

// cachedMergedResponse holds a path to a pre-serialized gzipped response file on disk.
type cachedMergedResponse struct {
	mu        sync.RWMutex
	cacheDir  string    // directory for cache files
	gzPath    string    // path to gzipped JSON file
	cacheTime time.Time // wall clock when cached
}

var mergedResponseCache = &cachedMergedResponse{}

// InitMergedStateCache sets the cache directory and cleans up stale files.
func InitMergedStateCache(stateDir string) {
	dir := filepath.Join(stateDir, "cache")
	_ = os.MkdirAll(dir, 0755)

	// Clean up stale files from previous runs
	matches, _ := filepath.Glob(filepath.Join(dir, mergedStateCachePrefix+"*"))
	for _, f := range matches {
		_ = os.Remove(f)
	}

	mergedResponseCache.mu.Lock()
	mergedResponseCache.cacheDir = dir
	mergedResponseCache.mu.Unlock()
}

func (c *cachedMergedResponse) storeStreaming(response interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cacheDir == "" {
		return
	}

	// Stream JSON → gzip → temp file (near-zero memory)
	f, err := os.CreateTemp(c.cacheDir, mergedStateCachePrefix+"tmp-*.json.gz")
	if err != nil {
		return
	}
	tmpPath := f.Name()

	gz := gzip.NewWriter(f)
	err = json.NewEncoder(gz).Encode(response)
	_ = gz.Close()
	_ = f.Close()

	if err != nil {
		_ = os.Remove(tmpPath)
		return
	}

	finalPath := filepath.Join(c.cacheDir, mergedStateCachePrefix+"current.json.gz")
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return
	}

	if c.gzPath != "" && c.gzPath != finalPath {
		_ = os.Remove(c.gzPath)
	}

	c.gzPath = finalPath
	c.cacheTime = time.Now()
}

// clusterMergedStateHandler exposes the cluster-wide merged state (leader only).
// GET /api/v1/cluster/state/merged?since=<timestamp>
func clusterMergedStateHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only leader can provide merged state
		if p.GetNodeRole() != "leader" {
			jsonError(w, "Only leader provides merged state", http.StatusForbidden)
			return
		}

		// Parse optional "since" parameter for incremental sync
		sinceStr := r.URL.Query().Get("since")
		var since time.Time
		if sinceStr != "" {
			var err error
			since, err = time.Parse(time.RFC3339, sinceStr)
			if err != nil {
				jsonError(w, "Invalid 'since' timestamp format", http.StatusBadRequest)
				return
			}
		}

		// For incremental sync, always compute fresh (small delta)
		if !since.IsZero() {
			serveMergedStateFresh(p, w, r, since)
			return
		}

		// For full sync, serve from pre-built disk cache if still relevant
		mergedResponseCache.mu.RLock()
		gzPath := mergedResponseCache.gzPath
		cacheTime := mergedResponseCache.cacheTime
		mergedResponseCache.mu.RUnlock()

		if gzPath != "" && !cacheTime.IsZero() {
			// Check if cache is still relevant by counting changes since snapshot
			changed := 0
			total := 0
			p.GetPersistenceMutex().Lock()
			for _, state := range p.GetIPStates() {
				total++
				if state.ModifiedAt.After(cacheTime) {
					changed++
				}
			}
			p.GetPersistenceMutex().Unlock()

			// Rebuild if >10% of IPs changed since snapshot
			if total == 0 || changed <= total/10 {
				_, useCompression, _, _ := p.GetStateSyncConfig()
				acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
				w.Header().Set("Content-Type", "application/json")
				if useCompression && acceptsGzip {
					w.Header().Set("Content-Encoding", "gzip")
					serveFileContent(w, gzPath)
				} else {
					serveGzFileDecompressed(w, gzPath)
				}
				return
			}
		}

		// No cache yet (first request before sync loop runs) — compute fresh
		serveMergedStateFreshAndCache(p, w, r)
	}
}

// serveFileContent streams a file to the response writer without loading it all into memory.
func serveFileContent(w http.ResponseWriter, path string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "Cache read error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = io.Copy(w, f)
}

// serveGzFileDecompressed streams a gzipped file decompressed to the response writer.
func serveGzFileDecompressed(w http.ResponseWriter, path string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "Cache read error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		http.Error(w, "Cache decompress error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = gz.Close() }()
	_, _ = io.Copy(w, gz)
}

// serveMergedStateFresh serves a fresh (non-cached) merged state response.
func serveMergedStateFresh(p Provider, w http.ResponseWriter, r *http.Request, since time.Time) {
	merged, nodesQueried, nodesFailed := collectAndMergeStates(p, since)

	allBadActors, _ := p.GetAllBadActors()
	var baList []persistence.BadActorInfo
	for _, a := range allBadActors {
		if ba, ok := a.(persistence.BadActorInfo); ok {
			baList = append(baList, ba)
		}
	}

	response := MergedStateResponse{
		Version:      StateSyncVersion,
		Timestamp:    time.Now(),
		NodesQueried: nodesQueried,
		NodesFailed:  nodesFailed,
		States:       merged,
		BadActors:    baList,
	}

	w.Header().Set("Content-Type", "application/json")
	_, useCompression, _, _ := p.GetStateSyncConfig()
	acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")

	if useCompression && acceptsGzip {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer func() { _ = gz.Close() }()
		if err := json.NewEncoder(gz).Encode(response); err != nil {
			p.Log(logging.LevelError, "STATE_SYNC", "Failed to encode merged state response: %v", err)
		}
	} else {
		if err := json.NewEncoder(w).Encode(response); err != nil {
			p.Log(logging.LevelError, "STATE_SYNC", "Failed to encode merged state response: %v", err)
		}
	}
}

// serveMergedStateFreshAndCache builds a fresh response, caches to disk, and serves from the file.
func serveMergedStateFreshAndCache(p Provider, w http.ResponseWriter, r *http.Request) {
	merged, _, _ := collectAndMergeStates(p, time.Time{})

	allBadActors, _ := p.GetAllBadActors()
	var baList []persistence.BadActorInfo
	for _, a := range allBadActors {
		if ba, ok := a.(persistence.BadActorInfo); ok {
			baList = append(baList, ba)
		}
	}

	response := MergedStateResponse{
		Version:   StateSyncVersion,
		Timestamp: time.Now(),
		States:    merged,
		BadActors: baList,
	}

	// Stream to disk cache
	mergedResponseCache.storeStreaming(response)

	// Serve from the cached file
	mergedResponseCache.mu.RLock()
	gzPath := mergedResponseCache.gzPath
	mergedResponseCache.mu.RUnlock()

	if gzPath != "" {
		_, useCompression, _, _ := p.GetStateSyncConfig()
		acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
		w.Header().Set("Content-Type", "application/json")
		if useCompression && acceptsGzip {
			w.Header().Set("Content-Encoding", "gzip")
			serveFileContent(w, gzPath)
		} else {
			serveGzFileDecompressed(w, gzPath)
		}
		return
	}

	// Fallback: stream directly to response (no state dir configured)
	w.Header().Set("Content-Type", "application/json")
	_, useCompression, _, _ := p.GetStateSyncConfig()
	acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	if useCompression && acceptsGzip {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_ = json.NewEncoder(gz).Encode(response)
		_ = gz.Close()
	} else {
		_ = json.NewEncoder(w).Encode(response)
	}
}

// collectAndMergeStates queries all nodes and merges their IPStates.
func collectAndMergeStates(p Provider, since time.Time) (map[string]persistence.IPState, []string, []string) {
	merged := make(map[string]persistence.IPState)
	var nodesQueried []string
	var nodesFailed []string

	clusterConfigInterface := p.GetClusterConfig()
	if clusterConfigInterface == nil {
		return merged, nodesQueried, nodesFailed
	}

	// Use reflection to access cluster config fields
	v := reflect.ValueOf(clusterConfigInterface)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	nodesField := v.FieldByName("Nodes")
	protocolField := v.FieldByName("Protocol")
	stateSyncField := v.FieldByName("StateSync")

	if !nodesField.IsValid() || !protocolField.IsValid() || !stateSyncField.IsValid() {
		return merged, nodesQueried, nodesFailed
	}

	protocol := protocolField.String()
	timeout := stateSyncField.FieldByName("Timeout").Interface().(time.Duration)
	incremental := stateSyncField.FieldByName("Incremental").Bool()

	nodeName := p.GetNodeName()

	// Add leader's own state first
	p.GetPersistenceMutex().Lock()
	for ip, state := range p.GetIPStates() {
		// Skip expired blocked entries, but keep unblocked entries (good actors)
		if state.State == persistence.BlockStateBlocked {
			if !state.ExpireTime.IsZero() && time.Now().After(state.ExpireTime) {
				continue
			}
		}
		// For incremental, filter by modification time
		if incremental && !since.IsZero() && !state.ModifiedAt.IsZero() && !state.ModifiedAt.After(since) {
			continue
		}
		// Add source node to reason
		state.Reason = addSourceNode(state.Reason, nodeName, "")
		merged[ip] = state
	}
	p.GetPersistenceMutex().Unlock()
	nodesQueried = append(nodesQueried, nodeName)

	// Query each follower
	client := &http.Client{Timeout: timeout}
	for i := 0; i < nodesField.Len(); i++ {
		nodeVal := nodesField.Index(i)
		nodeNameField := nodeVal.FieldByName("Name").String()
		nodeAddressField := nodeVal.FieldByName("Address").String()

		if nodeNameField == nodeName {
			continue // Skip self
		}

		url := protocol + "://" + nodeAddressField + "/api/v1/cluster/internal/persistence/state"
		if incremental && !since.IsZero() {
			url += "?since=" + since.Format(time.RFC3339)
		}

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			p.Log(logging.LevelWarning, "STATE_MERGE", "Failed to create request for %s: %v", nodeNameField, err)
			nodesFailed = append(nodesFailed, nodeNameField)
			continue
		}

		// Request gzip compression if enabled (always request, let server decide)
		req.Header.Set("Accept-Encoding", "gzip")

		resp, err := client.Do(req)
		if err != nil {
			p.Log(logging.LevelWarning, "STATE_MERGE", "Failed to fetch state from %s: %v", nodeNameField, err)
			nodesFailed = append(nodesFailed, nodeNameField)
			continue
		}

		// Check if response is gzipped
		var reader = resp.Body
		if resp.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(resp.Body)
			if err != nil {
				p.Log(logging.LevelWarning, "STATE_MERGE", "Failed to create gzip reader for %s: %v", nodeNameField, err)
				_ = resp.Body.Close()
				nodesFailed = append(nodesFailed, nodeNameField)
				continue
			}
			defer func() { _ = gz.Close() }()
			reader = gz
		}

		var stateResp StateSyncResponse
		if err := json.NewDecoder(reader).Decode(&stateResp); err != nil {
			p.Log(logging.LevelWarning, "STATE_MERGE", "Failed to decode state from %s: %v", nodeNameField, err)
			_ = resp.Body.Close()
			nodesFailed = append(nodesFailed, nodeNameField)
			continue
		}
		_ = resp.Body.Close()

		// Check version compatibility
		if stateResp.Version != StateSyncVersion {
			p.Log(logging.LevelWarning, "STATE_MERGE", "Version mismatch from %s: got %s, expected %s", nodeNameField, stateResp.Version, StateSyncVersion)
			// Continue anyway for backward compatibility
		}

		nodesQueried = append(nodesQueried, nodeNameField)

		// Merge states with conflict resolution
		for ip, state := range stateResp.States {
			// Add source node to reason
			state.Reason = addSourceNode(state.Reason, nodeNameField, nodeAddressField)

			if existing, ok := merged[ip]; ok {
				// Merge reasons (prevent duplication)
				mergedState := persistence.IPState{
					Reason:     mergeReasons(existing.Reason, state.Reason),
					ExpireTime: maxTime(existing.ExpireTime, state.ExpireTime),
					ModifiedAt: maxTime(existing.ModifiedAt, state.ModifiedAt),
				}
				// Use State from the entry with longest expiry
				if state.ExpireTime.After(existing.ExpireTime) {
					mergedState.State = state.State
				} else {
					mergedState.State = existing.State
				}
				merged[ip] = mergedState
			} else {
				merged[ip] = state
			}
		}
	}

	return merged, nodesQueried, nodesFailed
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// addSourceNode adds source node attribution to a reason if not already present.
func addSourceNode(reason, nodeName, nodeAddress string) string {
	if strings.Contains(reason, " (") && strings.HasSuffix(reason, ")") {
		return reason
	}
	source := nodeName
	if source == "" {
		source = nodeAddress
	}
	// Validate source is not empty
	if source == "" {
		return reason // Return unchanged if no source available
	}
	return reason + " (" + source + ")"
}

// mergeReasons combines two reasons without duplication.
func mergeReasons(existing, newReason string) string {
	if existing == "" {
		return newReason
	}
	if newReason == "" {
		return existing
	}

	// Parse existing reasons into map
	reasonMap := make(map[string]bool)
	for _, part := range strings.Split(existing, ReasonSeparator) {
		baseReason := extractBaseReason(strings.TrimSpace(part))
		reasonMap[baseReason] = true
	}

	newBaseReason := extractBaseReason(newReason)
	if !reasonMap[newBaseReason] {
		return existing + ReasonSeparator + newReason
	}
	return existing
}

// extractBaseReason extracts the reason without source node attribution.
func extractBaseReason(reason string) string {
	if idx := strings.Index(reason, " ("); idx != -1 {
		return reason[:idx]
	}
	return reason
}
