package app

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/types"
	"bot-detector/internal/utils"
)

// signalMap maps common signal names to their syscall values.
// Moved from cmd/bot-detector/main.go to break import cycle
var signalMap = map[string]os.Signal{
	"HUP":  syscall.SIGHUP,
	"USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
}

// logStructFields recursively logs the fields of a struct that have a "summary" tag.
func logStructFields(p *Processor, val reflect.Value, typ reflect.Type) {
	for i := 0; i < val.NumField(); i++ {
		structField := typ.Field(i)
		fieldValue := val.Field(i)

		// Recurse into nested structs
		if fieldValue.Kind() == reflect.Struct {
			logStructFields(p, fieldValue, fieldValue.Type())
			continue
		}

		tag := structField.Tag.Get("summary")
		if tag == "" {
			continue // Skip fields without the summary tag
		}

		p.LogFunc(logging.LevelDebug, "CONFIG", "  - %s: %v", tag, fieldValue.Interface())
	}
}

// LogConfigurationSummary logs the key-value pairs of the current application configuration.
// This is useful for visibility on startup and after a configuration reload.
// Exported so it can be called from main or other packages.
func LogConfigurationSummary(p *Processor) {
	p.ConfigMutex.RLock()
	cfg := p.Config
	logRegex := p.LogRegex
	currentLogLevel := logging.GetLogLevel().String()
	p.ConfigMutex.RUnlock()

	if p.ConfigReloaded {
		p.LogFunc(logging.LevelInfo, "CONFIG_RELOAD", "Successfully reloaded main configuration from '%s'", p.ConfigPath)
	} else {
		p.ConfigReloaded = true
		p.LogFunc(logging.LevelInfo, "CONFIG", "Successfully loaded main configuration from '%s'", p.ConfigPath)
	}

	p.LogFunc(logging.LevelDebug, "CONFIG", "Loaded configuration:")

	// Handle special cases first
	p.LogFunc(logging.LevelDebug, "CONFIG", "  - log_level: %s", currentLogLevel)

	// Use reflection to iterate over tagged fields in AppConfig
	val := reflect.ValueOf(*cfg)
	typ := val.Type()
	logStructFields(p, val, typ)

	// Only show timestamp format if it's not the default.
	if cfg.Parser.TimestampFormat != config.AccessLogTimeFormat {
		p.LogFunc(logging.LevelDebug, "CONFIG", "  - timestamp_format: custom")
	}

	// Only show log format regex if it's custom.
	if logRegex != nil {
		p.LogFunc(logging.LevelDebug, "CONFIG", "  - log_format_regex: custom")
	}
}

// LogChainDetails logs details for a given list of chains, one per line.
// Exported for use in main.
func LogChainDetails(p *Processor, chains []config.BehavioralChain, header string) {
	p.LogFunc(logging.LevelDebug, "CONFIG", "%s (%d total)", header, len(chains))
	for _, chain := range chains {
		details := fmt.Sprintf("Name: '%s', Action: %s, Steps: %d, MatchKey: %s", chain.Name, chain.Action, len(chain.Steps), chain.MatchKey)
		// Always show block duration for clarity, indicating if it's a default.
		if chain.BlockDurationStr != "" {
			details += fmt.Sprintf(", BlockDuration: %s", chain.BlockDurationStr)
		} else if chain.UsesDefaultBlockDuration && chain.BlockDuration > 0 {
			// Fallback for default duration which doesn't have an original string from a chain
			details += fmt.Sprintf(", BlockDuration: default(%s)", chain.BlockDuration)
		}
		p.LogFunc(logging.LevelDebug, "CONFIG", "  - %s", details)
	}
}

// logFileDependencyChanges logs changes in file dependencies between old and new configurations.
func logFileDependencyChanges(p *Processor, oldDeps, newDeps map[string]*types.FileDependency) {
	var added, removed, modified []string

	// Check for added or modified files
	for path, newDep := range newDeps {
		oldDep, exists := oldDeps[path]
		if !exists {
			added = append(added, fmt.Sprintf("'%s' (Status: %s)", path, newDep.CurrentStatus.Status))
		} else {
			// Compare CurrentStatus of oldDep with CurrentStatus of newDep
			// This is the crucial part: oldDep.CurrentStatus represents the state *before* the reload.
			// newDep.CurrentStatus represents the state *after* the reload.
			if oldDep.CurrentStatus == nil || newDep.CurrentStatus == nil {
				// This case should ideally not happen if both maps are populated correctly,
				// but as a safeguard, treat as modified if status structs are missing.
				modified = append(modified, fmt.Sprintf("'%s' (status struct missing in comparison)", path))
			} else if oldDep.CurrentStatus.Status != newDep.CurrentStatus.Status {
				modified = append(modified, fmt.Sprintf("'%s' (status changed from %s to %s)", path, oldDep.CurrentStatus.Status, newDep.CurrentStatus.Status))
			} else if oldDep.CurrentStatus.Checksum != newDep.CurrentStatus.Checksum {
				modified = append(modified, fmt.Sprintf("'%s' (content changed - checksum mismatch)", path))
			}
		}
	}

	// Check for removed files
	for path := range oldDeps {
		if _, exists := newDeps[path]; !exists {
			removed = append(removed, fmt.Sprintf("'%s'", path))
		}
	}

	if len(added) > 0 {
		p.LogFunc(logging.LevelInfo, "FILE_DEP", "Added file dependencies: %s", strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		p.LogFunc(logging.LevelInfo, "FILE_DEP", "Removed file dependencies: %s", strings.Join(removed, ", "))
	}
	if len(modified) > 0 {
		p.LogFunc(logging.LevelInfo, "FILE_DEP", "Modified file dependencies: %s", strings.Join(modified, ", "))
	}
}

// findNewlyAddedGoodActors compares old and new good actor lists and returns newly added ones.
func findNewlyAddedGoodActors(oldActors, newActors []config.GoodActorDef) []config.GoodActorDef {
	oldSet := make(map[string]bool)
	for _, actor := range oldActors {
		oldSet[actor.Name] = true
	}

	var newlyAdded []config.GoodActorDef
	for _, actor := range newActors {
		if !oldSet[actor.Name] {
			newlyAdded = append(newlyAdded, actor)
		}
	}
	return newlyAdded
}

// unblockNewlyWhitelistedIPs checks all currently blocked IPs against newly added good actors
// and unblocks those that match. Uses a hybrid approach: fast path for exact IPs (O(1)),
// slow path for CIDR/regex patterns (O(N)).
func unblockNewlyWhitelistedIPs(p *Processor, newGoodActors []config.GoodActorDef) {
	if len(newGoodActors) == 0 {
		return
	}

	p.PersistenceMutex.Lock()
	blockedCount := len(p.ActiveBlocks)
	p.PersistenceMutex.Unlock()

	if blockedCount == 0 {
		return
	}

	unblocked := 0
	slowPathCount := 0

	for _, goodActor := range newGoodActors {
		// Check if this good actor has IP matchers
		if len(goodActor.IPMatchers) == 0 {
			continue // No IP matcher, skip
		}

		// We need to iterate through activeBlocks for pattern matching (CIDR/regex)
		// Create a temporary log entry for testing
		p.PersistenceMutex.Lock()
		for ip := range p.ActiveBlocks {
			// Create a minimal LogEntry with just the IP set
			testEntry := &LogEntry{
				IPInfo: utils.NewIPInfo(ip),
			}

			// Test if this IP matches the good actor's IP matcher
			if goodActor.IPMatchers[0](testEntry) {
				// Match found - need to unblock
				blockInfo := p.ActiveBlocks[ip]
				delete(p.ActiveBlocks, ip)

				// Issue unblock command to HAProxy
				if !p.DryRun && p.Blocker != nil {
					ipInfo := utils.NewIPInfo(ip)
					_ = p.Blocker.Unblock(ipInfo, "added-to-good-actors")
				}

				p.LogFunc(logging.LevelInfo, "UNBLOCK_WHITELIST",
					"Unblocked %s (was blocked by %s): newly added to good_actors (%s)",
					ip, blockInfo.Reason, goodActor.Name)
				unblocked++
			}
		}
		p.PersistenceMutex.Unlock()
		slowPathCount++
	}

	if unblocked > 0 {
		p.LogFunc(logging.LevelInfo, "UNBLOCK_WHITELIST",
			"Checked %d blocked IPs against %d new good actor rule(s), unblocked %d IPs",
			blockedCount, slowPathCount, unblocked)
	}
}

// ReloadConfiguration contains the logic to reload configuration, compare for changes,
// and update the processor state. It is designed to be called by either the
// file watcher or the signal reloader.
// Exported so it can be called from main or other packages.
func ReloadConfiguration(p *Processor, mainConfigChanged bool, oldConfigForComparison *config.AppConfig) { //nolint:cyclop
	p.ConfigMutex.RLock()
	oldChains := p.Chains
	// Use the provided oldConfigForComparison instead of cloning here.
	oldConfig := oldConfigForComparison
	oldLogRegex := p.LogRegex
	p.ConfigMutex.RUnlock()

	var newLastModTime time.Time
	if mainConfigChanged {
		fileInfo, err := os.Stat(p.ConfigPath)
		if err != nil {
			p.LogFunc(logging.LevelError, "WATCH_ERROR", "Failed to stat file for ModTime update %s: %v", p.ConfigPath, err)
			newLastModTime = oldConfig.LastModTime // Fallback to old time on error
		} else {
			newLastModTime = fileInfo.ModTime()
		}
	} else {
		newLastModTime = oldConfig.LastModTime // Preserve old time if main config didn't change
	}

	opts := config.LoadConfigOptions{
		ConfigPath:   p.ConfigPath,
		ExistingDeps: oldConfig.FileDependencies,
	}
	loadedCfg, err := config.LoadConfigFromYAML(opts)
	if err != nil {
		p.LogFunc(logging.LevelError, "LOAD_ERROR", "Failed to reload configuration: %v", err)
		return // The deferred signal will still fire.
	}

	// Create a new AppConfig from the loaded configuration.
	newAppConfig := &config.AppConfig{
		Application:      loadedCfg.Application,
		Parser:           loadedCfg.Parser,
		Checker:          loadedCfg.Checker,
		Blockers:         loadedCfg.Blockers,
		GoodActors:       loadedCfg.GoodActors,
		FileDependencies: loadedCfg.FileDependencies,

		// Preserve mockable functions and set the correct LastModTime.
		StatFunc:    oldConfig.StatFunc,
		LastModTime: newLastModTime,

		YAMLContent: loadedCfg.YAMLContent,
	}

	// Update the processor's state with the new config.
	p.ConfigMutex.Lock()
	p.Chains = loadedCfg.Chains
	p.Config = newAppConfig // Atomically swap the config pointer.
	// The LastModTime is already set correctly in newAppConfig, no need to update here.
	p.LogRegex = loadedCfg.LogFormatRegex
	p.EnableMetrics = loadedCfg.Application.EnableMetrics // Set the processor's EnableMetrics field
	InitializeMetrics(p, loadedCfg)

	logging.SetLogLevel(loadedCfg.Application.LogLevel)
	p.ConfigMutex.Unlock()

	// --- Compare and log general config changes ---
	configChanged := config.CompareConfigs(*oldConfig, *loadedCfg) ||
		(oldLogRegex != nil) != (loadedCfg.LogFormatRegex != nil)

	if configChanged {
		LogConfigurationSummary(p)
	}

	// --- Compare and log file dependency changes ---
	logFileDependencyChanges(p, oldConfig.FileDependencies, loadedCfg.FileDependencies)

	// --- Compare and log chain differences ---
	oldChainsMap := make(map[string]config.BehavioralChain)
	for _, chain := range oldChains {
		oldChainsMap[chain.Name] = chain
	}
	newChainsMap := make(map[string]config.BehavioralChain)
	for _, chain := range loadedCfg.Chains {
		newChainsMap[chain.Name] = chain
	}

	var added, removed, modified []config.BehavioralChain
	for name, newChain := range newChainsMap {
		if oldChain, exists := oldChainsMap[name]; !exists {
			added = append(added, newChain)
		} else if !config.AreChainsSemanticallyEqual(oldChain, newChain) {
			modified = append(modified, newChain)
		}
	}
	for name, oldChain := range oldChainsMap {
		if _, exists := newChainsMap[name]; !exists {
			removed = append(removed, oldChain)
		}
	}

	if len(added) > 0 {
		LogChainDetails(p, added, "Added chains:")
	}
	if len(modified) > 0 {
		LogChainDetails(p, modified, "Modified chains:")
	}
	if len(removed) > 0 {
		LogChainDetails(p, removed, "Removed chains:")
	}

	// --- Unblock IPs that match newly added good actors ---
	if newAppConfig.Checker.UnblockOnGoodActor {
		newlyAdded := findNewlyAddedGoodActors(oldConfig.GoodActors, loadedCfg.GoodActors)
		if len(newlyAdded) > 0 {
			unblockNewlyWhitelistedIPs(p, newlyAdded)
		}
	}

}

// InitializeMetrics sets up all the metric counters based on the loaded configuration.
// It resets and repopulates the metric maps, making it safe to call on both startup and reload.
// Exported so it can be called from main or other packages.
func InitializeMetrics(p *Processor, loadedCfg *config.LoadedConfig) {
	if !p.EnableMetrics {
		// If metrics are disabled, ensure all metric maps are nil or empty
		p.Metrics.ChainsCompleted = nil
		p.Metrics.ChainsReset = nil
		p.Metrics.ChainsHits = nil
		p.Metrics.MatchKeyHits = nil
		p.Metrics.BlockDurations = nil
		p.Metrics.CmdsPerBlocker = nil
		p.Metrics.GoodActorHits = nil
		return
	}

	// Reset and initialize per-chain metrics.
	p.Metrics.ChainsCompleted = &sync.Map{}
	p.Metrics.ChainsReset = &sync.Map{}
	p.Metrics.ChainsHits = &sync.Map{}
	for _, chain := range p.Chains {
		p.Metrics.ChainsCompleted.Store(chain.Name, chain.MetricsCounter)
		p.Metrics.ChainsReset.Store(chain.Name, chain.MetricsResetCounter)
		p.Metrics.ChainsHits.Store(chain.Name, chain.MetricsHitsCounter)
	}

	// Initialize match key hit counters.
	p.Metrics.MatchKeyHits = &sync.Map{}
	matchKeys := []string{"ip", "ipv4", "ipv6", "ip_ua", "ipv4_ua", "ipv6_ua"}
	for _, key := range matchKeys {
		p.Metrics.MatchKeyHits.Store(key, new(atomic.Int64))
	}

	// Initialize block duration counters.
	p.Metrics.BlockDurations = &sync.Map{}
	for duration := range loadedCfg.Blockers.Backends.HAProxy.DurationTables {
		p.Metrics.BlockDurations.Store(duration, new(atomic.Int64))
	}
	if loadedCfg.Blockers.DefaultDuration > 0 {
		p.Metrics.BlockDurations.Store(loadedCfg.Blockers.DefaultDuration, new(atomic.Int64))
	}

	// Initialize per-blocker command counters.
	p.Metrics.CmdsPerBlocker = &sync.Map{}
	for _, addr := range loadedCfg.Blockers.Backends.HAProxy.Addresses {
		p.Metrics.CmdsPerBlocker.Store(addr, new(atomic.Int64))
	}
	// Initialize good actor hit counters.
	p.Metrics.GoodActorHits = &sync.Map{}
	for _, goodActor := range loadedCfg.GoodActors {
		p.Metrics.GoodActorHits.Store(goodActor.Name, new(atomic.Int64))
	}
}

// SignalReloader listens for a specific OS signal to trigger a configuration reload. //nolint:cyclop
func SignalReloader(p *Processor, stop <-chan struct{}, signalCh chan os.Signal) {
	var signalName string
	// If ReloadOn is not specified, default to SIGHUP.
	if p.ReloadOn == "" {
		signalName = "HUP"
	} else {
		signalName = strings.ToUpper(p.ReloadOn)
	}

	// The main function should have already validated the signal name.
	// This check is now just a safeguard, especially for dry-run mode.
	if _, ok := signalMap[signalName]; !ok || p.DryRun {
		p.LogFunc(logging.LevelDebug, "SIGNAL", "Signal-based config reloading is disabled or signal is unsupported.")
		return
	}

	// The signal channel is already notified by the caller in main.go.

	p.LogFunc(logging.LevelInfo, "SIGNAL", "Signal-based config reloading enabled. Send %s signal to reload.", signalName)

	for {
		select {
		case <-stop:
			p.LogFunc(logging.LevelInfo, "SIGNAL", "SignalReloader received stop signal. Shutting down.")
			return
		case s := <-signalCh:
			p.LogFunc(logging.LevelInfo, "SIGNAL", "Received signal %s. Reloading configuration...", s)
			func() { // Use an anonymous function to scope the defer correctly.
				// Defer the test signal to ensure it's sent whether the reload succeeds or fails.
				if p.TestSignals != nil && p.TestSignals.ReloadDoneSignal != nil {
					defer func() { p.TestSignals.ReloadDoneSignal <- struct{}{} }()
				}
				// When reloading via signal, we don't have an "old" config from the watcher's perspective.
				// We need to clone the current config to serve as the oldConfigForComparison.
				p.ConfigMutex.RLock()
				currentConfig := p.Config.Clone()
				p.ConfigMutex.RUnlock()
				ReloadConfiguration(p, true, &currentConfig)
			}()
		}
	}
}

// ConfigWatcher monitors the YAML config file and FOLLOW file for modifications.
// It reloads config dynamically and detects role changes.
func ConfigWatcher(p *Processor, stop <-chan struct{}) {
	if p.DryRun {
		return
	}

	// Enforce a minimum safe interval.
	pollingInterval := p.Config.Application.Config.PollingInterval
	if pollingInterval < config.DefaultMinPollingInterval {
		pollingInterval = config.DefaultMinPollingInterval
	}

	// Track FOLLOW file state for change detection
	followPath := p.ConfigDir + "/FOLLOW"
	lastFollowExists := false
	lastFollowContent := ""

	// Initialize FOLLOW file state
	if followData, err := os.ReadFile(followPath); err == nil {
		lastFollowExists = true
		lastFollowContent = string(followData)
	}

	p.LogFunc(logging.LevelDebug, "WATCH", "Starting ConfigWatcher, polling every %v (monitoring config and FOLLOW file)", pollingInterval)
	timer := time.NewTicker(pollingInterval)
	defer timer.Stop()

	// Conditionally include the test channel in the select statement.
	forceCheckCh := make(chan struct{}) // A dummy channel that is never written to.
	if p.TestSignals != nil {
		forceCheckCh = p.TestSignals.ForceCheckSignal
	}

	for {
		select {
		case <-stop:
			p.LogFunc(logging.LevelInfo, "WATCH", "ConfigWatcher received stop signal. Shutting down.")
			return
		case <-forceCheckCh:
			// This case is for testing only, to trigger an immediate check.
			if p.TestSignals != nil { // Double-check for safety, though it should always be true here.
				p.LogFunc(logging.LevelDebug, "WATCH", "Received test signal for immediate reload check.")
			}
		case <-timer.C:
			// Timer fired, continue with polling.
		}

		// Check FOLLOW file for role changes
		followExists := false
		followContent := ""
		if followData, err := os.ReadFile(followPath); err == nil {
			followExists = true
			followContent = string(followData)
		}

		// Detect FOLLOW file changes
		if followExists != lastFollowExists {
			if followExists {
				p.LogFunc(logging.LevelWarning, "CLUSTER_ROLE", "FOLLOW file created - node role changed to FOLLOWER (leader: %s). Restart required for role switch.", strings.TrimSpace(followContent))
			} else {
				p.LogFunc(logging.LevelWarning, "CLUSTER_ROLE", "FOLLOW file deleted - node role changed to LEADER. Restart required for role switch.")
			}
			lastFollowExists = followExists
			lastFollowContent = followContent
		} else if followExists && followContent != lastFollowContent {
			p.LogFunc(logging.LevelWarning, "CLUSTER_ROLE", "FOLLOW file modified - leader address changed to: %s. Restart required to apply changes.", strings.TrimSpace(followContent))
			lastFollowContent = followContent
		}

		// Clone the current config *before* checking for changes in file dependencies.
		// This ensures that oldConfigForComparison accurately represents the state before any updates.
		p.ConfigMutex.RLock()
		oldConfigForComparison := p.Config.Clone()
		p.ConfigMutex.RUnlock()

		isChanged := false
		changedFile := ""
		mainFileChanged := false

		// 1. Check the main YAML file
		fileInfo, err := os.Stat(p.ConfigPath)
		if err != nil {
			p.LogFunc(logging.LevelError, "WATCH_ERROR", "Failed to stat file %s: %v", p.ConfigPath, err)
			continue
		}

		p.ConfigMutex.RLock()
		if fileInfo.ModTime().After(p.Config.LastModTime) {
			isChanged = true
			mainFileChanged = true
			changedFile = p.ConfigPath
		} else {
			// 2. Check all file dependencies if YAML hasn't changed
			for path, fileDep := range p.Config.FileDependencies {
				fileDep.UpdateStatus()
				if fileDep.HasChanged() {
					isChanged = true
					changedFile = path
					break
				}
			}
		}
		p.ConfigMutex.RUnlock()

		if isChanged {
			p.LogFunc(logging.LevelInfo, "WATCH", "Detected change in '%s'. Attempting reload...", changedFile)
			func() { // Use an anonymous function to scope the defer correctly.
				defer func() {
					if r := recover(); r != nil {
						p.LogFunc(logging.LevelError, "WATCH_PANIC", "Recovered from panic during config reload: %v", r)
					}
				}()
				// Defer the test signal to ensure it's sent whether the reload succeeds or fails.
				if p.TestSignals != nil && p.TestSignals.ReloadDoneSignal != nil {
					defer func() { p.TestSignals.ReloadDoneSignal <- struct{}{} }()
				}
				ReloadConfiguration(p, mainFileChanged, &oldConfigForComparison)
			}()
		}
	}
}
