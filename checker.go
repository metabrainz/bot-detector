package main

import (
	"fmt"
	"net/url"
	"strconv"
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
		// A mismatch is signaled by GetTrackingKey returning a key without a UserAgent.
		// If the primary key requires a UA, but the chain-specific key doesn't have one, it's a mismatch.
		// Also, if the chain requires a UA but the primary key doesn't, we can't track it.
		if (uaRequired && chainKey.UA == "") || (!uaRequired && chainKey.UA != "") {
			continue
		}

		// Get the current state for this chain.
		state, exists := currentActivity.ChainProgress[chain.Name]
		if !exists {
			// Initialize state if it's the first step for this chain.
			state = StepState{CurrentStep: 0, LastMatchTime: time.Time{}}
		}

		// Look for the next step in the chain
		nextStepIndex := state.CurrentStep
		if nextStepIndex < len(chain.Steps) {
			step := chain.Steps[nextStepIndex]

			// --- CHECK TIME WINDOWS ---
			if exists {
				// Check MaxDelayDuration
				if step.MaxDelayDuration > 0 && time.Since(state.LastMatchTime) > step.MaxDelayDuration {
					p.LogFunc(LevelDebug, "RESET", "Chain %s: MaxDelay %v exceeded for key %s. Resetting state.", chain.Name, step.MaxDelayDuration, trackingKey.IPInfo.Address)
					state.CurrentStep = 0
					goto UpdateChainProgress // Restart check on step 0
				}

				// FIX 2: MinDelayNotMet failure should reset the state, not just continue
				if step.MinDelayDuration > 0 && time.Since(state.LastMatchTime) < step.MinDelayDuration {
					p.LogFunc(LevelDebug, "RESET", "Chain %s: MinDelay %v not met for key %s. Resetting state.", chain.Name, step.MinDelayDuration, trackingKey.IPInfo.Address)
					state.CurrentStep = 0    // Reset state
					goto UpdateChainProgress // Proceed to state update/cleanup (which deletes the state)
				}
			}

			// --- CHECK FIELD MATCHES ---
			match := true
			for fieldName, _ := range step.FieldMatches {
				matchValue := ""
				var err error

				// Special handling for Referrer: extract path first.
				if fieldName == "Referrer" {
					matchValue, err = ExtractReferrerPath(entry.Referrer)
					if err != nil {
						// Log only if referrer is not empty but fails to parse.
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

				// The step's CompiledRegexes field is pre-compiled during config loading.
				if !step.CompiledRegexes[fieldName].MatchString(matchValue) {
					match = false
					break
				}
			}

			if match {
				// Step matched! Advance the state.
				state.CurrentStep++
				state.LastMatchTime = entry.Timestamp

				// --- CHECK FOR CHAIN COMPLETION ---
				if state.CurrentStep == len(chain.Steps) {
					isWhitelisted := p.IsWhitelistedFunc(entry.IPInfo)

					// 3. Take action (block/log)
					if chain.Action == "block" {
						// CONSOLIDATED BLOCKING LOGIC for action: block

						// 4. Send command to external blocker unless in DryRun mode
						if p.DryRun {
							p.LogFunc(LevelCritical, "DRY_RUN", "BLOCK! Chain: %s completed by IP %s. Action set to 'block' (DryRun).", chain.Name, entry.IPInfo.Address)
						} else if !isWhitelisted {
							p.LogFunc(LevelCritical, "ALERT", "BLOCK! Chain: %s completed by IP %s. Blocking for %v.", chain.Name, entry.IPInfo.Address, chain.BlockDuration)

							// Attempt to block the IP via the injected Blocker
							if err := p.Blocker.Block(entry.IPInfo, chain.BlockDuration); err != nil {
								// Error is logged inside Block, no action needed here
							}
						}

						// 5. Optimization: Mark the IP-ONLY key as blocked for skipping future log lines from this IP.
						// We can reuse the IPInfo from the LogEntry.
						ipOnlyKey := TrackingKey{IPInfo: entry.IPInfo, UA: ""}

						// Select the correct store/mutex for the IP-only block update based on p.DryRun
						// NOTE: We do not need a second mutex.Lock/Unlock because we already have the
						// primary mutex for the current activity locked (either p.ActivityMutex or DryRunActivityMutex),
						// and the IP-only key *must* reside in the same global map.

						// GetOrCreateActivityUnsafe is used because we hold the current activity's lock.
						ipActivity := GetOrCreateActivityUnsafe(store, ipOnlyKey)
						ipActivity.IsBlocked = true
						ipActivity.BlockedUntil = time.Now().Add(chain.BlockDuration) // Set block expiration time

					} else if chain.Action == "log" {

						// CONSOLIDATED LOGGING LOGIC for action: log
						baseMessage := fmt.Sprintf("LOG! Chain: %s completed by IP %s. Action set to 'log'.", chain.Name, entry.IPInfo.Address)

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
		}

	UpdateChainProgress: // Label for the single goto
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

// GetTrackingKey now performs a more fine-grained check for an individual chain,
// ensuring the IP version matches what the chain requires.
// NOTE: This function's logic is only for internal loop use inside CheckChains
// to filter out incompatible log lines for a specific chain.
// It is NOT used to determine the primary key for the log entry (GetTrackingKeyFromLogEntry is for that).
// This function should be removed if the logic in GetTrackingKeyFromLogEntry is deemed sufficient.
// For now, it is kept to mirror the original intent of checking version compatibility per chain.
/*
func GetTrackingKey(chain *BehavioralChain, entry *LogEntry) TrackingKey {
	// Replaced by logic in utils_ip.go's GetTrackingKey
}
*/
