package main

import (
	"bot-detector/internal/logging"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// GetMatchValue retrieves the field value from a LogEntry based on the field name.
func GetMatchValue(fieldName string, entry *LogEntry) (interface{}, FieldType, error) {
	// If entry is nil, this is a compile-time check for the field's type.
	if entry == nil {
		entry = &LogEntry{} // Use a zero-value entry to get the type.
	}

	switch fieldName {
	case "IP":
		return entry.IPInfo.Address, StringField, nil
	case "Path":
		return entry.Path, StringField, nil
	case "Method":
		return entry.Method, StringField, nil
	case "Protocol":
		return entry.Protocol, StringField, nil
	case "UserAgent":
		return entry.UserAgent, StringField, nil
	case "Referrer":
		return entry.Referrer, StringField, nil
	case "StatusCode":
		return entry.StatusCode, IntField, nil
	case "Size":
		return entry.Size, IntField, nil
	case "VHost":
		return entry.VHost, StringField, nil
	default:
		return nil, UnsupportedField, fmt.Errorf("unknown field: '%s'", fieldName)
	}
}

// GetMatchValueIfType retrieves a field's value only if it matches the expected type.
// It returns the value, or nil if the type doesn't match or an error occurs.
func GetMatchValueIfType(fieldName string, entry *LogEntry, expectedType FieldType) interface{} {
	value, actualType, err := GetMatchValue(fieldName, entry)
	if err != nil || actualType != expectedType {
		return nil
	}
	return value
}

// preCheckActivity performs initial checks on an actor before processing against chains.
// It returns the relevant ActorActivity and a boolean indicating if further processing should be skipped.
// The caller is responsible for locking/unlocking the ActivityMutex.
func preCheckActivity(p *Processor, entry *LogEntry, actor Actor) (*ActorActivity, bool) {
	// 2. Get or create actor activity and check for existing blocks.
	activity := GetOrCreateActorActivityUnsafe(p.ActivityStore, actor)

	if activity.IsBlocked {
		if time.Now().After(activity.BlockedUntil) {
			// Block has expired, clear it and proceed.
			p.LogFunc(logging.LevelInfo, "EXPIRE", "Chain-specific block expired for actor %s (UA: %s).", actor.IPInfo.Address, actor.UA)
			activity.IsBlocked = false
			activity.BlockedUntil = time.Time{}
		} else {
			// Still blocked, update timestamp and skip.
			if entry.Timestamp.After(activity.LastRequestTime) {
				activity.LastRequestTime = entry.Timestamp
			}
			p.LogFunc(logging.LevelDebug, "SKIP", "Actor %s (UA: %s): Skipped (Already blocked in memory by a chain).", actor.IPInfo.Address, actor.UA)
			return activity, true // Skip processing
		}
	}

	return activity, false // Do not skip
}

// handleChainCompletion takes action when a chain is completed (log, block, etc.).
// It updates the actor's state and returns true if the chain was completed.
// It returns true if processing of other chains should be stopped for this log entry.
func handleChainCompletion(p *Processor, chain *BehavioralChain, entry *LogEntry, currentActivity *ActorActivity) bool {
	// Increment the counter for the specific chain that was completed.
	// This is the equivalent of `chains_completed_total{chain="<name>"}`.
	if val, ok := p.Metrics.ChainsCompleted.Load(chain.Name); ok {
		if counter, ok := val.(*atomic.Int64); ok {
			counter.Add(1)
		}
	}

	// --- 1. Log the completion event ---
	logLevel := logging.LevelCritical
	if IsTesting() {
		logLevel = logging.LevelDebug
	}

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
	if p.DryRun {
		// The ActivityMutex is already held by the caller.
		actor := GetActor(chain, entry)
		actorString := actor.String()
		if _, ok := p.TopActorsPerChain[chain.Name]; !ok {
			p.TopActorsPerChain[chain.Name] = make(map[string]*ActorStats)
		}
		if _, ok := p.TopActorsPerChain[chain.Name][actorString]; !ok {
			p.TopActorsPerChain[chain.Name][actorString] = &ActorStats{}
		}
		p.TopActorsPerChain[chain.Name][actorString].Completions++
	}
	// --- 2. Perform the action ---
	if chain.Action == "block" {
		p.Metrics.BlockActions.Add(1)
		// Increment the counter for the specific block duration used.
		if val, ok := p.Metrics.BlockDurations.Load(chain.BlockDuration); ok {
			if counter, ok := val.(*atomic.Int64); ok {
				counter.Add(1)
			}
		}
		executeBlock(p, entry, chain)
		// Update the in-memory state to reflect the block for both live and dry runs.
		ipOnlyActor := Actor{IPInfo: entry.IPInfo, UA: ""}
		ipActivity := GetOrCreateActorActivityUnsafe(p.ActivityStore, ipOnlyActor)
		ipActivity.IsBlocked = true
		ipActivity.BlockedUntil = time.Now().Add(chain.BlockDuration)

		currentActivity.IsBlocked = true
		currentActivity.BlockedUntil = ipActivity.BlockedUntil
	} else if chain.Action == "log" {
		p.Metrics.LogActions.Add(1)
	}

	// Return true if OnMatch is "stop" to halt further chain processing.
	return chain.OnMatch == "stop"
}

// executeBlock calls the external blocker unless in DryRun mode.
func executeBlock(p *Processor, entry *LogEntry, chain *BehavioralChain) {
	if p.DryRun {
		return
	}
	if err := p.Blocker.Block(entry.IPInfo, chain.BlockDuration); err != nil {
		// Error is logged inside Block, no action needed here.
	}
}

// flushEntryBufferUnsafe contains the core logic for processing all entries in the buffer.
// It assumes the caller holds the ActivityMutex. This function is NOT thread-safe on its own.
func flushEntryBufferUnsafe(p *Processor) {
	if len(p.EntryBuffer) == 0 {
		return
	}
	sort.Slice(p.EntryBuffer, func(i, j int) bool {
		return p.EntryBuffer[i].Timestamp.Before(p.EntryBuffer[j].Timestamp)
	})
	for _, entry := range p.EntryBuffer {
		checkChainsInternal(p, entry)
	}
	p.EntryBuffer = nil // Clear the buffer.
}

// FlushEntryBuffer checks the entry buffer and processes any entries that are older
// than the out-of-order tolerance, which is useful when log processing is paused (e.g., at EOF).
func FlushEntryBuffer(p *Processor) {
	if len(p.EntryBuffer) > 0 {
		p.LogFunc(logging.LevelDebug, "BUFFER_FLUSH", "Flushing %d buffered entries.", len(p.EntryBuffer))
	}
	p.ActivityMutex.Lock()
	defer p.ActivityMutex.Unlock()
	flushEntryBufferUnsafe(p)
}

// entryBufferWorker is a background goroutine that processes log entries from the buffer
// in chronological order, respecting the out-of-order tolerance.

// logDryRunCompletion handles logging for completed chains in dry-run mode.
func logDryRunCompletion(p *Processor, chain *BehavioralChain, entry *LogEntry) {
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
func matchStepFields(step *StepDef, entry *LogEntry) bool {
	// Iterate over the pre-compiled matcher functions.
	for _, matcher := range step.Matchers {
		if !matcher(entry) {
			return false
		}
	}
	return true
}

// getOnMatchSuffix is a small helper to generate the logging suffix.
func getOnMatchSuffix(chain *BehavioralChain) string {
	if chain.OnMatch == "stop" {
		return " (on_match: stop)"
	}
	return ""
}

// processChainForEntry evaluates a single log entry against a single behavioral chain.
// It manages state transitions (advancing, resetting) and triggers completion handling.
// It returns true if processing of other chains should be stopped for this entry.
func processChainForEntry(p *Processor, chain *BehavioralChain, entry *LogEntry, currentActivity *ActorActivity, previousRequestTime time.Time) bool {
	// If GetActor returns an empty actor, it's a mismatch for this chain (e.g., wrong IP version).
	if GetActor(chain, entry).IPInfo.Address == "" {
		return false
	}

	// Increment the counter for the match_key type.
	// This gives us metrics on which keying strategies are most active.
	if counter, ok := p.Metrics.MatchKeyHits.Load(chain.MatchKey); ok {
		if c, ok := counter.(*atomic.Int64); ok {
			c.Add(1)
		}
	}

	// Get the current state for this chain.
	state, exists := currentActivity.ChainProgress[chain.Name]
	if !exists {
		// Initialize state if it's the first time we're seeing this actor for this chain.
		state = StepState{CurrentStep: 0, LastMatchTime: time.Time{}}
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
			// First-step specific checks
			if step.MinTimeSinceLastHit > 0 {
				if previousRequestTime.IsZero() || timeSinceLastHit <= step.MinTimeSinceLastHit {
					break // Condition not met: IP is new or was seen too recently.
				}
			}
		} else {
			// Inter-step (2nd step onwards) checks
			if step.MaxDelayDuration > 0 && timeSinceLastStepHit > step.MaxDelayDuration {
				if val, ok := p.Metrics.ChainsReset.Load(chain.Name); ok {
					if counter, ok := val.(*atomic.Int64); ok {
						counter.Add(1)
					}
				}
				if p.DryRun {
					// Re-create the actor specific to this chain to get the correct actor string.
					actor := GetActor(chain, entry)
					actorString := actor.String()
					if _, ok := p.TopActorsPerChain[chain.Name]; !ok {
						p.TopActorsPerChain[chain.Name] = make(map[string]*ActorStats)
					}
					if _, ok := p.TopActorsPerChain[chain.Name][actorString]; !ok {
						p.TopActorsPerChain[chain.Name][actorString] = &ActorStats{}
					}
					p.TopActorsPerChain[chain.Name][actorString].Resets++
				}
				p.LogFunc(logging.LevelDebug, "RESET", "Chain %s: MaxDelay %v exceeded. Resetting.", chain.Name, step.MaxDelayDuration)
				state.CurrentStep = 0
				continue // Restart check from step 0.
			}
			if step.MinDelayDuration > 0 && timeSinceLastStepHit < step.MinDelayDuration {
				if val, ok := p.Metrics.ChainsReset.Load(chain.Name); ok {
					if counter, ok := val.(*atomic.Int64); ok {
						counter.Add(1)
					}
				}
				if p.DryRun {
					// Re-create the actor specific to this chain to get the correct actor string.
					actor := GetActor(chain, entry)
					actorString := actor.String()
					if _, ok := p.TopActorsPerChain[chain.Name]; !ok {
						p.TopActorsPerChain[chain.Name] = make(map[string]*ActorStats)
					}
					if _, ok := p.TopActorsPerChain[chain.Name][actorString]; !ok {
						p.TopActorsPerChain[chain.Name][actorString] = &ActorStats{}
					}
					p.TopActorsPerChain[chain.Name][actorString].Resets++
				}
				p.LogFunc(logging.LevelDebug, "RESET", "Chain %s: MinDelay %v not met. Resetting.", chain.Name, step.MinDelayDuration)
				state.CurrentStep = 0
				continue // Restart check from step 0.
			}
		}

		// --- FIELD MATCHING ---
		if !matchStepFields(&step, entry) {
			break // No match on this step, exit the `for {}` loop for this chain.
		}

		// --- STEP MATCHED ---
		// Increment the total hits counter for this chain.
		if val, ok := p.Metrics.ChainsHits.Load(chain.Name); ok {
			if counter, ok := val.(*atomic.Int64); ok {
				counter.Add(1)
			}
		}

		// If in dry-run mode, record the actor hit for top actors summary.
		// The ActivityMutex is already held by the caller (checkChainsWithLock).
		if p.DryRun {
			// Re-create the actor specific to this chain to get the correct actor string.
			actor := GetActor(chain, entry)
			actorString := actor.String()
			if _, ok := p.TopActorsPerChain[chain.Name]; !ok {
				p.TopActorsPerChain[chain.Name] = make(map[string]*ActorStats)
			}
			if _, ok := p.TopActorsPerChain[chain.Name][actorString]; !ok {
				p.TopActorsPerChain[chain.Name][actorString] = &ActorStats{}
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
var checkChainsInternal = func(p *Processor, entry *LogEntry) {
	// This function is now called by checkChainsWithLock, which acquires the lock.
	// The original lock acquisition has been moved up to the caller.

	// --- The original logic of the function remains, but without the lock/defer unlock ---


	// If we've reached this point, the line was successfully parsed and was not whitelisted.
	// This is a "valid hit" that will be processed against the chains.
	p.Metrics.ValidHits.Add(1)
	// Determine the most specific actor key required by any applicable chain.
	primaryKeySpecificity := 0 // 0=none, 1=ip, 2=ip_ua
	p.ConfigMutex.RLock()
	chains := p.Chains
	for _, chain := range chains {
		if GetActor(&chain, entry).IPInfo.Address != "" { // Does this chain apply to this entry?
			if strings.HasSuffix(chain.MatchKey, "_ua") {
				primaryKeySpecificity = 2 // ip_ua is most specific
				break
			} else if primaryKeySpecificity < 1 {
				primaryKeySpecificity = 1 // ip is less specific
			}
		}
	}
	p.ConfigMutex.RUnlock()

	actor := Actor{IPInfo: entry.IPInfo, UA: ""}
	if primaryKeySpecificity == 2 {
		actor.UA = entry.UserAgent
	}

	// Perform pre-checks for whitelisting and existing blocks.
	currentActivity, skip := preCheckActivity(p, entry, actor)
	if skip {
		return
	}

	// Defer the update of LastRequestTime. This is CRITICAL.
	// It ensures that all time-based checks (like min_time_since_last_hit) for the current
	// entry use the timestamp from the *previous* request. The current entry's timestamp
	// only becomes the new LastRequestTime after all processing is complete.
	defer func() {
		if entry.Timestamp.After(currentActivity.LastRequestTime) {
			currentActivity.LastRequestTime = entry.Timestamp
		}
	}()

	// Capture the last request time *before* any potential updates.
	// This is the correct value to use for all time-based checks for this entry.
	previousRequestTime := currentActivity.LastRequestTime

	// 2. Iterate over all configured chains.
	for _, chain := range chains {
		if stop := processChainForEntry(p, &chain, entry, currentActivity, previousRequestTime); stop {
			break // Stop processing other chains if requested.
		}
	}

}

// checkChainsWithLock acquires the necessary lock and then calls the internal checking logic.
// This is the new entry point for single, immediate processing.
func checkChainsWithLock(p *Processor, entry *LogEntry) {
	p.ActivityMutex.Lock()
	defer p.ActivityMutex.Unlock()
	checkChainsInternal(p, entry)
}

// CheckChains is the entry point for processing a log entry.
// If out-of-order tolerance is configured, it buffers the entry. Otherwise, it processes immediately.
func CheckChains(p *Processor, entry *LogEntry) {
	p.ConfigMutex.RLock()
	tolerance := p.Config.OutOfOrderTolerance
	p.ConfigMutex.RUnlock()
	if tolerance == 0 {
		// If tolerance is zero, process immediately without any buffering logic.
		checkChainsWithLock(p, entry)
		return
	}

	p.ActivityMutex.Lock()
	defer p.ActivityMutex.Unlock()

	// Determine the actor key to find the last request time.
	// We use a simple 'ip' key here as a proxy for the activity's last seen time.
	// A more complex implementation might find the most specific key, but this is sufficient.
	actor := Actor{IPInfo: entry.IPInfo}
	actorActivity := GetOrCreateActorActivityUnsafe(p.ActivityStore, actor)
	lastRequestTime := actorActivity.LastRequestTime

	// If the entry is older than the last request we've seen for this IP,
	// and it's within the tolerance window, buffer it.
	if !lastRequestTime.IsZero() && entry.Timestamp.Before(lastRequestTime) && lastRequestTime.Sub(entry.Timestamp) <= tolerance {
		p.EntryBuffer = append(p.EntryBuffer, entry)
		p.Metrics.ReorderedEntries.Add(1)
		// Do not process it now. It will be processed by the worker or a subsequent newer entry.
		return
	}

	// If the entry is in-order (or the first one seen), process it immediately.
	checkChainsInternal(p, entry)

	// After processing a newer entry, re-process any buffered entries that might now be valid.
	// This is the key logic that was missing.
	// We call the unsafe version because we already hold the lock.
	flushEntryBufferUnsafe(p) // This call is now valid as the function is defined above.
}

// entryBufferWorker is a background goroutine that processes log entries from the buffer
// in chronological order, respecting the out-of-order tolerance.
func entryBufferWorker(p *Processor, stop <-chan struct{}) {
	// Use a ticker that is half the tolerance duration for responsiveness,
	// with a minimum floor to prevent busy-looping.
	p.ConfigMutex.RLock()
	tolerance := p.Config.OutOfOrderTolerance
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
		case <-ticker.C:
			p.ActivityMutex.Lock()

			// Determine the processing horizon. Entries older than this are safe to process.
			processingHorizon := p.NowFunc().Add(-tolerance)

			var toProcess []*LogEntry
			var remaining []*LogEntry

			// Partition the buffer into entries that are ready and those that are not.
			for _, entry := range p.EntryBuffer {
				if entry.Timestamp.Before(processingHorizon) {
					toProcess = append(toProcess, entry)
				} else {
					remaining = append(remaining, entry)
				}
			}

			// Update the buffer with the entries that are not yet ready.
			p.EntryBuffer = remaining

			// Sort the entries to be processed by timestamp to ensure strict chronological order.
			sort.Slice(toProcess, func(i, j int) bool {
				return toProcess[i].Timestamp.Before(toProcess[j].Timestamp)
			})

			// Process the sorted entries. The lock is already held.
			for _, entry := range toProcess {
				checkChainsInternal(p, entry)
			}
			p.ActivityMutex.Unlock()

			// Signal for tests that a tick has been processed.
			// This is done after unlocking to avoid holding the lock while signaling.
			if IsTesting() {
				// Use a very specific tag that the test harness can listen for.
				p.LogFunc(logging.LevelDebug, "BUFFER_WORKER_TICK_DONE", "Tick processed.")
			}
		}
	}
}
