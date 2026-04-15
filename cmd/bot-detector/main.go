package main

import (
	"bot-detector/internal/app"
	"bot-detector/internal/blocker"
	"bot-detector/internal/checker"
	"bot-detector/internal/cluster"
	"bot-detector/internal/commandline"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/logparser"
	"bot-detector/internal/metrics"
	"bot-detector/internal/persistence"
	"bot-detector/internal/processor"
	"bot-detector/internal/server"
	"bot-detector/internal/store"
	"bot-detector/internal/testutil"
	"bot-detector/internal/utils"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Helper function to extract settings from the BuildInfo struct
func findSetting(info *debug.BuildInfo, key string) string {
	for _, setting := range info.Settings {
		if setting.Key == key {
			return setting.Value
		}
	}
	return "unknown"
}

func buildDetails() {
	// Use the runtime/debug package to get automatically embedded info
	info, ok := debug.ReadBuildInfo()
	if ok {
		fmt.Fprintf(os.Stderr, "\n--- BUILD DETAILS:\n")
		fmt.Fprintf(os.Stderr, "  Go Version:  %s\n", info.GoVersion)
		fmt.Fprintf(os.Stderr, "  Commit Hash: %s\n", findSetting(info, "vcs.revision"))
		fmt.Fprintf(os.Stderr, "  Build Time:  %s\n", findSetting(info, "vcs.time"))
		fmt.Fprintf(os.Stderr, "  Dirty Build: %s\n", findSetting(info, "vcs.modified"))
	}
}

func customPanic() {
	if r := recover(); r != nil {
		now := time.Now()
		fmt.Fprintf(os.Stderr, "--- START OF PANIC REPORT\n")
		fmt.Fprintf(os.Stderr, "Time: %s\n", now)
		fmt.Fprintf(os.Stderr, "Message: %v\n", r)

		fmt.Fprintf(os.Stderr, "\n--- STACK TRACE:\n")
		debug.PrintStack()
		buildDetails()
		fmt.Fprintf(os.Stderr, "\n--- END OF PANIC REPORT\n")

		os.Exit(1)
	}
}

// main is the application entry point.
func main() {
	defer customPanic()

	params, err := commandline.ParseParameters(os.Args)
	if err != nil {
		switch err.Error() {
		case "flag: help requested", "no flag: help requested":
			os.Exit(0)
		default:
			// A parsing error will have already printed usage information.
			// We exit with a non-zero code after the error is logged.
			log.Printf("[FATAL] %v", err)
			os.Exit(1)
		}
	}

	if err := execute(params); err != nil {
		if err.Error() != "exit" {
			log.Printf("[FATAL] %v", err)
			os.Exit(1)
		} else {
			os.Exit(0)
		}
	}
}

func GetConfigFilePath(params *commandline.AppParameters) string {
	return filepath.Join(params.ConfigDir, "config.yaml")
}

// handleStartupFlags checks for command-line flags that prevent normal startup,
// such as --version or --check. It returns a special "exit" error to signal
// a clean exit, an error for failures, or nil to continue execution.
func handleStartupFlags(params *commandline.AppParameters) error {
	if params.ShowVersion {
		// Get build info from runtime
		info, ok := debug.ReadBuildInfo()
		commit := "unknown"
		buildTime := "unknown"
		if ok {
			commit = findSetting(info, "vcs.revision")
			buildTime = findSetting(info, "vcs.time")
		}
		fmt.Printf("bot-detector version %s (commit: %s, built: %s)\n",
			config.AppVersion, commit, buildTime)
		return fmt.Errorf("exit") // Signal clean exit
	}

	if params.Check {
		opts := config.LoadConfigOptions{
			ConfigFilePath: GetConfigFilePath(params),
		}
		var err error
		if _, err = config.LoadConfigFromYAML(opts); err != nil {
			return fmt.Errorf("configuration check failed: %v", err)
		}
		log.Println("[SUCCESS] Configuration is valid.")
		return fmt.Errorf("exit") // Signal clean exit
	}

	return nil // Continue execution
}

func initializeProcessor(params *commandline.AppParameters, appConfig *config.AppConfig, loadedCfg *config.LoadedConfig) *app.Processor {
	return &app.Processor{
		ActivityMutex:        &sync.RWMutex{},
		TopActorsPerChain:    make(map[string]map[string]*store.ActorStats),
		ActivityStore:        make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:          &sync.RWMutex{},
		Metrics:              metrics.NewMetrics(),
		Chains:               loadedCfg.Chains,
		Config:               appConfig,
		LogRegex:             loadedCfg.LogFormatRegex,
		DryRun:               params.DryRun,
		ExitOnEOF:            params.ExitOnEOF,
		EnableMetrics:        loadedCfg.Application.EnableMetrics,
		OooBufferFlushSignal: make(chan struct{}, 1), // Buffered channel of size 1
		SignalCh:             make(chan os.Signal, 1),
		LogFunc:              logging.LogOutput,
		NowFunc:              time.Now, // Use the real time.Now in production.
		ConfigFilePath:       GetConfigFilePath(params),
		ConfigDir:            params.ConfigDir,
		LogPath:              params.LogPath,
		ReloadOn:             params.ReloadOn,
		TopN:                 params.TopN,
		ListenConfigs:        params.ListenConfigs,
		ConfigReloaded:       false,

		// Initialize persistence fields
		// If --state-dir is set, persistence is enabled by default unless explicitly disabled in config
		PersistenceEnabled: params.StateDir != "" && (loadedCfg.Application.Persistence.Enabled == nil || *loadedCfg.Application.Persistence.Enabled),
		CompactionInterval: loadedCfg.Application.Persistence.CompactionInterval,
		ReasonCache:        make(map[string]*string),

		// Initialize cluster fields with defaults (will be set properly in later phases)
		Cluster:           loadedCfg.Cluster,
		NodeRole:          "leader",
		NodeName:          "",
		NodeAddress:       "",
		NodeLeaderAddress: "",

		// Initialize multi-website fields
		Websites:       loadedCfg.Websites,
		VHostToWebsite: make(map[string]string),
		WebsiteChains:  make(map[string][]int),
		GlobalChains:   []int{},
		UnknownVHosts:  make(map[string]bool),
	}
}

// fetchInitialStateFromCluster fetches the current cluster state from the leader
// instead of replaying the journal. Returns the cluster state timestamp.
func fetchInitialStateFromCluster(p *app.Processor) (time.Time, error) {
	// Find leader node
	var leaderNode *cluster.NodeConfig
	for i := range p.Cluster.Nodes {
		if p.Cluster.Nodes[i].Name != p.NodeName {
			leaderNode = &p.Cluster.Nodes[i]
			break
		}
	}
	if leaderNode == nil {
		return time.Time{}, fmt.Errorf("no leader node found in cluster configuration")
	}

	// Fetch merged state from leader using shared helper
	url := fmt.Sprintf("%s://%s/api/v1/cluster/state/merged", p.Cluster.Protocol, leaderNode.Address)
	client := &http.Client{Timeout: p.Cluster.StateSync.Timeout}

	states, peerBadActors, timestamp, m, err := cluster.FetchMergedState(url, client, p.Cluster.StateSync.Compression)
	if err != nil {
		return time.Time{}, err
	}

	// Apply fetched state to SQLite
	p.PersistenceMutex.Lock()
	blockedCount := 0
	for ip, state := range states {
		reason := p.InternReason(state.Reason)
		if err := persistence.UpsertIPState(p.DB, ip, state.State, state.ExpireTime, reason, state.ModifiedAt, state.FirstBlockedAt); err != nil {
			p.LogFunc(logging.LevelError, "STATE_SYNC", "Failed to upsert state for %s: %v", ip, err)
		}
		if state.State == persistence.BlockStateBlocked {
			blockedCount++
		}
	}
	unblockedCount := len(states) - blockedCount
	p.PersistenceMutex.Unlock()

	// Apply bad actors from leader
	for _, ba := range peerBadActors {
		_ = p.ApplyBadActorFromPeer(ba.IP, ba.TotalScore, ba.BlockCount, ba.PromotedAt)
	}

	modeStr := "gz,full"
	if !m.Compressed {
		modeStr = "plain,full"
	}

	p.LogFunc(logging.LevelInfo, "STATE_SYNC", "Fetched initial state from leader: %d IPs (%d blocked + %d unblocked), %d bad actors, size: %.1f KB, rate: %.1f KB/s, duration: %v, mode: %s",
		len(states), blockedCount, unblockedCount, len(peerBadActors), m.SizeKB, m.RateKBps, m.Duration.Round(time.Millisecond), modeStr)

	return timestamp, nil
}

// replayJournalAfter replays journal entries that are newer than the given timestamp.
// This ensures local changes that haven't been synced to the cluster are not lost.
func replayJournalAfter(p *app.Processor, after time.Time) error {
	journalPath := filepath.Join(p.StateDir, "events.log")
	journalFile, err := os.Open(journalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No journal file, nothing to replay
		}
		return err
	}
	defer func() { _ = journalFile.Close() }()

	blockEvents := 0
	unblockEvents := 0
	skippedEvents := 0
	parseErrors := 0

	scanner := bufio.NewScanner(journalFile)
	for scanner.Scan() {
		line := scanner.Bytes()

		var v1Entry persistence.JournalEntryV1
		if err := json.Unmarshal(line, &v1Entry); err != nil {
			p.LogFunc(logging.LevelWarning, "JOURNAL_PARSE_FAIL", "Failed to parse journal event: %v", err)
			parseErrors++
			continue
		}

		// Only replay entries newer than cluster state
		if v1Entry.Timestamp.After(after) {
			reason := p.InternReason(v1Entry.Event.Reason)

			p.PersistenceMutex.Lock()
			switch v1Entry.Event.Type {
			case persistence.EventTypeBlock:
				blockEvents++
				expireTime := v1Entry.Timestamp.Add(v1Entry.Event.Duration)
				_ = persistence.InsertEvent(p.DB, v1Entry.Timestamp, persistence.EventTypeBlock, v1Entry.Event.IP, reason, v1Entry.Event.Duration, "")
				_ = persistence.UpsertIPState(p.DB, v1Entry.Event.IP, persistence.BlockStateBlocked, expireTime, reason, v1Entry.Timestamp, v1Entry.Timestamp)
			case persistence.EventTypeUnblock:
				unblockEvents++
				_ = persistence.InsertEvent(p.DB, v1Entry.Timestamp, persistence.EventTypeUnblock, v1Entry.Event.IP, reason, 0, "")
				_ = persistence.UpsertIPState(p.DB, v1Entry.Event.IP, persistence.BlockStateUnblocked, v1Entry.Timestamp, reason, v1Entry.Timestamp, time.Time{})
			}
			p.PersistenceMutex.Unlock()
		} else {
			skippedEvents++
		}
	}

	if blockEvents > 0 || unblockEvents > 0 {
		p.LogFunc(logging.LevelInfo, "JOURNAL_REPLAY", "Replayed local journal entries after cluster state (blocks=%d, unblocks=%d, skipped=%d, errors=%d)",
			blockEvents, unblockEvents, skippedEvents, parseErrors)
	}

	return nil
}

func restorePersistenceState(p *app.Processor) error {
	// -- STATE RESTORATION --
	if err := os.MkdirAll(p.StateDir, 0750); err != nil {
		return fmt.Errorf("failed to create state directory '%s': %v", p.StateDir, err)
	}
	p.LogFunc(logging.LevelInfo, "SETUP", "Persistence enabled. Loading state from '%s'...", p.StateDir)

	// 1. Open (or create) SQLite database
	db, err := persistence.OpenDB(p.StateDir, p.DryRun)
	if err != nil {
		p.LogFunc(logging.LevelError, "SQLITE_INIT_FAIL", "Failed to initialize SQLite: %v", err)
		return err
	}
	p.DB = db

	// 2. Migrate from legacy format if needed (skip in dry-run to avoid modifying files)
	if !p.DryRun && persistence.ShouldMigrate(p.StateDir) {
		p.LogFunc(logging.LevelInfo, "MIGRATION", "Legacy persistence files detected, migrating to SQLite...")
		if err := persistence.MigrateFromLegacy(p.DB, p.StateDir); err != nil {
			p.LogFunc(logging.LevelError, "MIGRATION_FAIL", "Failed to migrate legacy persistence: %v", err)
			return err
		}
		p.LogFunc(logging.LevelInfo, "MIGRATION", "Legacy persistence migration completed successfully")
	}

	// 3. Cluster state sync (follower optimization)
	if p.Cluster != nil && p.Cluster.StateSync.Enabled && p.NodeRole == "follower" {
		p.LogFunc(logging.LevelInfo, "STATE_SYNC", "Attempting to fetch initial state from cluster...")
		if clusterTimestamp, fetchErr := fetchInitialStateFromCluster(p); fetchErr == nil {
			p.LogFunc(logging.LevelInfo, "STATE_SYNC", "Successfully fetched initial state from cluster")
			p.InitialSyncTime = clusterTimestamp

			// Replay local journal entries newer than cluster state (only during migration transition)
			if err := replayJournalAfter(p, clusterTimestamp); err != nil {
				p.LogFunc(logging.LevelWarning, "JOURNAL_REPLAY", "Failed to replay local journal: %v", err)
			}
			return nil
		} else {
			p.LogFunc(logging.LevelWarning, "STATE_SYNC", "Failed to fetch initial state from cluster, using local database: %v", fetchErr)
		}
	}

	// 4. Restore state to HAProxy (skip in dry-run mode)
	if p.DryRun {
		p.LogFunc(logging.LevelInfo, "SETUP", "Dry-run mode: skipping HAProxy state restoration")
		return nil
	}

	allStates, err := persistence.GetAllIPStates(p.DB)
	if err != nil {
		p.LogFunc(logging.LevelError, "STATE_LOAD_FAIL", "Failed to load IP states from database: %v", err)
		return err
	}

	blockedCount := 0
	unblockedCount := 0
	for _, state := range allStates {
		if state.State == persistence.BlockStateBlocked {
			blockedCount++
		} else {
			unblockedCount++
		}
	}
	p.LogFunc(logging.LevelInfo, "STATE_LOAD", "Loaded %d IP states from database (%d blocked + %d unblocked)",
		len(allStates), blockedCount, unblockedCount)

	p.LogFunc(logging.LevelInfo, "STATE_RESTORE", "Querying HAProxy current state...")
	currentState, err := p.Blocker.GetCurrentState()
	if err != nil {
		p.LogFunc(logging.LevelWarning, "STATE_QUERY_FAIL", "Failed to query HAProxy state: %v. Will restore all IPs.", err)
		currentState = make(map[string]int)
	}

	// Create a sorted list of table durations for best-fit matching
	type tableInfo struct {
		duration time.Duration
		name     string
	}
	var sortedTables []tableInfo
	for d, n := range p.Config.Blockers.Backends.HAProxy.DurationTables {
		sortedTables = append(sortedTables, tableInfo{duration: d, name: n})
	}
	sort.Slice(sortedTables, func(i, j int) bool {
		return sortedTables[i].duration < sortedTables[j].duration
	})

	type directBlocker interface {
		BlockDirect(ipInfo utils.IPInfo, duration time.Duration, reason string) error
		UnblockDirect(ipInfo utils.IPInfo, reason string) error
	}
	directB, hasDirectMethods := p.Blocker.(directBlocker)

	skipped := 0
	skippedGoodActor := 0
	skippedAlreadyBlocked := 0
	skippedAlreadyUnblocked := 0
	skippedExpired := 0
	restored := 0
	for ip, state := range allStates {
		tempEntry := &app.LogEntry{IPInfo: utils.NewIPInfo(ip)}
		if isGood, reason := checker.IsGoodActor(p, tempEntry); isGood {
			p.LogFunc(logging.LevelInfo, "STATE_RESTORE_SKIP", "Skipping restore for %s (good_actor: %s)", ip, reason)
			skipped++
			skippedGoodActor++
			continue
		}

		if state.State == persistence.BlockStateBlocked {
			if gpc0, exists := currentState[ip]; exists && gpc0 > 0 {
				skipped++
				skippedAlreadyBlocked++
				continue
			}

			remainingDuration := time.Until(state.ExpireTime)
			if remainingDuration > 0 {
				bestFitDuration := p.Config.Blockers.DefaultDuration
				for _, t := range sortedTables {
					if remainingDuration <= t.duration {
						bestFitDuration = t.duration
						break
					}
				}
				var blockErr error
				if hasDirectMethods {
					blockErr = directB.BlockDirect(utils.NewIPInfo(ip), bestFitDuration, state.Reason)
				} else {
					blockErr = p.Blocker.Block(utils.NewIPInfo(ip), bestFitDuration, state.Reason)
				}
				if blockErr != nil {
					p.LogFunc(logging.LevelError, "STATE_RESTORE_FAIL", "Failed to restore block for IP %s: %v", ip, blockErr)
				} else {
					restored++
				}
			} else {
				skipped++
				skippedExpired++
			}
		} else if state.State == persistence.BlockStateUnblocked {
			if gpc0, exists := currentState[ip]; exists && gpc0 == 0 {
				skipped++
				skippedAlreadyUnblocked++
				continue
			}

			var unblockErr error
			if hasDirectMethods {
				unblockErr = directB.UnblockDirect(utils.NewIPInfo(ip), state.Reason)
			} else {
				unblockErr = p.Blocker.Unblock(utils.NewIPInfo(ip), state.Reason)
			}
			if unblockErr != nil {
				p.LogFunc(logging.LevelError, "STATE_RESTORE_FAIL", "Failed to restore unblock for IP %s: %v", ip, unblockErr)
			} else {
				restored++
			}
		}
	}

	p.LogFunc(logging.LevelInfo, "STATE_RESTORE", "State restoration complete: %d restored, %d skipped (already_blocked=%d, already_unblocked=%d, expired=%d, good_actors=%d)",
		restored, skipped, skippedAlreadyBlocked, skippedAlreadyUnblocked, skippedExpired, skippedGoodActor)

	if restored > 50000 {
		p.LogFunc(logging.LevelWarning, "STATE_RESTORE", "Restored %d IPs. Verify HAProxy stick table capacity.", restored)
	}

	// Restore bad actors
	if p.Config.BadActors.Enabled {
		badActors, baErr := persistence.GetAllBadActors(p.DB)
		if baErr != nil {
			p.LogFunc(logging.LevelError, "STATE_RESTORE", "Failed to load bad actors: %v", baErr)
		} else if len(badActors) > 0 {
			baRestored := 0
			for _, ba := range badActors {
				if !p.DryRun && p.Blocker != nil {
					ipInfo := utils.NewIPInfo(ba.IP)
					if blockErr := p.Blocker.Block(ipInfo, p.Config.BadActors.BlockDuration, "bad-actor"); blockErr == nil {
						baRestored++
					}
				}
			}
			p.LogFunc(logging.LevelInfo, "STATE_RESTORE", "Restored %d bad actors (%d total in database)", baRestored, len(badActors))
		}
	}

	return nil
}

// resolveLeaderAddress resolves a leader address from FOLLOW file content.
// If the content looks like a URL or host:port, it's used directly.
// Otherwise, it's treated as a node name and resolved from environment variables.
func resolveLeaderAddress(followContent string, envParams *commandline.EnvParameters) (string, error) {
	// Check if it's already a URL or host:port (use directly)
	if strings.Contains(followContent, "://") {
		return followContent, nil
	}

	// Check for host:port pattern (contains colon and numeric port)
	parts := strings.Split(followContent, ":")
	if len(parts) == 2 {
		if _, err := strconv.Atoi(parts[1]); err == nil {
			return followContent, nil
		}
	}

	// Otherwise, treat as node name and resolve from environment
	if envParams == nil || len(envParams.ClusterNodes) == 0 {
		return "", fmt.Errorf(
			"FOLLOW file contains node name '%s', but no BOT_DETECTOR_NODES environment variable is set. "+
				"During bootstrap, either provide a direct address in FOLLOW or set BOT_DETECTOR_NODES",
			followContent,
		)
	}

	// Search for node by name
	for _, node := range envParams.ClusterNodes {
		if node.Name == followContent {
			logging.LogOutput(logging.LevelInfo, "CLUSTER",
				"Resolved leader name '%s' to address '%s' for bootstrap",
				followContent, node.Address)
			return node.Address, nil
		}
	}

	return "", fmt.Errorf(
		"FOLLOW file contains node name '%s', but no such node found in BOT_DETECTOR_NODES",
		followContent,
	)
}

// execute is the main application logic, decoupled from command-line parsing.
func execute(params *commandline.AppParameters) error {
	if err := handleStartupFlags(params); err != nil {
		if err.Error() == "exit" {
			return nil
		}
		return err
	}

	// Log build information (after handling --version and --check)
	info, ok := debug.ReadBuildInfo()
	if ok {
		commit := findSetting(info, "vcs.revision")
		buildTime := findSetting(info, "vcs.time")
		dirty := findSetting(info, "vcs.modified")

		log.Printf("[BUILD] Version: %s, Go: %s", config.AppVersion, info.GoVersion)
		log.Printf("[BUILD] Commit: %s, Time: %s, Dirty: %s", commit, buildTime, dirty)
	}

	configFilePath := GetConfigFilePath(params)

	// Check if FOLLOW file exists and determine if we need to bootstrap
	followPath := filepath.Join(params.ConfigDir, "FOLLOW")
	followData, err := os.ReadFile(followPath)
	if err == nil {
		// FOLLOW file exists - this is a follower
		followContent := strings.TrimSpace(string(followData))
		if followContent == "" {
			return fmt.Errorf("FOLLOW file exists but is empty")
		}

		// Check if config file exists, if not, bootstrap
		if _, err := os.Stat(configFilePath); os.IsNotExist(err) {
			// Config doesn't exist, bootstrap from leader
			// Resolve leader address from FOLLOW content (may be name or address)
			leaderAddr, err := resolveLeaderAddress(followContent, params.Envs)
			if err != nil {
				return fmt.Errorf("failed to resolve leader address for bootstrap: %w", err)
			}

			// Add http:// prefix if not present
			if !strings.HasPrefix(leaderAddr, "http://") && !strings.HasPrefix(leaderAddr, "https://") {
				leaderAddr = "http://" + leaderAddr
			}

			logging.LogOutput(logging.LevelInfo, "CLUSTER", "Config file not found, bootstrapping from leader at %s", leaderAddr)
			if err := cluster.Bootstrap(cluster.BootstrapOptions{
				LeaderAddress:  leaderAddr,
				ConfigFilePath: configFilePath,
				LogFunc:        logging.LogOutput,
				HTTPTimeout:    10 * time.Second,
				ForceUpdate:    false,
				Protocol:       config.DefaultClusterProtocol,
			}); err != nil {
				return fmt.Errorf("failed to bootstrap config from leader: %w", err)
			}
		}
	} else if !os.IsNotExist(err) {
		// Error reading FOLLOW file (but not "file doesn't exist")
		return fmt.Errorf("failed to read FOLLOW file: %w", err)
	}
	// If FOLLOW doesn't exist, this is a leader - no bootstrap needed

	fileInfo, err := os.Stat(configFilePath)
	if err != nil {
		return fmt.Errorf("could not stat file: %q  - %v", configFilePath, err)
	}

	logging.LogOutput(logging.LevelInfo, "CONFIG", "Configuration directory: %s", params.ConfigDir)

	loadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigFilePath: configFilePath})
	if err != nil {
		return fmt.Errorf("configuration Load Error: %v", err)
	}

	// Apply environment variable overrides
	if params.Envs != nil && len(params.Envs.ClusterNodes) > 0 {
		if loadedCfg.Cluster != nil {
			logging.LogOutput(logging.LevelInfo, "CONFIG",
				"Cluster nodes from config.yaml replaced by BOT_DETECTOR_NODES environment variable (%d nodes)",
				len(params.Envs.ClusterNodes))
			loadedCfg.Cluster.Nodes = params.Envs.ClusterNodes
		} else {
			// Cluster wasn't configured in YAML, but BOT_DETECTOR_NODES is set
			// Create a minimal ClusterConfig with the nodes from environment
			logging.LogOutput(logging.LevelInfo, "CONFIG",
				"Cluster configuration created from BOT_DETECTOR_NODES environment variable (%d nodes)",
				len(params.Envs.ClusterNodes))
			loadedCfg.Cluster = config.NewClusterConfigWithDefaults(params.Envs.ClusterNodes)
		}
	}

	// Runtime validation: if cluster is configured, it must have at least one node
	if loadedCfg.Cluster != nil && len(loadedCfg.Cluster.Nodes) == 0 {
		return fmt.Errorf("cluster configuration is present but has no nodes. Either add nodes to cluster.nodes in config.yaml or set BOT_DETECTOR_NODES environment variable")
	}

	// Runtime validation: multi-website mode vs --log-path flag
	if len(loadedCfg.Websites) > 0 {
		if params.LogPath != "" && !params.DryRun {
			logging.LogOutput(logging.LevelWarning, "CONFIG",
				"--log-path flag is ignored in multi-website mode. Log paths are defined in config.yaml")
		}
	} else {
		// Legacy single-website mode requires --log-path
		if params.LogPath == "" && !params.DryRun {
			return fmt.Errorf("--log-path is required in single-website mode (no 'websites' section in config)")
		}
	}

	logging.SetLogLevel(loadedCfg.Application.LogLevel)

	appConfig := &config.AppConfig{
		Application:      loadedCfg.Application,
		Parser:           loadedCfg.Parser,
		Checker:          loadedCfg.Checker,
		Blockers:         loadedCfg.Blockers,
		BadActors:        loadedCfg.BadActors,
		GoodActors:       loadedCfg.GoodActors,
		FileDependencies: loadedCfg.FileDependencies,
		LastModTime:      fileInfo.ModTime(),
		StatFunc:         processor.DefaultStatFunc,
		FileOpener:       func(name string) (config.FileHandle, error) { return os.Open(name) },
		YAMLContent:      loadedCfg.YAMLContent,
	}

	p := initializeProcessor(params, appConfig, loadedCfg)

	if params.StateDir != "" {
		p.StateDir = params.StateDir
	}

	if p.PersistenceEnabled && p.StateDir == "" {
		return fmt.Errorf("persistence cannot be enabled without --state-dir")
	}

	// Determine node identity based on FOLLOW file and cluster configuration
	// Extract addresses from ListenConfigs for cluster matching
	listenAddrs := make([]string, 0, len(params.ListenConfigs))
	for _, cfg := range params.ListenConfigs {
		listenAddrs = append(listenAddrs, cfg.Address)
	}

	identity, err := cluster.DetermineIdentity(params.ConfigDir, listenAddrs, params.ClusterNodeName, loadedCfg.Cluster)
	if err != nil {
		return fmt.Errorf("failed to determine node identity: %w", err)
	}
	p.NodeRole = identity.Role.String()
	p.NodeName = identity.Name
	p.NodeAddress = identity.Address
	p.NodeLeaderAddress = identity.LeaderAddress

	logging.LogOutput(logging.LevelInfo, "CLUSTER", "Node identity: %s", identity.String())

	p.StartTime = p.NowFunc()
	p.SignalOooBufferFlush = func() { checker.DoSignalOooBufferFlush(p) }
	app.InitializeMetrics(p, loadedCfg)

	// Initialize multi-website support if configured
	if len(p.Websites) > 0 {
		p.VHostToWebsite, p.CatchAllWebsite = app.BuildVHostMap(p.Websites)
		p.WebsiteChains, p.GlobalChains = app.CategorizeChains(p.Chains)
		p.LogFunc(logging.LevelInfo, "SETUP", "Multi-website mode: %d websites, %d global chains",
			len(p.Websites), len(p.GlobalChains))
	}

	haproxyBlocker := blocker.NewHAProxyBlocker(p, p.DryRun)

	// Set up resync callback to handle backend restarts/recoveries
	haproxyBlocker.ResyncCallback = func(addr string) {
		blockedIPs := make(map[string]blocker.BlockInfo)
		unblockedIPs := make(map[string]string)

		// Use persistence state if available, otherwise use activity store
		if p.PersistenceEnabled {
			// Resync from persistence state (more reliable)
			allStates, err := persistence.GetAllIPStates(p.DB)
			if err != nil {
				p.LogFunc(logging.LevelError, "RESYNC", "Failed to query IP states for resync: %v", err)
			} else {
				for ip, state := range allStates {
					switch state.State {
					case persistence.BlockStateBlocked:
						remaining := time.Until(state.ExpireTime)
						if remaining > 0 {
							blockedIPs[ip] = blocker.BlockInfo{
								Duration: remaining,
								Reason:   state.Reason,
							}
						}
					case persistence.BlockStateUnblocked:
						unblockedIPs[ip] = state.Reason
					}
				}
			}
		} else {
			// Resync from activity store (in-memory only)
			p.ActivityMutex.RLock()
			for actor, activity := range p.ActivityStore {
				if activity.IsBlocked && time.Now().Before(activity.BlockedUntil) {
					remaining := time.Until(activity.BlockedUntil)
					blockedIPs[actor.IPInfo.Address] = blocker.BlockInfo{
						Duration: remaining,
						Reason:   "resync",
					}
				} else if activity.LastUnblockTime.After(time.Time{}) {
					unblockedIPs[actor.IPInfo.Address] = activity.LastUnblockReason
				}
			}
			p.ActivityMutex.RUnlock()
		}

		// Trigger resync for blocked IPs
		if err := haproxyBlocker.ResyncBackend(addr, blockedIPs); err != nil {
			p.LogFunc(logging.LevelError, "RESYNC", "Resync failed for backend %s: %v", addr, err)
		}

		// Trigger resync for unblocked IPs (good actors)
		if len(unblockedIPs) > 0 {
			if err := haproxyBlocker.ResyncUnblockedIPs(addr, unblockedIPs); err != nil {
				p.LogFunc(logging.LevelError, "RESYNC", "Unblock resync failed for backend %s: %v", addr, err)
			}
		}
	}

	haproxyBlocker.StartHealthCheck(p.Config.Blockers.HealthCheckInterval)
	rateLimitedBlocker := blocker.NewRateLimitedBlocker(p, p, haproxyBlocker, p.Config.Blockers.CommandQueueSize, p.Config.Blockers.CommandsPerSecond)
	p.Blocker = rateLimitedBlocker

	if p.PersistenceEnabled {
		if err := restorePersistenceState(p); err != nil {
			return err
		}
	}

	if params.DumpBackends {
		return dumpBackendsAndExit(p)
	}

	p.CheckChainsFunc = func(entry *app.LogEntry) { checker.CheckChains(p, entry) }
	p.ProcessLogLine = func(line string) { logparser.ProcessLogLineInternal(p, line) }

	app.LogConfigurationSummary(p, nil, nil) // Initial load, no old config
	app.LogChainDetails(p, p.Chains, "Loaded chains")

	start(p)

	performGracefulShutdown(p)

	return nil
}

func dumpBackendsAndExit(p *app.Processor) error {
	// Only run the sync check if there are multiple backends to compare.
	if len(p.GetBlockerAddresses()) > 1 {
		logging.LogOutput(logging.LevelInfo, "SYNC_CHECK", "Checking HAProxy backend synchronization...")
		// Use a 5-second tolerance for expiration differences
		discrepancies, err := p.Blocker.CompareHAProxyBackends(5 * time.Second)
		if err != nil {
			logging.LogOutput(logging.LevelError, "SYNC_CHECK_FAIL", "Failed to compare HAProxy backends: %v", err)
			return err
		}

		if len(discrepancies) > 0 {
			logging.LogOutput(logging.LevelError, "SYNC_CHECK_FAIL", "HAProxy backends are out of sync. Aborting dump.")
			for _, d := range discrepancies {
				logging.LogOutput(logging.LevelError, "SYNC_CHECK_FAIL", "  - IP: %s, Table: %s, Reason: %s, Details: %v", d.IP, d.TableName, d.Reason, d.Details)
			}
			return fmt.Errorf("HAProxy backends are out of sync")
		}
		logging.LogOutput(logging.LevelInfo, "SYNC_CHECK", "HAProxy backends are in sync.")
	} else {
		logging.LogOutput(logging.LevelInfo, "SYNC_CHECK", "Skipping backend synchronization check (only one backend configured).")
	}

	logging.LogOutput(logging.LevelInfo, "DUMP_BACKENDS", "Retrieving currently blocked IPs from HAProxy...")
	// Add a small delay to allow the command queue to process restored blocks.
	time.Sleep(1 * time.Second)
	blockedIPs, err := p.Blocker.DumpBackends()
	if err != nil {
		logging.LogOutput(logging.LevelError, "DUMP_FAIL", "Failed to retrieve blocked IPs: %v", err)
		return err
	}
	if len(blockedIPs) == 0 {
		logging.LogOutput(logging.LevelInfo, "DUMP_BACKENDS", "No IPs currently blocked by HAProxy.")
	} else {
		logging.LogOutput(logging.LevelInfo, "DUMP_BACKENDS", "Currently blocked IPs:")
		for _, ip := range blockedIPs {
			fmt.Println(ip)
		}
	}
	return nil
}

func performGracefulShutdown(p *app.Processor) {
	p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Graceful shutdown initiated.")

	if p.Blocker != nil {
		p.Blocker.Shutdown()
	}

	if p.PersistenceEnabled {
		if p.CompactionInterval > 0 {
			p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Performing final cleanup...")
			runCleanup(p)
		}

		p.LogFunc(logging.LevelInfo, "PERSISTENCE", "Closing database.")
		if err := persistence.CloseDB(p.DB); err != nil {
			p.LogFunc(logging.LevelError, "PERSISTENCE", "Error closing database: %v", err)
		}
	}
	fmt.Fprintln(os.Stderr, "[SHUTDOWN] Shutdown complete.")
}

func runCleanup(p *app.Processor) {
	now := p.NowFunc()
	retentionPeriod := p.Config.Application.Persistence.RetentionPeriod

	expiredBlocks, err := persistence.CleanupExpiredBlocks(p.DB, now)
	if err != nil {
		p.LogFunc(logging.LevelError, "CLEANUP_FAIL", "Failed to cleanup expired blocks: %v", err)
	}

	cleanedUnblocked, err := persistence.CleanupOldUnblocked(p.DB, now, retentionPeriod)
	if err != nil {
		p.LogFunc(logging.LevelError, "CLEANUP_FAIL", "Failed to cleanup old unblocked: %v", err)
	}

	cleanedEvents, err := persistence.CleanupOldEvents(p.DB, retentionPeriod)
	if err != nil {
		p.LogFunc(logging.LevelError, "CLEANUP_FAIL", "Failed to cleanup old events: %v", err)
	}

	cleanedReasons, err := persistence.CleanupOrphanedReasons(p.DB)
	if err != nil {
		p.LogFunc(logging.LevelError, "CLEANUP_FAIL", "Failed to cleanup orphaned reasons: %v", err)
	}

	// Cleanup low bad actor scores (> 30 days old, score < 2.0)
	cleanedScores := 0
	if p.Config.BadActors.Enabled {
		cleanedScores, err = persistence.CleanupLowScores(p.DB, p.Config.BadActors.ScoreMaxAge, p.Config.BadActors.ScoreMinCleanup)
		if err != nil {
			p.LogFunc(logging.LevelError, "CLEANUP_FAIL", "Failed to cleanup low scores: %v", err)
		}
	}

	if expiredBlocks > 0 || cleanedUnblocked > 0 || cleanedEvents > 0 || cleanedReasons > 0 || cleanedScores > 0 {
		p.LogFunc(logging.LevelInfo, "CLEANUP", "Cleanup completed: expired_blocks=%d, old_unblocked=%d, old_events=%d, orphaned_reasons=%d, low_scores=%d",
			expiredBlocks, cleanedUnblocked, cleanedEvents, cleanedReasons, cleanedScores)
	}

	if err := persistence.CheckpointWAL(p.DB); err != nil {
		p.LogFunc(logging.LevelError, "CLEANUP_FAIL", "Failed to checkpoint WAL: %v", err)
	}

	// Reclaim freed pages from deletions above.
	// Pass a large page count to ensure all free pages are reclaimed.
	if _, err := p.DB.Exec("PRAGMA incremental_vacuum(1000000)"); err != nil {
		p.LogFunc(logging.LevelError, "CLEANUP_FAIL", "Failed to run incremental vacuum: %v", err)
	}
}

// start is the unexported function that contains the main application logic,
// which is called by the tests and the main function.
func start(p *app.Processor) {
	if p.DryRun {
		// DryRun mode: Process a static log file and exit when done.
		done := make(chan struct{})
		// Pass the Processor instance P
		go processor.DryRunLogProcessor(p, done)

		// Wait for the processor to finish in dry-run mode
		<-done

	} else {
		// Live mode: Start background routines and the main log tailing loop.
		stopWatcher := make(chan struct{})
		defer close(stopWatcher) // Ensure watcher is stopped on main exit
		// Only start these background tasks if not in a test that bypasses them.
		// This allows tests to focus on specific components like the tailer without
		// interference from other goroutines like the config watcher.
		if !testutil.IsTesting() {
			// The ConfigWatcher is not started in test mode to prevent race conditions where
			// the test's config is overwritten by a reload from the default config file.
			reloadSignalCh := make(chan os.Signal, 1)
			signal.Notify(reloadSignalCh, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2)

			reloadFlag := strings.ToLower(p.ReloadOn)

			// Determine which reloading mechanisms to start based on the flag.
			switch reloadFlag {
			case "hup", "usr1", "usr2":
				// Flag is set to a specific signal. Watcher is disabled.
				p.LogFunc(logging.LevelInfo, "SETUP", "File watcher disabled. Reloading only on %s signal.", strings.ToUpper(reloadFlag))
				go app.SignalReloader(p, stopWatcher, reloadSignalCh)

			case "watcher":
				// Flag is 'watcher'. Signal reloading is disabled.
				p.LogFunc(logging.LevelInfo, "SETUP", "Signal-based reloading disabled. Watching config file for changes.")
				go app.ConfigWatcher(p, stopWatcher)

			case "":
				// Flag is absent. Both watcher and SIGHUP reloader are active.
				p.LogFunc(logging.LevelInfo, "SETUP", "File watcher enabled. Also listening on SIGHUP for forced reload.")
				go app.ConfigWatcher(p, stopWatcher)
				go app.SignalReloader(p, stopWatcher, reloadSignalCh) // This will default to HUP when p.ReloadOn is ""
			default:
				// An invalid value was provided. Log a fatal error and exit.
				log.Fatalf("[FATAL] Invalid value for --reload-on: '%s'. Must be one of 'watcher', 'hup', 'usr1', or 'usr2'.", p.ReloadOn)
			}

			if p.PersistenceEnabled && p.CompactionInterval > 0 {
				go func() {
					ticker := time.NewTicker(p.CompactionInterval)
					defer ticker.Stop()
					p.LogFunc(logging.LevelInfo, "SETUP", "Database cleanup enabled, running every %v.", p.CompactionInterval)
					for {
						select {
						case <-ticker.C:
							runCleanup(p)
						case <-stopWatcher:
							p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Cleanup goroutine shutting down.")
							return
						}
					}
				}()
			}

			if p.TopN > 0 {
				go processor.CleanupTopActors(p, stopWatcher)
			}
			go store.CleanUpIdleActors(p, stopWatcher)
			go checker.EntryBufferWorker(p, stopWatcher)

			// Set up signal handling before starting server
			signal.Notify(p.SignalCh, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-p.SignalCh
				signal.Stop(p.SignalCh)
				close(p.SignalCh)
			}()

			go server.Start(p)

			// Start config poller for follower nodes
			if p.NodeRole == "follower" && p.NodeLeaderAddress != "" {
				// Create a channel for config reload signals
				configReloadCh := make(chan struct{}, 1)

				// Start the config poller
				pollInterval := 30 * time.Second // Default
				if p.Cluster != nil && p.Cluster.ConfigPollInterval > 0 {
					pollInterval = p.Cluster.ConfigPollInterval
				}

				poller := cluster.NewConfigPoller(cluster.ConfigPollerOptions{
					LeaderAddress:  p.NodeLeaderAddress,
					ConfigFilePath: p.ConfigFilePath,
					PollInterval:   pollInterval,
					ConfigReloadCh: configReloadCh,
					ShutdownCh:     p.SignalCh,
					LogFunc:        p.LogFunc,
					HTTPTimeout:    10 * time.Second,
					Protocol:       p.Cluster.Protocol,
				})
				go poller.Start()

				// Handle config reload signals
				go func() {
					for {
						select {
						case <-configReloadCh:
							p.LogFunc(logging.LevelInfo, "CLUSTER", "Config reload triggered by leader update")
							// Make a copy of the old config for comparison
							p.ConfigMutex.RLock()
							oldConfig := p.Config.Clone()
							p.ConfigMutex.RUnlock()
							// Trigger config reload
							app.ReloadConfiguration(p, true, &oldConfig)
						case <-stopWatcher:
							return
						}
					}
				}()
			}

			// Start metrics collector for leader nodes
			if p.NodeRole == "leader" && p.Cluster != nil && len(p.Cluster.Nodes) > 0 {
				metricsInterval := 60 * time.Second // Default
				if p.Cluster.MetricsReportInterval > 0 {
					metricsInterval = p.Cluster.MetricsReportInterval
				}

				collector := cluster.NewMetricsCollector(cluster.MetricsCollectorOptions{
					Nodes:        p.Cluster.Nodes,
					PollInterval: metricsInterval,
					ShutdownCh:   p.SignalCh,
					LogFunc:      p.LogFunc,
					HTTPTimeout:  10 * time.Second,
					Protocol:     p.Cluster.Protocol,
				})
				// Store collector reference in processor for access by HTTP handlers
				p.MetricsCollector = collector
				go collector.Start()
			}

			// Start state sync manager if enabled
			if p.Cluster != nil && p.Cluster.StateSync.Enabled && p.PersistenceEnabled {
				syncMgr := cluster.NewStateSyncManager(
					p.Cluster,
					p.NodeRole,
					p.NodeName,
					p.NodeAddress,
					p.DB,
					&p.PersistenceMutex,
					p.LogFunc,
				)
				p.StateSyncManager = syncMgr

				// Wire up bad actor sync
				syncMgr.BadActorApplyFunc = p.ApplyBadActorFromPeer

				// If we fetched initial state from cluster, set the lastSyncTime
				// so the first periodic sync is incremental
				if !p.InitialSyncTime.IsZero() {
					syncMgr.SetLastSyncTime(p.InitialSyncTime)
				}

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go func() {
					<-p.SignalCh
					cancel()
				}()
				syncMgr.Start(ctx)
			}
		}

		// Main log tailing loop - always use dynamic manager for seamless config reload
		if processor.IsMultiWebsiteMode(p) {
			// Multi-website mode: tail multiple log files concurrently
			if p.LogPath != "" {
				p.LogFunc(logging.LevelWarning, "SETUP", "--log-path flag ignored: multi-website mode is enabled. Log paths are defined per-website in config.")
			}
			p.LogFunc(logging.LevelInfo, "SETUP", "Starting multi-website mode with %d websites", len(p.Websites))
		} else {
			// Single-website mode: create a catch-all website from --log-path
			// This allows seamless transition to multi-website mode on config reload
			p.Websites = []config.WebsiteConfig{{
				Name:    p.LogPath, // Use log path as website name
				LogPath: p.LogPath,
				VHosts:  []string{}, // Empty = catch-all
			}}
			p.LogFunc(logging.LevelInfo, "SETUP", "Starting single-website mode on %s", p.LogPath)
		}
		processor.MultiLogTailer(p, p.SignalCh)
	}
}
