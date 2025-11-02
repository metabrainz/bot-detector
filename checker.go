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

// CheckChains iterates through all chains and updates the IP's progress.
func CheckChains(entry *LogEntry) {

	ChainMutex.RLock()
	currentChains := Chains
	ChainMutex.RUnlock()

	for _, chain := range currentChains {

		trackingKey := GetTrackingKey(&chain, entry)

		if trackingKey == (TrackingKey{}) {
			LogOutput(LevelDebug, "SKIP", "IP %s: Skipped chain '%s'. IP version does not match required key type (%s).", entry.IP, chain.Name, chain.MatchKey)
			continue
		}

		// Choose appropriate store & mutex based on mode.
		store := ActivityStore
		mutex := &ActivityMutex
		if DryRun {
			store = DryRunActivityStore
			mutex = &DryRunActivityMutex
		}

		// Lock once for operations that need atomic access.
		mutex.Lock()

		// Use unsafe variant since we already hold the mutex.
		activity := GetOrCreateActivityUnsafe(store, trackingKey)

		state, exists := activity.ChainProgress[chain.Name]
		if !exists {
			state = StepState{}
		}

		nextStepIndex := state.CurrentStep
		if nextStepIndex >= len(chain.Steps) {
			nextStepIndex = 0
		}

		var nextStep StepDef
		if nextStepIndex < len(chain.Steps) {
			nextStep = chain.Steps[nextStepIndex]
		}

		// 1. Minimum Delay Check (min_delay)
		if nextStep.MinDelayDuration > 0 {
			var timeSource time.Time

			if state.CurrentStep == 0 {
				ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}
				// Use unsafe access to avoid re-locking the same mutex.
				ipActivity := GetOrCreateActivityUnsafe(store, ipOnlyKey)
				timeSource = ipActivity.LastRequestTime
			} else {
				timeSource = state.LastMatchTime
			}

			if !timeSource.IsZero() {
				timeSinceLastHit := entry.Timestamp.Sub(timeSource)

				// Only skip if the time difference is > 0 but < min_delay.
				// This allows hits logged in the same second (0s difference) to proceed.
				if timeSinceLastHit > 0 && timeSinceLastHit < nextStep.MinDelayDuration {
					LogOutput(LevelDebug, "SKIP", "IP %s: Hit for step %d of chain '%s' skipped (delay too short: %v < %v).", entry.IP, nextStepIndex+1, chain.Name, timeSinceLastHit, nextStep.MinDelayDuration)
					mutex.Unlock()
					continue
				}
			}
		}

		// 2. Maximum Delay Check: Reset progress if delay exceeds the Max Delay Duration.
		// FIX: We now check the MaxDelayDuration on the *current target step* (nextStep)
		// when progressing from a previously matched step (CurrentStep > 0).
		if state.CurrentStep > 0 && nextStepIndex < len(chain.Steps) {
			delay := entry.Timestamp.Sub(state.LastMatchTime)
			LogOutput(LevelDebug, "DEBUG", "IP %s: Checking Max Delay for transition to Step %d. Delay: %v", entry.IP, nextStepIndex+1, delay)

			// Check the MaxDelayDuration of the target step (nextStep)
			if nextStep.MaxDelayDuration > 0 && delay > nextStep.MaxDelayDuration {
				LogOutput(LevelDebug, "RESET", "IP %s: Progress on step %d of chain '%s' reset due to max_delay timeout (%v > %v) defined on the target step.", entry.IP, state.CurrentStep, chain.Name, delay, nextStep.MaxDelayDuration)
				state.CurrentStep = 0
				nextStepIndex = 0
				nextStep = chain.Steps[nextStepIndex]
			}
		}

		// 3. Field Match Check
		allFieldsMatch := false
		if nextStepIndex < len(chain.Steps) {
			allFieldsMatch = true

			for fieldName := range nextStep.FieldMatches {
				regex := nextStep.CompiledRegexes[fieldName]
				fieldValue := ""
				var err error

				switch fieldName {
				case "Referrer":
					fieldValue = entry.Referrer
				case "ReferrerPrevPath":
					if entry.Referrer != "" {
						u, parseErr := url.Parse(entry.Referrer)
						if parseErr == nil && u.Path != "" {
							fieldValue = u.Path
						} else {
							if !DryRun {
								LogOutput(LevelWarning, "WARN", "Failed to parse URL path from referrer: %s (Error: %v)", entry.Referrer, parseErr)
							}
							fieldValue = entry.Referrer
						}
					} else {
						allFieldsMatch = false
						break
					}
				case "StatusCode":
					fieldValue = strconv.Itoa(entry.StatusCode)
				default:
					fieldValue, err = GetMatchValue(fieldName, entry)
				}

				if err != nil {
					LogOutput(LevelError, "ERROR", "Internal error in GetMatchValue for field %s: %v", fieldName, err)
					allFieldsMatch = false
					break
				}

				if !regex.MatchString(fieldValue) {
					allFieldsMatch = false
					break
				}
			}

			if allFieldsMatch {
				// Corrected Logging Logic
				isCompletion := state.CurrentStep+1 == len(chain.Steps)

				if isCompletion {
					LogOutput(LevelDebug, "MATCH", "IP %s: Matched final step %d of chain '%s'. Chain completion detected.", entry.IP, state.CurrentStep+1, chain.Name)
				} else {
					LogOutput(LevelDebug, "MATCH", "IP %s: Matched step %d of chain '%s'. Progressing to step %d.", entry.IP, state.CurrentStep+1, chain.Name, state.CurrentStep+2)
				}

				state.CurrentStep++
				state.LastMatchTime = entry.Timestamp

				// 4. Check for Chain Completion
				if state.CurrentStep == len(chain.Steps) {
					isWhitelisted := IsIPWhitelisted(entry.IP) // Check whitelist status here

					if chain.Action == "block" {
						baseMessage := fmt.Sprintf("BLOCK! Chain: %s completed by IP %s. Attempting to block for %v.", chain.Name, entry.IP, chain.BlockDuration)

						if isWhitelisted {
							LogOutput(LevelCritical, "ALERT", "%s (IP is whitelisted: BLOCK ACTION SKIPPED)", baseMessage)
						} else {
							// Original logic for non-whitelisted block: two logs (ALERT + HAPROXY_BLOCK)
							LogOutput(LevelCritical, "ALERT", baseMessage)

							if err := BlockIP(entry.IP, entry.IPVersion, chain.BlockDuration); err != nil {
								// Error is logged inside BlockIP, no action needed here
							}

							// Optimization: Mark the IP-ONLY key as blocked for skipping future log lines from this IP.
							ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}
							ipActivity := GetOrCreateActivityUnsafe(store, ipOnlyKey)
							ipActivity.IsBlocked = true
							ipActivity.BlockedUntil = time.Now().Add(chain.BlockDuration) // Set block expiration time
							ipActivity.LastRequestTime = entry.Timestamp
						}

					} else if chain.Action == "log" {

						// CONSOLIDATED LOGGING LOGIC for action: log
						baseMessage := fmt.Sprintf("LOG! Chain: %s completed by IP %s. Action set to 'log'.", chain.Name, entry.IP)

						if isWhitelisted {
							// Consolidate into a single log line when whitelisted
							LogOutput(LevelCritical, "ALERT", "%s (IP is whitelisted: NO FURTHER ACTION TAKEN)", baseMessage)
						} else {
							// Original log output for non-whitelisted IPs
							LogOutput(LevelCritical, "ALERT", baseMessage)
						}
					}

					// Reset state *after* action is taken.
					state.CurrentStep = 0
				}
			}
		}

		// 5. Conditional Update and Cleanup of ChainProgress State (Memory Optimization)
		// Only store the state if the key is actively progressing (CurrentStep > 0).
		if state.CurrentStep > 0 {
			activity.ChainProgress[chain.Name] = state
		} else {
			delete(activity.ChainProgress, chain.Name)
		}

		// --- NEW: 6. Update LastRequestTime for IP-only activity (used by min_delay check for step 1) ---
		// We only update the IP-only key because the min_delay check on step 1
		// specifically fetches the IP-only activity's LastRequestTime.
		ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}
		ipActivity := GetOrCreateActivityUnsafe(store, ipOnlyKey)
		ipActivity.LastRequestTime = entry.Timestamp

		mutex.Unlock()
	}
}
