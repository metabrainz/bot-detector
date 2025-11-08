package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// GetMatchValue retrieves the field value from a LogEntry based on the field name.
func GetMatchValue(fieldName string, entry *LogEntry) (string, error) {
	switch fieldName {
	case "IP":
		return entry.IPInfo.Address, nil
	case "Path":
		return entry.Path, nil
	case "Method":
		return entry.Method, nil
	case "Protocol":
		return entry.Protocol, nil
	case "UserAgent":
		return entry.UserAgent, nil
	case "Referrer":
		return entry.Referrer, nil
	case "StatusCode":
		return strconv.Itoa(entry.StatusCode), nil
	default:
		return "", fmt.Errorf("unknown field: %s", fieldName)
	}
}

// preCheckActivity performs initial checks on an IP/key before processing against chains.
// It returns the relevant BotActivity and a boolean indicating if further processing should be skipped.
// The caller is responsible for locking/unlocking the ActivityMutex.
func (p *Processor) preCheckActivity(entry *LogEntry, trackingKey TrackingKey) (*BotActivity, bool) {
	// 2. Get or create activity and check for existing blocks.
	activity := GetOrCreateActivityUnsafe(p.ActivityStore, trackingKey)

	if activity.IsBlocked {
		if time.Now().After(activity.BlockedUntil) {
			// Block has expired, clear it and proceed.
			p.LogFunc(LevelInfo, "EXPIRE", "Chain-specific block expired for key %s (UA: %s).", trackingKey.IPInfo.Address, trackingKey.UA)
			activity.IsBlocked = false
			activity.BlockedUntil = time.Time{}
		} else {
			// Still blocked, update timestamp and skip.
			if entry.Timestamp.After(activity.LastRequestTime) {
				activity.LastRequestTime = entry.Timestamp
			}
			p.LogFunc(LevelDebug, "SKIP", "Key %s (UA: %s): Skipped (Already blocked in memory by a chain).", trackingKey.IPInfo.Address, trackingKey.UA)
			return activity, true // Skip processing
		}
	}

	return activity, false // Do not skip
}

// handleOutOfOrderEntry checks if a log entry is out-of-order and handles it based on tolerance.
// It returns true if the entry should be skipped, false otherwise.
// It also updates the LastRequestTime of the activity if the entry is in-order and newer.
// The caller is responsible for holding the ActivityMutex.
func (p *Processor) handleOutOfOrderEntry(entry *LogEntry, currentActivity *BotActivity) (skip bool) {
	previousRequestTime := currentActivity.LastRequestTime
	// If the entry is in order or it's the first time we've seen this key, process it.
	if previousRequestTime.IsZero() || !entry.Timestamp.Before(previousRequestTime) {
		return false // Do not skip.
	}

	// At this point, we know the entry is out-of-order.
	timeDifference := previousRequestTime.Sub(entry.Timestamp)
	if timeDifference <= p.Config.OutOfOrderTolerance {
		p.LogFunc(LevelDebug, "OUT_OF_ORDER_TOLERATED", "Processing out-of-order log entry for IP %s within tolerance (%v). Current: %s, Last seen: %s.",
			entry.IPInfo.Address, p.Config.OutOfOrderTolerance,
			entry.Timestamp.Format(AppLogTimestampFormat), previousRequestTime.Format(AppLogTimestampFormat))
		return false // Do not skip, process it.
	}

	p.LogFunc(LevelWarning, "OUT_OF_ORDER_SKIPPED", "Skipping out-of-order log entry for IP %s (too old: %v > %v). Current: %s, Last seen: %s.",
		entry.IPInfo.Address, timeDifference, p.Config.OutOfOrderTolerance,
		entry.Timestamp.Format(AppLogTimestampFormat), previousRequestTime.Format(AppLogTimestampFormat))
	return true // Skip this entry entirely.
}

// handleChainCompletion takes action when a chain is completed (log, block, etc.).
// It updates the activity state and returns true if the chain was completed.
// The caller is responsible for holding the ActivityMutex.
func (p *Processor) handleChainCompletion(chain *BehavioralChain, entry *LogEntry, currentActivity *BotActivity) {
	// --- 1. Log the completion event ---
	logLevel := LevelCritical
	if isTesting() {
		logLevel = LevelDebug
	}

	if p.DryRun {
		p.logDryRunCompletion(chain, entry)
	} else {
		// In live mode, log the action taken.
		switch chain.Action {
		case "block":
			p.LogFunc(logLevel, "ALERT", "BLOCK! Chain: %s completed by IP %s. Blocking for %v.", chain.Name, entry.IPInfo.Address, chain.BlockDuration)
		case "log":
			p.LogFunc(logLevel, "ALERT", "LOG! Chain: %s completed by IP %s. Action set to 'log'.", chain.Name, entry.IPInfo.Address)
		}
	}

	// --- 2. Perform the action ---
	if chain.Action == "block" {
		p.executeBlock(entry, chain)
		// Update the in-memory state to reflect the block for both live and dry runs.
		ipOnlyKey := TrackingKey{IPInfo: entry.IPInfo, UA: ""}
		ipActivity := GetOrCreateActivityUnsafe(p.ActivityStore, ipOnlyKey)
		ipActivity.IsBlocked = true
		ipActivity.BlockedUntil = time.Now().Add(chain.BlockDuration)

		currentActivity.IsBlocked = true
		currentActivity.BlockedUntil = ipActivity.BlockedUntil
	}
}

// executeBlock calls the external blocker unless in DryRun mode.
func (p *Processor) executeBlock(entry *LogEntry, chain *BehavioralChain) {
	if p.DryRun {
		return
	}
	if err := p.Blocker.Block(entry.IPInfo, chain.BlockDuration); err != nil {
		// Error is logged inside Block, no action needed here.
	}
}

// logDryRunCompletion handles logging for completed chains in dry-run mode.
func (p *Processor) logDryRunCompletion(chain *BehavioralChain, entry *LogEntry) {
	switch chain.Action {
	case "block":
		p.LogFunc(LevelInfo, "DRY_RUN", "BLOCK! Chain: %s completed by IP %s. Action set to 'block' (DryRun).", chain.Name, entry.IPInfo.Address)
	case "log":
		p.LogFunc(LevelInfo, "DRY_RUN", "LOG! Chain: %s completed by IP %s. Action set to 'log' (DryRun).", chain.Name, entry.IPInfo.Address)
	default:
		p.LogFunc(LevelInfo, "DRY_RUN", "UNKNOWN_ACTION! Chain: %s completed by IP %s. Unrecognized action '%s' (DryRun).", chain.Name, entry.IPInfo.Address, chain.Action)
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

// processChainForEntry evaluates a single log entry against a single behavioral chain.
// It manages state transitions (advancing, resetting) and triggers completion handling.
// The caller is responsible for holding the ActivityMutex.
func (p *Processor) processChainForEntry(chain *BehavioralChain, entry *LogEntry, currentActivity *BotActivity, previousRequestTime time.Time) {
	// If GetTrackingKey returns an empty key, it's a mismatch for this chain (e.g., wrong IP version).
	if GetTrackingKey(chain, entry).IPInfo.Address == "" {
		return
	}

	// Get the current state for this chain.
	state, exists := currentActivity.ChainProgress[chain.Name]
	if !exists {
		// Initialize state if it's the first time we're seeing this chain for this key.
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
				p.LogFunc(LevelDebug, "RESET", "Chain %s: MaxDelay %v exceeded. Resetting.", chain.Name, step.MaxDelayDuration)
				state.CurrentStep = 0
				continue // Restart check from step 0.
			}
			if step.MinDelayDuration > 0 && timeSinceLastStepHit < step.MinDelayDuration {
				p.LogFunc(LevelDebug, "RESET", "Chain %s: MinDelay %v not met. Resetting.", chain.Name, step.MinDelayDuration)
				state.CurrentStep = 0
				continue // Restart check from step 0.
			}
		}

		// --- FIELD MATCHING ---
		if !matchStepFields(&step, entry) {
			break
		} // No match on this step, exit the `for {}` loop for this chain.

		// --- STEP MATCHED ---
		state.CurrentStep++
		state.LastMatchTime = entry.Timestamp

		// --- CHECK FOR CHAIN COMPLETION ---
		if state.CurrentStep == len(chain.Steps) {
			p.handleChainCompletion(chain, entry, currentActivity)
			state.CurrentStep = 0 // Reset state after action is taken.
		}

		// If the chain did not complete, or if it completed and was reset,
		// break from the loop to save the new state.
		break
	}

	// --- STATE MANAGEMENT ---
	// Only store the state if the key is actively progressing (CurrentStep > 0).
	// This saves memory by not storing state for IPs that are at step 0.
	if state.CurrentStep > 0 {
		currentActivity.ChainProgress[chain.Name] = state
	} else {
		// If CurrentStep is 0 (due to reset or completion), clean up the state to save memory.
		// It's safe to call delete even if the key doesn't exist.
		delete(currentActivity.ChainProgress, chain.Name)
	}
}

// CheckChains is refactored as a method on Processor.
func (p *Processor) CheckChains(entry *LogEntry) {
	// Immediately skip processing if the IP is whitelisted. This is the primary guard.
	if p.IsWhitelistedFunc(entry.IPInfo) {
		p.LogFunc(LevelDebug, "SKIP", "IP %s: Skipped (IP is whitelisted).", entry.IPInfo.Address)
		return
	}

	// Determine the most specific tracking key required by any applicable chain.
	primaryKeySpecificity := 0 // 0=none, 1=ip, 2=ip_ua
	p.ChainMutex.RLock()
	chains := p.Chains
	for _, chain := range chains {
		if GetTrackingKey(&chain, entry).IPInfo.Address != "" { // Does this chain apply to this entry?
			if strings.HasSuffix(chain.MatchKey, "_ua") {
				primaryKeySpecificity = 2 // ip_ua is most specific
				break
			} else if primaryKeySpecificity < 1 {
				primaryKeySpecificity = 1 // ip is less specific
			}
		}
	}
	p.ChainMutex.RUnlock()

	trackingKey := TrackingKey{IPInfo: entry.IPInfo, UA: ""}
	if primaryKeySpecificity == 2 {
		trackingKey.UA = entry.UserAgent
	}

	p.ActivityMutex.Lock()
	// Ensure the lock is released when the function exits.
	defer p.ActivityMutex.Unlock()

	// Perform pre-checks for whitelisting and existing blocks.
	currentActivity, skip := p.preCheckActivity(entry, trackingKey)
	if skip {
		// If preCheckActivity returns a skip, the lock is still held,
		// so we can just return. The defer will unlock.
		return
	}

	// Handle out-of-order log entries and update LastRequestTime.
	// This function will return true if the entry should be skipped.
	if p.handleOutOfOrderEntry(entry, currentActivity) {
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
		p.processChainForEntry(&chain, entry, currentActivity, previousRequestTime)
	}

}
