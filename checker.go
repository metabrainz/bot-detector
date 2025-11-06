package main

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// GetMatchValue retrieves the field value from a LogEntry based on the field name.
func GetMatchValue(fieldName string, entry *LogEntry) (string, error) {
	switch fieldName {
	case "IP":
		return entry.IP, nil
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

// ExtractReferrerPath safely parses the path from a full URL string.
func ExtractReferrerPath(referrer string) (string, error) {
	if referrer == "" {
		return "", fmt.Errorf("referrer is empty")
	}
	u, parseErr := url.Parse(referrer)
	if parseErr != nil {
		return referrer, fmt.Errorf("failed to parse URL from referrer: %w", parseErr)
	}
	if u.Path == "" {
		return referrer, fmt.Errorf("URL parsed but path is empty")
	}
	return u.Path, nil
}

// CheckChains is refactored as a method on Processor.
func (p *Processor) CheckChains(entry *LogEntry) {

	// 1. Get the current chains safely.
	p.ChainMutex.RLock()
	currentChains := p.Chains
	p.ChainMutex.RUnlock()

	for _, chain := range currentChains {

		trackingKey := GetTrackingKey(&chain, entry)

		if trackingKey == (TrackingKey{}) {
			p.LogFunc(LevelDebug, "SKIP", "IP %s: Skipped chain '%s'. IP version does not match required key type (%s).", entry.IP, chain.Name, chain.MatchKey)
			continue
		}

		// Choose appropriate store & mutex based on mode.
		// Note: p.ActivityStore and p.ActivityMutex are already set to DryRun versions if needed.
		store := p.ActivityStore
		mutex := p.ActivityMutex

		// 2. Lock the activity state for the key.
		mutex.Lock()
		// GetOrCreateActivityUnsafe is used because we hold the lock.
		activity := GetOrCreateActivityUnsafe(store, trackingKey)

		// Optimization: Check the IP-only key's block state before checking chains.
		// Since we only block the IP-only key, we only need to check that one.
		// We are already inside a mutex.Lock() on the (possibly IP+UA) key's activity.
		// This block is checked in ProcessLogLine(), so we rely on that.

		// Initialize state for the chain if it doesn't exist
		state, exists := activity.ChainProgress[chain.Name]
		if !exists {
			state = StepState{CurrentStep: 0, LastMatchTime: time.Time{}}
		}

		// 3. Check if the current request is valid for the next expected step.
		nextStepIndex := state.CurrentStep // The step we are *trying* to match now.
		if nextStepIndex >= len(chain.Steps) {
			// This should not happen if state is properly reset after a chain completion.
			nextStepIndex = 0
			state.CurrentStep = 0
		}

		nextStep := chain.Steps[nextStepIndex]

		// Check the delay between the current request and the PREVIOUS successful step.
		// For the first step (nextStepIndex == 0), LastMatchTime is zero, so this check is skipped.
		if nextStepIndex > 0 {
			// Check MaxDelay: Has too much time passed since the last step?
			timeElapsed := entry.Timestamp.Sub(state.LastMatchTime)
			if timeElapsed > nextStep.MaxDelayDuration {
				// Too slow. Reset to step 0 and start over.
				p.LogFunc(LevelDebug, "FAIL", "IP %s: Chain '%s' reset. MaxDelay (%v) exceeded (%v passed).", entry.IP, chain.Name, nextStep.MaxDelayDuration, timeElapsed)
				state.CurrentStep = 0
				// Rerun the check against the new Step 0 (the first step).
				nextStepIndex = 0
				nextStep = chain.Steps[nextStepIndex]
			} else if nextStep.MinDelayDuration > 0 && timeElapsed < nextStep.MinDelayDuration {
				// Too fast. Skip processing this line for this chain.
				// NOTE: We do not reset the chain, we just ignore this line to allow the user to continue if they are too fast.
				p.LogFunc(LevelDebug, "FAIL", "IP %s: Chain '%s' skipped. MinDelay (%v) not met (%v passed).", entry.IP, chain.Name, nextStep.MinDelayDuration, timeElapsed)
				mutex.Unlock()
				continue
			}
		}

		// Check the delay between the current request and the PREVIOUS overall request (LastRequestTime).
		// This is for min_delay check on the FIRST step (nextStepIndex == 0) and applies globally.
		if nextStepIndex == 0 && nextStep.MinDelayDuration > 0 {
			// Check the delay since the PREVIOUS overall request from this key.
			// The overall activity.LastRequestTime is updated *after* CheckChains, so it holds the previous time.
			// NOTE: We use the key's LastRequestTime for the step 0 min_delay check.
			timeElapsedSinceLastRequest := entry.Timestamp.Sub(activity.LastRequestTime)

			// Only enforce MinDelay for step 0 if LastRequestTime is NOT the zero time (i.e., not the first ever request)
			if !activity.LastRequestTime.IsZero() && timeElapsedSinceLastRequest < nextStep.MinDelayDuration {
				// Too fast. Skip processing this line for this chain.
				p.LogFunc(LevelDebug, "FAIL", "IP %s: Chain '%s' skipped at Step 0. Global MinDelay (%v) not met (%v passed).", entry.IP, chain.Name, nextStep.MinDelayDuration, timeElapsedSinceLastRequest)
				mutex.Unlock()
				continue
			}
		}

		// 4. Check all field matches for the current step
		allMatches := true
		for fieldName, regexStr := range nextStep.CompiledRegexes {
			var value string
			var err error

			// Special handling for "ReferrerPath"
			if fieldName == "ReferrerPath" {
				value, err = ExtractReferrerPath(entry.Referrer)
			} else {
				value, err = GetMatchValue(fieldName, entry)
			}

			if err != nil {
				// This happens if an unknown field is requested, log it but don't fail the chain
				p.LogFunc(LevelDebug, "MATCH_ERROR", "IP %s: Chain '%s' Field '%s' check failed: %v", entry.IP, chain.Name, fieldName, err)
				allMatches = false
				break
			}

			if !regexStr.MatchString(value) {
				allMatches = false
				break
			}
		}

		// 5. Update state based on match result
		if allMatches {
			// Successful match: advance to the next step
			state.CurrentStep++
			state.LastMatchTime = entry.Timestamp

			p.LogFunc(LevelDebug, "MATCH", "IP %s: Chain '%s' advanced to Step %d.", entry.IP, chain.Name, state.CurrentStep)

			// Check if the chain is complete
			if state.CurrentStep >= len(chain.Steps) {
				isWhitelisted := p.IsWhitelistedFunc(entry.IP) // Use injected function

				// CHAIN COMPLETE: Take Action
				if chain.Action == "block" {

					// CONSOLIDATED BLOCKING LOGIC for action: block
					baseMessage := fmt.Sprintf("BLOCK! Chain: %s completed by IP %s. Blocking for %v.", chain.Name, entry.IP, chain.BlockDuration)

					if isWhitelisted {
						// Consolidate into a single log line when whitelisted
						p.LogFunc(LevelCritical, "ALERT", "%s (IP is whitelisted: NO BLOCK ACTION TAKEN)", baseMessage)
					} else {
						// Original log output for non-whitelisted IPs
						p.LogFunc(LevelCritical, "ALERT", baseMessage)

						// Attempt to block the IP via the injected Blocker
						if err := p.Blocker.Block(entry.IP, entry.IPVersion, chain.BlockDuration); err != nil {
							// Error is logged inside Block, no action needed here
						}

						// Optimization: Mark the IP-ONLY key as blocked for skipping future log lines from this IP.
						ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}
						ipActivity := GetOrCreateActivityUnsafe(store, ipOnlyKey)
						ipActivity.IsBlocked = true
						ipActivity.BlockedUntil = time.Now().Add(chain.BlockDuration) // Set block expiration time
					}

				} else if chain.Action == "log" {

					// CONSOLIDATED LOGGING LOGIC for action: log
					baseMessage := fmt.Sprintf("LOG! Chain: %s completed by IP %s. Action set to 'log'.", chain.Name, entry.IP)

					if isWhitelisted {
						// Consolidate into a single log line when whitelisted
						p.LogFunc(LevelCritical, "ALERT", "%s (IP is whitelisted: NO FURTHER ACTION TAKEN)", baseMessage)
					} else {
						// Original log output for non-whitelisted IPs
						p.LogFunc(LevelCritical, "ALERT", baseMessage)
					}

				}

				// Reset state *after* action is taken.
				state.CurrentStep = 0
			}
		}

		// 5. Conditional Update and Cleanup of ChainProgress State (Memory Optimization)
		// Only store the state if the key is actively progressing (CurrentStep > 0).
		if state.CurrentStep > 0 {
			activity.ChainProgress[chain.Name] = state
		} else {
			delete(activity.ChainProgress, chain.Name)
		}

		mutex.Unlock()
	}
}
