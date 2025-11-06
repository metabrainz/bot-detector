package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
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

// ExtractReferrerPath safely parses the path from a full URL string.
func ExtractReferrerPath(referrer string) (string, error) {
	if referrer == "" {
		return "", fmt.Errorf("referrer is empty")
	}
	u, parseErr := url.Parse(referrer)
	if parseErr != nil {
		return referrer, fmt.Errorf("failed to parse URL from referrer: %w", parseErr)
	}

	// FIX: Check for both empty path and root path ("/") as required by the failing test.
	if u.Path == "" || u.Path == "/" {
		// Returning "" (empty string) for the path satisfies the test assertion.
		return "", fmt.Errorf("URL parsed but path is empty")
	}

	return u.Path, nil
}

// CheckChains is refactored as a method on Processor.
func (p *Processor) CheckChains(entry *LogEntry) {

	// FIX 1: Check whitelisting immediately after acquiring the IP/key
	// This prevents creating activity state for whitelisted IPs, fixing TestCheckChains_WhitelistSkip.
	if p.IsWhitelistedFunc(entry.IPInfo) {
		p.LogFunc(LevelDebug, "SKIP", "IP %s: Skipped (IP is whitelisted).", entry.IPInfo.Address)
		return
	}

	// Choose appropriate store & mutex based on the Processor's DryRun state.
	var store map[TrackingKey]*BotActivity
	var mutex *sync.RWMutex

	if p.DryRun {
		store = DryRunActivityStore
		mutex = &DryRunActivityMutex
	} else {
		store = p.ActivityStore
		mutex = p.ActivityMutex
	}

	// Determine if any chain requires a User Agent to create the primary tracking key.
	uaRequired := false
	p.ChainMutex.RLock()
	chains := p.Chains
	for _, chain := range chains {
		if strings.HasSuffix(chain.MatchKey, "_ua") {
			uaRequired = true
			break
		}
	}
	p.ChainMutex.RUnlock()

	trackingKey := TrackingKey{IPInfo: entry.IPInfo, UA: ""}
	if uaRequired {
		trackingKey.UA = entry.UserAgent
	}

	mutex.Lock()
	// Ensure the lock is released when the function exits.
	defer mutex.Unlock()

	// Get or create the activity struct for the current log line's tracking key.
	currentActivity := GetOrCreateActivityUnsafe(store, trackingKey)
	currentActivity.LastRequestTime = entry.Timestamp // Always update last request time

	// Optimization: If the current key (IP or IP+UA) is already blocked, stop processing chains.
	// NOTE: The primary block check (ProcessLogLine) uses the IP-only key for optimization.
	// This check is for the specific key (which might be IP+UA) that caused the original block.
	if currentActivity.IsBlocked {
		if time.Now().After(currentActivity.BlockedUntil) {
			p.LogFunc(LevelInfo, "EXPIRE", "Chain-specific block expired for key %s (UA: %s).", trackingKey.IPInfo.Address, trackingKey.UA)
			currentActivity.IsBlocked = false
			currentActivity.BlockedUntil = time.Time{}
		} else {
			p.LogFunc(LevelDebug, "SKIP", "Key %s (UA: %s): Skipped (Already blocked in memory by a chain).", trackingKey.IPInfo.Address, trackingKey.UA)
			return
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

			// --- CHECK TIME WINDOWS ---
			if exists && state.CurrentStep > 0 { // Time checks only apply after the first step.
				if step.MaxDelayDuration > 0 && time.Since(state.LastMatchTime) > step.MaxDelayDuration {
					p.LogFunc(LevelDebug, "RESET", "Chain %s: MaxDelay %v exceeded for key %s. Resetting state.", chain.Name, step.MaxDelayDuration, trackingKey.IPInfo.Address)
					state.CurrentStep = 0
					continue // Restart loop from step 0.
				}

				if step.MinDelayDuration > 0 && time.Since(state.LastMatchTime) < step.MinDelayDuration {
					p.LogFunc(LevelDebug, "RESET", "Chain %s: MinDelay %v not met for key %s. Resetting state.", chain.Name, step.MinDelayDuration, trackingKey.IPInfo.Address)
					state.CurrentStep = 0
					continue // Restart loop from step 0.
				}
			}

			// --- CHECK FIELD MATCHES ---
			match := true
			for fieldName, _ := range step.FieldMatches {
				matchValue := ""
				var err error

				if fieldName == "Referrer" {
					matchValue, err = ExtractReferrerPath(entry.Referrer)
					if err != nil {
						if entry.Referrer != "" {
							p.LogFunc(LevelDebug, "WARN", "Chain %s: Failed to extract path from referrer '%s': %v", chain.Name, entry.Referrer, err)
						}
						match = false
						break
					}
				} else {
					matchValue, err = GetMatchValue(fieldName, entry)
					if err != nil {
						p.LogFunc(LevelDebug, "WARN", "Chain %s: Unknown field '%s' in configuration. Skipping match.", chain.Name, fieldName)
						match = false
						break
					}
				}

				if !step.CompiledRegexes[fieldName].MatchString(matchValue) {
					match = false
					break
				}
			}

			if !match {
				break // No match on this step, exit the loop for this chain.
			}

			// Step matched! Advance the state.
			state.CurrentStep++
			state.LastMatchTime = entry.Timestamp

			// --- CHECK FOR CHAIN COMPLETION ---
			if state.CurrentStep == len(chain.Steps) {
				isWhitelisted := p.IsWhitelistedFunc(entry.IPInfo)

				// --- 3. Take Action (Log, Block, etc.) ---

				// First, handle the logging for all actions.
				if p.DryRun {
					p.LogFunc(LevelCritical, "DRY_RUN", "BLOCK! Chain: %s completed by IP %s. Action set to 'block' (DryRun).", chain.Name, entry.IPInfo.Address)
				} else if chain.Action == "block" {
					p.LogFunc(LevelCritical, "ALERT", "BLOCK! Chain: %s completed by IP %s. Blocking for %v.", chain.Name, entry.IPInfo.Address, chain.BlockDuration)
				} else if chain.Action == "log" {
					baseMessage := fmt.Sprintf("LOG! Chain: %s completed by IP %s. Action set to 'log'.", chain.Name, entry.IPInfo.Address)
					if isWhitelisted {
						p.LogFunc(LevelCritical, "ALERT", "%s (IP is whitelisted: NO FURTHER ACTION TAKEN)", baseMessage)
					} else {
						p.LogFunc(LevelCritical, "ALERT", baseMessage)
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
					ipActivity := GetOrCreateActivityUnsafe(store, ipOnlyKey)
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
