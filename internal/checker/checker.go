package checker

import (
	"bot-detector/internal/app"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
	"fmt"
	"sort"
	"sync/atomic"
	"time"
)

// GetActor constructs the correct store.Actor key for a given log entry based on the chain's match_key.
// It handles IP version filtering and decides whether to include the User-Agent in the key.
// If the entry's IP version doesn't match the key (e.g., ipv4 vs ipv6), it returns an empty store.Actor.
func GetActor(chain *config.BehavioralChain, entry *app.LogEntry) store.Actor {
	ipVersion := entry.IPInfo.Version
	useUA := false

	switch chain.MatchKey {
	case "ip":
		// Any IP version is fine.
	case "ipv4":
		if ipVersion != utils.VersionIPv4 {
			return store.Actor{} // Mismatch, return empty actor
		}
	case "ipv6":
		if ipVersion != utils.VersionIPv6 {
			return store.Actor{} // Mismatch, return empty actor
		}
	case "ip_ua":
		useUA = true
	case "ipv4_ua":
		if ipVersion != utils.VersionIPv4 {
			return store.Actor{}
		}
		useUA = true
	case "ipv6_ua":
		if ipVersion != utils.VersionIPv6 {
			return store.Actor{}
		}
		useUA = true
	}

	if useUA {
		return store.Actor{IPInfo: entry.IPInfo, UA: entry.UserAgent}
	}
	return store.Actor{IPInfo: entry.IPInfo}
}

// GetMatchValue retrieves the field value from a LogEntry based on the field name.

// preCheckActivity performs initial checks on an actor before processing against chains.
// It returns the relevant ActorActivity and a boolean indicating if further processing should be skipped.
// The caller is responsible for locking/unlocking the ActivityMutex. It returns the store.SkipInfo if applicable.
func preCheckActivity(p *app.Processor, entry *app.LogEntry, actor store.Actor) (*store.ActorActivity, bool, store.SkipInfo) {
	// 2. Get or create actor activity and check for existing blocks.
	activity := store.GetOrCreateUnsafe(p.ActivityStore, store.Actor(actor))

	// If a skip reason is already set (e.g., from a previous good_actor match),
	// honor it and skip immediately.
	if activity.SkipInfo.Type == utils.SkipTypeGoodActor {
		return activity, true, activity.SkipInfo
	}

	if activity.IsBlocked {
		if time.Now().After(activity.BlockedUntil) {
			// Block has expired, clear it and proceed.
			p.LogFunc(logging.LevelDebug, "EXPIRE", "Chain-specific block expired for actor %s (UA: %s).", actor.IPInfo.Address, actor.UA)
			activity.IsBlocked = false
			activity.SkipInfo = store.SkipInfo{} // Clear the skip info.
			activity.BlockedUntil = time.Time{}
		} else {
			// Still blocked, update timestamp and skip.
			if entry.Timestamp.After(activity.LastRequestTime) {
				activity.LastRequestTime = entry.Timestamp
			}
			// Only log the skip message the first time.
			if activity.SkipInfo.Type != utils.SkipTypeBlocked { // This ensures we only log once per block.
				// The source is already set when the block was applied.
				p.LogFunc(logging.LevelDebug, "SKIP", "store.Actor %s (UA: %s): Skipped (blocked:%s).", actor.IPInfo.Address, entry.UserAgent, activity.SkipInfo.Source)
			}
			return activity, true, activity.SkipInfo
		}
	}
	return activity, false, store.SkipInfo{} // No skip, return empty SkipInfo
}

// isGoodActor checks if a log entry matches any of the configured "good actor" definitions.
// It returns true and the reason string if a match is found.
// This function is thread-safe and handles its own locking.
func isGoodActor(p *app.Processor, entry *app.LogEntry) (bool, string) {
	p.ConfigMutex.RLock()
	defer p.ConfigMutex.RUnlock()
	goodActors := p.Config.GoodActors
	if len(goodActors) == 0 {
		return false, ""
	}

	for _, def := range goodActors {
		// A rule with no matchers is invalid and should be skipped.
		if len(def.IPMatchers) == 0 && len(def.UAMatchers) == 0 {
			continue
		}

		// If a matcher is defined for a field, it must match.
		// If a matcher is NOT defined, it is considered a match for that field.
		ipMatch := (len(def.IPMatchers) == 0) || (def.IPMatchers[0](entry))
		uaMatch := (len(def.UAMatchers) == 0) || (def.UAMatchers[0](entry))

		// For a rule to apply, all defined matchers must succeed.
		if ipMatch && uaMatch {
			return true, def.Name
		}
	}
	return false, ""
}

// addToOooBuffer inserts a log entry into the out-of-order buffer while maintaining
// the buffer's sorted order by timestamp. This is more efficient than appending
// and re-sorting the entire buffer later.
func addToOooBuffer(p *app.Processor, entry *app.LogEntry) {
	// Find the correct insertion point using binary search.
	i := sort.Search(len(p.EntryBuffer), func(i int) bool {
		return p.EntryBuffer[i].Timestamp.After(entry.Timestamp)
	})

	// Grow the slice by one.
	p.EntryBuffer = append(p.EntryBuffer, nil)
	// Shift elements to the right to make space for the new entry.
	copy(p.EntryBuffer[i+1:], p.EntryBuffer[i:])
	// Insert the new entry at the correct position.
	p.EntryBuffer[i] = entry

	p.Metrics.ReorderedEntries.Add(1)
}

// nextOooCandidate checks the out-of-order buffer for the next entry that is ready
// to be processed. An entry is ready if its timestamp is older than the processing horizon.
// If a candidate is found, it is removed from the buffer and returned.
func nextOooCandidate(p *app.Processor, processingHorizon time.Time) *app.LogEntry {
	if len(p.EntryBuffer) == 0 {
		return nil // Buffer is empty.
	}

	// Since the buffer is sorted, the oldest entry is always at the front.
	candidate := p.EntryBuffer[0]
	if candidate.Timestamp.Before(processingHorizon) {
		p.EntryBuffer = p.EntryBuffer[1:] // Dequeue the entry.
		return candidate
	}

	return nil // No entries are old enough to be processed yet.
}

// flushOooBuffer processes all entries currently in the out-of-order buffer and clears it.
// This is typically used during shutdown to ensure no entries are lost.
func flushOooBuffer(p *app.Processor) {
	if len(p.EntryBuffer) == 0 {
		return
	}
	// Create a copy to avoid holding the lock during processing.
	toProcess := make([]*app.LogEntry, len(p.EntryBuffer))
	copy(toProcess, p.EntryBuffer)
	p.EntryBuffer = nil // Clear the buffer immediately.

	// The slice is already sorted, so we can process it directly.
	for _, entry := range toProcess {
		checkChainsInternal(p, entry)
	}
}

// shouldBufferOutOfOrder determines if an incoming log entry is out-of-order and within the
// configured tolerance window, indicating it should be buffered instead of processed immediately.
func shouldBufferOutOfOrder(lastRequestTime, entryTimestamp time.Time, tolerance time.Duration) bool {
	return !lastRequestTime.IsZero() && entryTimestamp.Before(lastRequestTime) && lastRequestTime.Sub(entryTimestamp) <= tolerance
}

// handleOutOfOrder decides whether to process an entry immediately or buffer it based on its timestamp
// relative to the last seen request for the same actor. It assumes the caller holds the ActivityMutex.
// It returns true if the entry was buffered, and false if it was processed immediately.
func handleOutOfOrder(p *app.Processor, entry *app.LogEntry) (buffered bool) {
	p.ConfigMutex.RLock()
	tolerance := p.Config.Parser.OutOfOrderTolerance
	p.ConfigMutex.RUnlock()

	if tolerance == 0 {
		// If tolerance is zero, process immediately without any buffering logic.
		checkChainsInternal(p, entry)
		return false
	}

	// We use a simple 'ip' key here as a proxy for the activity's last seen time.
	actor := store.Actor{IPInfo: entry.IPInfo}
	actorActivity := store.GetOrCreateUnsafe(p.ActivityStore, store.Actor(actor))
	lastRequestTime := actorActivity.LastRequestTime

	// If the entry is older than the last request we've seen for this IP,
	// and it's within the tolerance window, buffer it.
	if shouldBufferOutOfOrder(lastRequestTime, entry.Timestamp, tolerance) {
		addToOooBuffer(p, entry)
		return true // Indicate that the entry was buffered.
	}

	// If the entry is in-order (or the first one seen), process it immediately.
	checkChainsInternal(p, entry)

	// After processing a newer entry, signal the worker to check the buffer,
	// but only if there are entries in the buffer to process.
	if len(p.EntryBuffer) > 0 {
		p.SignalOooBufferFlush()
	}
	return false // Indicate that the entry was processed.
}

// handleChainCompletion takes action when a chain is completed (log, block, etc.).
// It updates the actor's state and metrics, and performs the configured action.
// It returns true if `on_match` is "stop", indicating that no further chains should be processed for this entry.
func handleChainCompletion(p *app.Processor, chain *config.BehavioralChain, entry *app.LogEntry, currentActivity *store.ActorActivity) bool {
	if p.EnableMetrics {
		if p.Metrics.ChainsCompleted != nil {
			if val, ok := p.Metrics.ChainsCompleted.Load(chain.Name); ok {
				if counter, ok := val.(*atomic.Int64); ok {
					counter.Add(1)
				}
			}
		}
	}

	// --- 1. Log the completion event ---
	logLevel := logging.LevelCritical
	// Removed IsTesting check to avoid import cycle with testutil
	// if testutil.IsTesting() {
	// 	logLevel = logging.LevelDebug
	// }

	if p.DryRun {
		logDryRunCompletion(p, chain, entry)
	} else {
		// In live mode, log the action taken.
		switch chain.Action {
		case "block":
			p.LogFunc(logLevel, "ALERT", "BLOCK! Chain: %s completed by IP %s. Blocking for %v%s", chain.Name, entry.IPInfo.Address, chain.BlockDuration, getOnMatchSuffix(chain))
		case "log":
			p.LogFunc(logLevel, "ALERT", "LOG! Chain: %s completed by IP %s. Action set to 'log'%s", chain.Name, entry.IPInfo.Address, getOnMatchSuffix(chain))
		}
	}

	// If in dry-run mode, record the completion for top actors summary.
	if p.TopN > 0 {
		// The ActivityMutex is already held by the caller.
		actor := GetActor(chain, entry)
		actorString := actor.String()
		if _, ok := p.TopActorsPerChain[chain.Name]; !ok {
			p.TopActorsPerChain[chain.Name] = make(map[string]*store.ActorStats)
		}
		if _, ok := p.TopActorsPerChain[chain.Name][actorString]; !ok {
			p.TopActorsPerChain[chain.Name][actorString] = &store.ActorStats{}
		}
		p.TopActorsPerChain[chain.Name][actorString].Completions++
	}
	// --- 2. Perform the action ---
	switch chain.Action {
	case "block":
		if p.EnableMetrics {
			p.Metrics.BlockActions.Add(1)
			// Increment the counter for the specific block duration used.
			if val, ok := p.Metrics.BlockDurations.Load(chain.BlockDuration); ok {
				if counter, ok := val.(*atomic.Int64); ok {
					counter.Add(1)
				}
			}
		}
		executeBlock(p, entry, chain)
		// Update the in-memory state to reflect the block for both live and dry runs.
		ipOnlyActor := store.Actor(store.Actor{IPInfo: entry.IPInfo, UA: ""})
		ipActivity := store.GetOrCreateUnsafe(p.ActivityStore, ipOnlyActor)
		ipActivity.IsBlocked = true                                                           // Mark as blocked_
		ipActivity.SkipInfo = store.SkipInfo{Type: utils.SkipTypeBlocked, Source: chain.Name} // Set SkipInfo
		ipActivity.BlockedUntil = time.Now().Add(chain.BlockDuration)

		currentActivity.IsBlocked = true                                                           // Mark as blocked
		currentActivity.SkipInfo = store.SkipInfo{Type: utils.SkipTypeBlocked, Source: chain.Name} // Set SkipInfo
		currentActivity.BlockedUntil = ipActivity.BlockedUntil
	case "log":
		if p.EnableMetrics {
			p.Metrics.LogActions.Add(1)
		}
	}

	// Return true if OnMatch is "stop" to halt further chain processing.
	return chain.OnMatch == "stop"
}

// executeBlock calls the external blocker unless in DryRun mode.
func executeBlock(p *app.Processor, entry *app.LogEntry, chain *config.BehavioralChain) {
	if p.PersistenceEnabled {
		p.PersistenceWg.Add(1)
		go func() {
			defer p.PersistenceWg.Done()
			p.PersistenceMutex.Lock()
			defer p.PersistenceMutex.Unlock()

			unblockTime := p.NowFunc().Add(chain.BlockDuration)
			event := &persistence.AuditEvent{
				Timestamp: p.NowFunc(),
				Event:     persistence.EventTypeBlock,
				IP:        entry.IPInfo.Address,
				Duration:  chain.BlockDuration,
				Reason:    chain.Name,
			}
			if err := persistence.WriteEventToJournal(p.JournalHandle, event); err != nil {
				p.LogFunc(logging.LevelError, "JOURNAL_FAIL", "Failed to write block event to journal for %s: %v", entry.IPInfo.Address, err)
			}

			// Update in-memory state
			p.ActiveBlocks[entry.IPInfo.Address] = persistence.ActiveBlockInfo{
				UnblockTime: unblockTime,
				Reason:      chain.Name,
			}
		}()
	}

	if p.DryRun { // Should not happen due to caller check, but safe to keep.
		return // Do not execute block in dry-run mode.
	}
	// Convert main.IPInfo to utils.IPInfo before calling the blocker.
	blockerIPInfo := utils.IPInfo{
		Address: entry.IPInfo.Address,
		Version: entry.IPInfo.Version,
	}
	if err := p.Blocker.Block(blockerIPInfo, chain.BlockDuration, chain.Name); err != nil {
		// The error is logged inside the HAProxyBlocker, but not the RateLimitedBlocker. Log it here for safety.
		p.LogFunc(logging.LevelError, "BLOCK_FAIL", "Failed to queue block command for %s: %v", entry.IPInfo.Address, err)
	}
}

// flushEntryBufferUnsafe contains the core logic for processing all entries in the buffer.
// It assumes the caller holds the ActivityMutex. This function is NOT thread-safe on its own.
func flushEntryBufferUnsafe(p *app.Processor) {
	if len(p.EntryBuffer) == 0 {
		return
	}
	flushOooBuffer(p)
}

// FlushEntryBuffer is a public wrapper to flush the OOO buffer, used on shutdown or EOF.
func FlushEntryBuffer(p *app.Processor) {
	if len(p.EntryBuffer) > 0 {
		p.LogFunc(logging.LevelDebug, "BUFFER_FLUSH", "Flushing %d buffered entries.", len(p.EntryBuffer))
	}
	p.ActivityMutex.Lock()
	defer p.ActivityMutex.Unlock()
	flushEntryBufferUnsafe(p)
}

// FlushGivenEntries processes a slice of entries in chronological order.
// It acquires the necessary lock and calls the internal processing function.
func FlushGivenEntries(p *app.Processor, entries []*app.LogEntry) {
	// Sort the entries to be processed by timestamp to ensure strict chronological order.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp.Before(entries[j].Timestamp)
	})

	p.ActivityMutex.Lock()
	defer p.ActivityMutex.Unlock()
	for _, entry := range entries {
		checkChainsInternal(p, entry)
	}
}

// entryBufferWorker is a background goroutine that processes log entries from the buffer
// in chronological order, respecting the out-of-order tolerance.

// logDryRunCompletion handles logging for completed chains in dry-run mode.
func logDryRunCompletion(p *app.Processor, chain *config.BehavioralChain, entry *app.LogEntry) {
	onMatchSuffix := getOnMatchSuffix(chain)
	switch chain.Action {
	case "block":
		p.LogFunc(logging.LevelInfo, "DRY_RUN", "BLOCK! Chain: %s completed by IP %s. Blocking for %v (DryRun)%s", chain.Name, entry.IPInfo.Address, chain.BlockDuration, onMatchSuffix)
	case "log":
		p.LogFunc(logging.LevelInfo, "DRY_RUN", "LOG! Chain: %s completed by IP %s. Action set to 'log' (DryRun)%s", chain.Name, entry.IPInfo.Address, onMatchSuffix)
	default:
		p.LogFunc(logging.LevelInfo, "DRY_RUN", "UNKNOWN_ACTION! Chain: %s completed by IP %s. Unrecognized action '%s' (DryRun)%s", chain.Name, entry.IPInfo.Address, chain.Action, onMatchSuffix)
	}
}

// matchStepFields checks if the fields of a log entry match the compiled matchers of a step.
// It returns true if all fields match, false otherwise.
func matchStepFields(p *app.Processor, chain *config.BehavioralChain, step *config.StepDef, entry *app.LogEntry) bool {
	// Iterate over the pre-compiled matcher functions.
	for _, matcher := range step.Matchers {
		if !matcher.Matcher(entry) { // Access the actual matcher function
			return false
		}
		// If metrics are enabled, increment the StepExecutionCounts for this step.
		if p.EnableMetrics {
			if p.Metrics.StepExecutionCounts != nil {
				stepIdentifier := fmt.Sprintf("step %d/%d of %s", step.Order, len(chain.Steps), chain.Name)
				if counter, ok := p.Metrics.StepExecutionCounts.Load(stepIdentifier); ok {
					if c, ok := counter.(*atomic.Int64); ok {
						c.Add(1)
					}
				} else {
					// If the step name is not yet in the map, add it.
					newCounter := new(atomic.Int64)
					newCounter.Add(1)
					p.Metrics.StepExecutionCounts.Store(stepIdentifier, newCounter)
				}
			}
		}
		// If Metrics are enabled and a match occurred, increment the field match counter for this chain.
		if p.EnableMetrics {
			if chain.FieldMatchCounts != nil {
				if counter, ok := chain.FieldMatchCounts.Load(matcher.FieldName); ok {
					if c, ok := counter.(*atomic.Int64); ok {
						c.Add(1)
					}
				}
			}
		}
	}
	return true
}

// getOnMatchSuffix is a small helper to generate the logging suffix.
func getOnMatchSuffix(chain *config.BehavioralChain) string {
	if chain.OnMatch == "stop" {
		return " (on_match: stop)"
	}
	return ""
}

// checkFirstStepTimeRule validates the `min_time_since_last_hit` rule for the first step of a chain.
// It returns true if the rule passes, and false otherwise.
func checkFirstStepTimeRule(step *config.StepDef, timeSinceLastHit time.Duration, previousRequestTime time.Time) bool {
	if step.MinTimeSinceLastHit > 0 {
		// The rule is active. It fails if the actor has been seen before (`!IsZero`)
		// and the time since the last hit is less than or equal to the minimum required.
		if !previousRequestTime.IsZero() && timeSinceLastHit <= step.MinTimeSinceLastHit {
			return false // Condition not met: actor was seen too recently.
		}
	}
	return true // Rule passes (or is not configured).
}

// handleTimeRuleReset logs the reason for a chain reset and updates metrics.
// This helper is used by checkInterStepTimeRules to reduce code duplication.
func handleTimeRuleReset(p *app.Processor, chain *config.BehavioralChain, entry *app.LogEntry, reason string, value time.Duration) {
	if p.EnableMetrics {
		if val, ok := p.Metrics.ChainsReset.Load(chain.Name); ok {
			if counter, ok := val.(*atomic.Int64); ok {
				counter.Add(1)
			}
		}
	}
	if p.TopN > 0 {
		actor := GetActor(chain, entry)
		actorString := actor.String()
		if _, ok := p.TopActorsPerChain[chain.Name]; !ok {
			p.TopActorsPerChain[chain.Name] = make(map[string]*store.ActorStats)
		}
		if _, ok := p.TopActorsPerChain[chain.Name][actorString]; !ok {
			p.TopActorsPerChain[chain.Name][actorString] = &store.ActorStats{}
		}
		p.TopActorsPerChain[chain.Name][actorString].Resets++
	}
	p.LogFunc(logging.LevelDebug, "RESET", "Chain %s: %s %v. Resetting.", chain.Name, reason, value)
}

// checkInterStepTimeRules validates `max_delay` and `min_delay` rules between steps.
// It returns true if the chain should be reset due to a time rule violation.
func checkInterStepTimeRules(p *app.Processor, chain *config.BehavioralChain, entry *app.LogEntry, step *config.StepDef, timeSinceLastStepHit time.Duration) bool {
	if step.MaxDelayDuration > 0 && timeSinceLastStepHit > step.MaxDelayDuration {
		handleTimeRuleReset(p, chain, entry, "MaxDelay exceeded", step.MaxDelayDuration)
		return true // Reset the chain.
	}
	if step.MinDelayDuration > 0 && timeSinceLastStepHit < step.MinDelayDuration {
		handleTimeRuleReset(p, chain, entry, "MinDelay not met", step.MinDelayDuration)
		return true // Reset the chain.
	}
	return false // No reset needed.
}

// processChainForEntry evaluates a single log entry against a single behavioral chain.
// It manages state transitions (advancing, resetting) and triggers completion handling.
// It returns true if the chain completed and its `on_match` rule was "stop".
func processChainForEntry(p *app.Processor, chain *config.BehavioralChain, entry *app.LogEntry, currentActivity *store.ActorActivity, previousRequestTime time.Time) bool {
	// If GetActor returns an empty actor, it's a mismatch for this chain (e.g., wrong IP version).
	if GetActor(chain, entry).IPInfo.Address == "" {
		return false
	}

	// Increment the counter for the match_key type.
	// This gives us metrics on which keying strategies are most active.
	if p.EnableMetrics {
		if counter, ok := p.Metrics.MatchKeyHits.Load(chain.MatchKey); ok {
			if c, ok := counter.(*atomic.Int64); ok {
				c.Add(1)
			}
		}
	}

	// Get the current state for this chain.
	state, exists := currentActivity.ChainProgress[chain.Name]
	if !exists {
		// Initialize state if it's the first time we're seeing this actor for this chain.
		state = store.StepState{CurrentStep: 0, LastMatchTime: time.Time{}}
	}

	// Use a loop to handle step progression and potential resets within a single log entry.
	// This is important for rules like max_delay that can cause a reset and then an immediate
	// re-evaluation of the first step.
	for {
		nextStepIndex := state.CurrentStep
		if nextStepIndex >= len(chain.Steps) {
			break // Chain already completed or no more steps.
		}
		step := chain.Steps[nextStepIndex]

		// --- TIME WINDOW CHECKS ---
		isFirstStep := state.CurrentStep == 0
		timeSinceLastHit := entry.Timestamp.Sub(previousRequestTime)
		timeSinceLastStepHit := entry.Timestamp.Sub(state.LastMatchTime)

		if isFirstStep {
			if !checkFirstStepTimeRule(&step, timeSinceLastHit, previousRequestTime) {
				break // First step time rule failed, stop processing this chain.
			}
		} else {
			// Inter-step (2nd+ step) time checks.
			if checkInterStepTimeRules(p, chain, entry, &step, timeSinceLastStepHit) {
				// The checkInterStepTimeRules function handles logging and metrics.
				// It returns true if a reset is needed.
				state.CurrentStep = 0
				continue // Restart check from step 0.
			}
		}

		// --- FIELD MATCHING ---
		if !matchStepFields(p, chain, &step, entry) { // Pass p and chain
			break // No match on this step, exit the `for {}` loop for this chain.
		}

		// --- STEP MATCHED ---
		// Increment the total hits counter for this chain.
		if p.EnableMetrics {
			if val, ok := p.Metrics.ChainsHits.Load(chain.Name); ok {
				if counter, ok := val.(*atomic.Int64); ok {
					counter.Add(1)
				}
			}
		}

		// If in dry-run mode, record the actor hit for top actors summary.
		// The ActivityMutex is already held by the caller (checkChainsWithLock).
		if p.TopN > 0 {
			// Re-create the actor specific to this chain to get the correct actor string.
			actor := GetActor(chain, entry)
			actorString := actor.String()
			if _, ok := p.TopActorsPerChain[chain.Name]; !ok {
				p.TopActorsPerChain[chain.Name] = make(map[string]*store.ActorStats)
			}
			if _, ok := p.TopActorsPerChain[chain.Name][actorString]; !ok {
				p.TopActorsPerChain[chain.Name][actorString] = &store.ActorStats{}
			}
			p.TopActorsPerChain[chain.Name][actorString].Hits++
		}

		state.CurrentStep++
		state.LastMatchTime = entry.Timestamp

		// --- CHECK FOR CHAIN COMPLETION ---
		if state.CurrentStep == len(chain.Steps) {
			stopProcessing := handleChainCompletion(p, chain, entry, currentActivity)
			// Reset state after action is taken. This is critical.
			// By setting CurrentStep to 0, we ensure the state is cleaned up
			// by the logic at the end of this function.
			state.CurrentStep = 0
			// We must also delete the progress here because the function returns early.
			// The cleanup logic at the end of the function will not be reached.
			delete(currentActivity.ChainProgress, chain.Name)
			return stopProcessing
		}

		// If the chain did not complete, or if it completed and was reset,
		// break from the loop to save the new state.
		break
	}

	// --- STATE MANAGEMENT ---
	// Only store the state if the actor is actively progressing (CurrentStep > 0).
	// This saves memory by not storing state for actors that are at step 0.
	if state.CurrentStep > 0 {
		currentActivity.ChainProgress[chain.Name] = state
	} else {
		// If CurrentStep is 0 (due to reset or completion), clean up the state to save memory.
		// It's safe to call delete even if the key doesn't exist.
		delete(currentActivity.ChainProgress, chain.Name)
	}
	return false
}

// checkChainsInternal is the core logic for checking an entry against all chains. It's a variable to allow mocking in tests.
var checkChainsInternal = func(p *app.Processor, entry *app.LogEntry) {
	// This function is now called by checkChainsWithLock, which acquires the lock.
	// The original lock acquisition has been moved up to the caller.

	// --- The original logic of the function remains, but without the lock/defer unlock ---

	// If we've reached this point, the line was successfully parsed.
	// This is a "valid hit" that will be processed against the chains.
	if p.EnableMetrics {
		p.Metrics.ValidHits.Add(1)
	}

	// A set to keep track of activities that have been processed for this entry.
	// This is crucial to ensure that LastRequestTime is updated only once per activity,
	// even if multiple chains map to the same actor.
	processedActivities := make(map[*store.ActorActivity]struct{})

	// 2. Iterate over all configured chains.
	for _, chain := range p.Chains {
		actor := GetActor(&chain, entry)
		if actor.IPInfo.Address == "" {
			continue // Skip chain if actor could not be determined (e.g., IP version mismatch).
		}

		// Perform pre-checks for existing blocks.
		currentActivity, skip, _ := preCheckActivity(p, entry, actor) // _ for SkipInfo, as metric is handled in CheckChains
		if skip {
			// The skip reason metric is now handled in CheckChains, so we just continue.
			continue
		}

		// Capture the last request time *before* any potential updates.
		// This is the correct value to use for all time-based checks for this entry.
		previousRequestTime := currentActivity.LastRequestTime

		stop := processChainForEntry(p, &chain, entry, currentActivity, previousRequestTime)

		// Mark this activity as processed for this entry.
		processedActivities[currentActivity] = struct{}{}

		if stop {
			break // Stop processing other chains if requested.
		}
	}

	// After all chains have been processed for this entry, update the LastRequestTime
	// for all unique activities that were involved.
	for activity := range processedActivities {
		activity.LastRequestTime = entry.Timestamp
	}

}

// doSignalOooBufferFlush sends a non-blocking signal to the entryBufferWorker to trigger an immediate flush.
func doSignalOooBufferFlush(p *app.Processor) {
	select {
	case p.OooBufferFlushSignal <- struct{}{}: // Send a signal if the channel is not full.
	default: // Channel is full, a flush is already pending. Do nothing.
	}
}

// CheckChains is the entry point for processing a log entry.
// If out-of-order tolerance is configured, it buffers the entry. Otherwise, it processes immediately.
func CheckChains(p *app.Processor, entry *app.LogEntry) {
	// This function now acts as a gatekeeper, performing pre-checks before handing
	// off to the out-of-order handler. The logic for buffering or immediate processing
	// is now encapsulated in handleOutOfOrder.

	p.ActivityMutex.Lock()
	defer p.ActivityMutex.Unlock()

	// Create the base actor for this entry. This is used for good_actor checks,
	// pre-checks, and out-of-order buffering logic.
	actor := store.Actor{IPInfo: entry.IPInfo}
	activity := store.GetOrCreateUnsafe(p.ActivityStore, store.Actor(actor))

	// 1. First, check if the entry is a good actor. This will set the SkipInfo on the actor's activity.
	isGood, goodActorRuleName := isGoodActor(p, entry)
	if isGood {
		// Only log the skip message and set the state the first time.
		if activity.SkipInfo.Type == utils.SkipTypeNone {
			activity.SkipInfo = store.SkipInfo{Type: utils.SkipTypeGoodActor, Source: goodActorRuleName}
			p.LogFunc(logging.LevelDebug, "SKIP", "store.Actor %s (UA: %s): Skipped (good_actor:%s).", entry.IPInfo.Address, entry.UserAgent, goodActorRuleName)
		}

		// --- UNBLOCK ON GOOD ACTOR LOGIC ---
		// This logic is placed here to ensure it runs for every good actor match,
		// even if the skip message has already been logged.
		p.ConfigMutex.RLock()
		unblockEnabled := p.Config.Checker.UnblockOnGoodActor
		unblockCooldown := p.Config.Checker.UnblockCooldown
		p.ConfigMutex.RUnlock()

		if unblockEnabled {
			// Use the existing activity for the IP-only actor.
			// The lock is already held by the caller (CheckChains).
			activity := store.GetOrCreateUnsafe(p.ActivityStore, store.Actor(actor))

			// Check if the cooldown has passed or if it has never been unblocked.
			if activity.LastUnblockTime.IsZero() || time.Since(activity.LastUnblockTime) > unblockCooldown {
				p.LogFunc(logging.LevelInfo, "UNBLOCK", "Good actor match for %s. Issuing unblock command.", entry.IPInfo.Address)

				if p.PersistenceEnabled {
					func() {
						p.PersistenceMutex.Lock()
						defer p.PersistenceMutex.Unlock()

						event := &persistence.AuditEvent{
							Timestamp: p.NowFunc(),
							Event:     persistence.EventTypeUnblock,
							IP:        entry.IPInfo.Address,
							Reason:    "good-actor-match",
						}
						if err := persistence.WriteEventToJournal(p.JournalHandle, event); err != nil {
							p.LogFunc(logging.LevelError, "JOURNAL_FAIL", "Failed to write unblock event to journal for %s: %v", entry.IPInfo.Address, err)
						}

						// Update in-memory state
						delete(p.ActiveBlocks, entry.IPInfo.Address)
					}()
				}

				// Convert main.IPInfo to utils.IPInfo before calling the blocker.
				blockerIPInfo := utils.IPInfo{
					Address: entry.IPInfo.Address,
					Version: entry.IPInfo.Version,
				}
				// The blocker's Unblock method is non-blocking and rate-limited.
				if err := p.Blocker.Unblock(blockerIPInfo, "good-actor-match"); err != nil {
					p.LogFunc(logging.LevelError, "UNBLOCK_FAIL", "Failed to queue unblock command for %s: %v", entry.IPInfo.Address, err)
				}
				activity.LastUnblockTime = time.Now()
			}
		}
	}

	// 2. Now, perform the pre-check for any existing skip reasons (good_actor or blocked).
	// This is the single point where we increment the per-reason skip metrics for all skips.
	_, skip, skipInfo := preCheckActivity(p, entry, actor)
	if skip {
		if skipInfo.Type != utils.SkipTypeNone {
			// Construct the reason string based on SkipInfo.Type for logging and metrics.
			// This ensures consistency and avoids string parsing.
			var reasonStr string
			if p.EnableMetrics {
				switch skipInfo.Type {
				case utils.SkipTypeGoodActor:
					reasonStr = fmt.Sprintf("good_actor:%s", skipInfo.Source)
					p.Metrics.GoodActorsSkipped.Add(1)
					if val, ok := p.Metrics.GoodActorHits.Load(skipInfo.Source); ok { // This map is pre-populated
						if counter, ok := val.(*atomic.Int64); ok {
							counter.Add(1)
						}
					}
				case utils.SkipTypeBlocked:
					reasonStr = fmt.Sprintf("blocked:%s", skipInfo.Source)
				}
				if reasonStr != "" {
					// Increment the counter for the specific skip reason.
					val, _ := p.Metrics.SkipsByReason.LoadOrStore(reasonStr, new(atomic.Int64))
					if counter, ok := val.(*atomic.Int64); ok {
						counter.Add(1)
					}
				}
			}
		}
		return // Skip all further processing for this entry.
	}

	// Delegate to the out-of-order handler, which will decide to buffer or process.
	handleOutOfOrder(p, entry) // This now calls p.SignalOooBufferFlush() internally.
}

// entryBufferWorker is a background goroutine that processes log entries from the buffer
// in chronological order, respecting the out-of-order tolerance.
func entryBufferWorker(p *app.Processor, stop <-chan struct{}) {
	// Use a ticker that is half the tolerance duration for responsiveness,
	// with a minimum floor to prevent busy-looping.
	p.ConfigMutex.RLock()
	tolerance := p.Config.Parser.OutOfOrderTolerance
	p.ConfigMutex.RUnlock()

	if tolerance == 0 {
		p.LogFunc(logging.LevelDebug, "BUFFER_WORKER", "Out-of-order tolerance is zero, buffer worker is disabled.")
		return // Do not run the worker if buffering is disabled.
	}

	tickerInterval := tolerance / 2
	if tickerInterval < 50*time.Millisecond {
		tickerInterval = 50 * time.Millisecond
	}

	ticker := time.NewTicker(tickerInterval)
	defer ticker.Stop()

	p.LogFunc(logging.LevelDebug, "BUFFER_WORKER", "Starting entry buffer worker with tolerance %v (tick interval %v).", tolerance, tickerInterval)

	for {
		select {
		case <-stop:
			p.LogFunc(logging.LevelInfo, "BUFFER_WORKER", "Shutting down. Processing remaining %d entries in buffer...", len(p.EntryBuffer))
			// On shutdown, process all remaining entries in the buffer immediately.
			FlushEntryBuffer(p) // Use the public, locking version.
			return

		case <-p.OooBufferFlushSignal:
			// An immediate flush was requested by a newer log entry.
			p.LogFunc(logging.LevelDebug, "BUFFER_WORKER", "Immediate flush triggered.")
			// Fall through to process the buffer.

		case <-ticker.C:
			// Ticker fired, time to process the buffer.
		}

		// --- Buffer Processing Logic ---
		// This block is executed on a ticker or an immediate flush signal.
		p.ActivityMutex.Lock()

		// Determine the processing horizon. Entries older than this are safe to process.
		processingHorizon := p.NowFunc().Add(-tolerance)

		// Ensure the buffer is sorted by timestamp before processing. This is crucial
		// because entries can be added from tests or other sources in an unsorted state.
		sort.Slice(p.EntryBuffer, func(i, j int) bool {
			return p.EntryBuffer[i].Timestamp.Before(p.EntryBuffer[j].Timestamp)
		})
		// Process all candidates that are ready by repeatedly calling nextOooCandidate.
		var processedInTick []*app.LogEntry
		for {
			if entry := nextOooCandidate(p, processingHorizon); entry != nil {
				processedInTick = append(processedInTick, entry)
			} else {
				break // No more candidates ready.
			}
		}

		p.ActivityMutex.Unlock()

		// Process the entries outside the main lock to reduce contention.
		if len(processedInTick) > 0 {
			FlushGivenEntries(p, processedInTick)
		}

		// Signal for tests that a tick has been processed.
		// Removed IsTesting check to avoid import cycle with testutil
		// if testutil.IsTesting() {
		// 	// Use a very specific tag that the test harness can listen for.
		// 	p.LogFunc(logging.LevelDebug, "BUFFER_WORKER_TICK_DONE", "Tick processed.")
		// }
	}
}
