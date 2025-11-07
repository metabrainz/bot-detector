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

// CheckChains is refactored as a method on Processor.
func (p *Processor) CheckChains(entry *LogEntry) {

	// FIX 1: Check whitelisting immediately after acquiring the IP/key
	// This prevents creating activity state for whitelisted IPs, fixing TestCheckChains_WhitelistSkip.
	if p.IsWhitelistedFunc(entry.IPInfo) {
		p.LogFunc(LevelDebug, "SKIP", "IP %s: Skipped (IP is whitelisted).", entry.IPInfo.Address)
		return
	}

	// Determine the most specific tracking key required by any matching chain.
	// This ensures we use the correct BotActivity store for the request.
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

	// Get or create the activity struct for the current log line's tracking key.
	currentActivity := GetOrCreateActivityUnsafe(p.ActivityStore, trackingKey)
	// This is the baseline for first-step time checks. It's the time of the IP's last hit.

	// Always defer the update of LastRequestTime. It should always reflect the latest timestamp seen
	// for this key, but only after all checks have used the 'previousRequestTime'.
	// This ensures monotonicity.
	defer func() {
		if entry.Timestamp.After(currentActivity.LastRequestTime) {
			currentActivity.LastRequestTime = entry.Timestamp
		}
	}()

	if currentActivity.IsBlocked {
		if time.Now().After(currentActivity.BlockedUntil) {
			p.LogFunc(LevelInfo, "EXPIRE", "Chain-specific block expired for key %s (UA: %s).", trackingKey.IPInfo.Address, trackingKey.UA)
			currentActivity.IsBlocked = false
			currentActivity.BlockedUntil = time.Time{}
		} else {
			// Even if blocked, ensure LastRequestTime is updated if this entry is newer,
			// to prevent premature cleanup by the idle routine.
			if entry.Timestamp.After(currentActivity.LastRequestTime) {
				currentActivity.LastRequestTime = entry.Timestamp
			}
			p.LogFunc(LevelDebug, "SKIP", "Key %s (UA: %s): Skipped (Already blocked in memory by a chain).", trackingKey.IPInfo.Address, trackingKey.UA)
			return
		}
	}

	previousRequestTime := currentActivity.LastRequestTime

	// Handle out-of-order log entries.
	// If the current entry's timestamp is older than the last recorded request time for this key,
	// it's an out-of-order entry.
	if !previousRequestTime.IsZero() && entry.Timestamp.Before(previousRequestTime) {
		timeDifference := previousRequestTime.Sub(entry.Timestamp)
		if timeDifference <= p.Config.OutOfOrderTolerance {
			// This entry is out-of-order but within tolerance. Process it.
			// IMPORTANT: For time-based rules (min_delay, max_delay, first_hit_since),
			// we must *still* use `previousRequestTime` (the newer timestamp)
			// to avoid negative durations or incorrect rule evaluations.
			// The `LastRequestTime` in `currentActivity` will *not* be updated by this entry
			// to maintain monotonicity.
			p.LogFunc(LevelDebug, "OUT_OF_ORDER_TOLERATED", "Processing out-of-order log entry for IP %s within tolerance (%v). Current: %v, Last seen: %v.", entry.IPInfo.Address, p.Config.OutOfOrderTolerance, entry.Timestamp, previousRequestTime)
			// We proceed, but the defer below needs to be conditional on entry.Timestamp being newer.
		} else {
			// This entry is too old and out-of-order. Skip it.
			p.LogFunc(LevelWarning, "OUT_OF_ORDER_SKIPPED", "Skipping out-of-order log entry for IP %s (too old: %v > %v). Current: %v, Last seen: %v.", entry.IPInfo.Address, timeDifference, p.Config.OutOfOrderTolerance, entry.Timestamp, previousRequestTime)
			return // Skip this entry entirely.
		}
	} else {
		// If the entry is in-order or the first for this IP, update LastRequestTime if needed.
		if p.Config.MaxTimeSinceLastHit > 0 { // Only update if min_time_since_last_hit rules are active
			defer func() {
				if entry.Timestamp.After(currentActivity.LastRequestTime) { // Only update if current entry is newer
					currentActivity.LastRequestTime = entry.Timestamp
				}
			}()
		}
	}

	// 2. Iterate over all configured chains.
	for _, chain := range chains {
		// Check if the current log entry's tracking key is applicable to this chain
		// (e.g., check IP version compatibility again, as not all chains may be applicable).
		chainKey := GetTrackingKey(&chain, entry)
		// If GetTrackingKey returns an empty key, it's a mismatch for this chain.
		if chainKey.IPInfo.Address == "" {
			continue
		}

		// Get the current state for this chain.
		state, exists := currentActivity.ChainProgress[chain.Name]
		if !exists {
			// Initialize state if it's the first step for this chain.
			state = StepState{CurrentStep: 0, LastMatchTime: time.Time{}}
		}

		// Use a loop to handle step progression and potential resets (replaces goto).
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
				// `min_time_since_last_hit` rule: The first step only matches if the time since the
				// last overall request from this key is GREATER than the specified duration.
				// This is useful for detecting "sleepy" bots that are not continuously active.
				if step.MinTimeSinceLastHit > 0 {
					if previousRequestTime.IsZero() || timeSinceLastHit < step.MinTimeSinceLastHit {
						break // Last hit was too recent, or it's the first ever hit. Skip.
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

			match := true
			for fieldName, _ := range step.FieldMatches {
				matchValue := ""
				var err error

				matchValue, err = GetMatchValue(fieldName, entry)
				if err != nil {
					// This can happen if the field is unknown. In that case, it's not a match.
					match = false
					break
				}

				if !step.CompiledRegexes[fieldName].MatchString(matchValue) {
					match = false
					break
				}
			}

			if !match {
				break // No match on this step, exit the `for {}` loop for this chain.
			}

			// Step matched! Advance the state.
			state.CurrentStep++
			state.LastMatchTime = entry.Timestamp

			// --- CHECK FOR CHAIN COMPLETION ---
			if state.CurrentStep == len(chain.Steps) {
				isWhitelisted := p.IsWhitelistedFunc(entry.IPInfo)

				// --- 3. Take Action (Log, Block, etc.) ---

				// Determine the appropriate log level. During tests, downgrade alerts to debug to reduce noise.
				logLevel := LevelCritical
				if isTesting() {
					logLevel = LevelDebug
				}

				// First, handle the logging for all actions.
				if p.DryRun { // In DryRun mode, log the *actual* action of the chain.
					// In dry-run, we use LevelInfo for the "DRY_RUN" tag.
					if chain.Action == "block" {
						p.LogFunc(LevelInfo, "DRY_RUN", "BLOCK! Chain: %s completed by IP %s. Action set to 'block' (DryRun).", chain.Name, entry.IPInfo.Address)
					} else if chain.Action == "log" {
						p.LogFunc(LevelInfo, "DRY_RUN", "LOG! Chain: %s completed by IP %s. Action set to 'log' (DryRun).", chain.Name, entry.IPInfo.Address)
					} else {
						p.LogFunc(LevelInfo, "DRY_RUN", "UNKNOWN_ACTION! Chain: %s completed by IP %s. Unrecognized action '%s' (DryRun).", chain.Name, entry.IPInfo.Address, chain.Action)
					}
				} else if chain.Action == "block" { // Live mode: block action
					p.LogFunc(logLevel, "ALERT", "BLOCK! Chain: %s completed by IP %s. Blocking for %v.", chain.Name, entry.IPInfo.Address, chain.BlockDuration)
				} else if chain.Action == "log" { // Live mode: log action
					baseMessage := fmt.Sprintf("LOG! Chain: %s completed by IP %s. Action set to 'log'.", chain.Name, entry.IPInfo.Address)
					if isWhitelisted {
						p.LogFunc(logLevel, "ALERT", "%s (IP is whitelisted: NO FURTHER ACTION TAKEN)", baseMessage)
					} else {
						p.LogFunc(logLevel, "ALERT", baseMessage)
					}
				}

				// Second, if the action is 'block', perform the blocking steps.
				if chain.Action == "block" {
					// Call the external blocker (e.g., HAProxy), unless in DryRun or whitelisted.
					if !p.DryRun && !isWhitelisted {
						if err := p.Blocker.Block(entry.IPInfo, chain.BlockDuration); err != nil {
							// Error is logged inside Block, no action needed here
						}
					}

					// Update the in-memory state to reflect the block for both live and dry runs.
					ipOnlyKey := TrackingKey{IPInfo: entry.IPInfo, UA: ""}
					ipActivity := GetOrCreateActivityUnsafe(p.ActivityStore, ipOnlyKey)
					ipActivity.IsBlocked = true
					ipActivity.BlockedUntil = time.Now().Add(chain.BlockDuration) // Set block expiration time

					currentActivity.IsBlocked = true
					currentActivity.BlockedUntil = ipActivity.BlockedUntil
				}

				// Reset state *after* action is taken.
				state.CurrentStep = 0
			}
			// If the chain did not complete, break from the for loop to save the new state.
			break
		}

		// 5. Conditional Update and Cleanup of ChainProgress State (Memory Optimization)
		// Only store the state if the key is actively progressing (CurrentStep > 0).
		if state.CurrentStep > 0 {
			currentActivity.ChainProgress[chain.Name] = state
		} else {
			// If CurrentStep is 0 (reset/complete) and state exists, clean up to save memory.
			if _, exists := currentActivity.ChainProgress[chain.Name]; exists {
				delete(currentActivity.ChainProgress, chain.Name)
			}
		}
	}

}
