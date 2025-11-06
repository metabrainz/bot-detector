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

	// Access chains and mutex via the processor struct
	p.ChainMutex.RLock()
	currentChains := p.Chains
	p.ChainMutex.RUnlock()

	for _, chain := range currentChains {

		// GetTrackingKey is assumed to be accessible in package scope
		trackingKey := GetTrackingKey(&chain, entry)

		if trackingKey == (TrackingKey{}) {
			p.LogFunc(LevelDebug, "SKIP", "IP %s: Skipped chain '%s'. IP version does not match required key type (%s).", entry.IP, chain.Name, chain.MatchKey)
			continue
		}

		// Choose appropriate store & mutex based on mode (already done by Processor construction).
		store := p.ActivityStore
		mutex := p.ActivityMutex

		// Lock once for operations that need atomic access.
		mutex.Lock()

		// GetOrCreateActivityUnsafe is assumed to be accessible in package scope
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
				if timeSinceLastHit > 0 && timeSinceLastHit < nextStep.MinDelayDuration {
					p.LogFunc(LevelDebug, "SKIP", "IP %s: Hit for step %d of chain '%s' skipped (delay too short: %v < %v).", entry.IP, nextStepIndex+1, chain.Name, timeSinceLastHit, nextStep.MinDelayDuration)
					mutex.Unlock()
					continue
				}
			}
		}

		// 2. Maximum Delay Check: Reset progress if delay exceeds the Max Delay Duration.
		if state.CurrentStep > 0 && nextStepIndex < len(chain.Steps) {
			delay := entry.Timestamp.Sub(state.LastMatchTime)
			p.LogFunc(LevelDebug, "DEBUG", "IP %s: Checking Max Delay for transition to Step %d. Delay: %v", entry.IP, nextStepIndex+1, delay)

			if nextStep.MaxDelayDuration > 0 && delay > nextStep.MaxDelayDuration {
				p.LogFunc(LevelDebug, "RESET", "IP %s: Progress on step %d of chain '%s' reset due to max_delay timeout (%v > %v) defined on the target step.", entry.IP, state.CurrentStep, chain.Name, delay, nextStep.MaxDelayDuration)
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
					var pathErr error
					fieldValue, pathErr = ExtractReferrerPath(entry.Referrer)
					if pathErr != nil {
						if !p.DryRun {
							p.LogFunc(LevelWarning, "WARN", "Failed to parse URL path from referrer: %s (Error: %v)", entry.Referrer, pathErr)
						}
						allFieldsMatch = false
						break
					}
				case "StatusCode":
					fieldValue = strconv.Itoa(entry.StatusCode)
				default:
					fieldValue, err = GetMatchValue(fieldName, entry)
				}

				if err != nil {
					p.LogFunc(LevelError, "ERROR", "Internal error in GetMatchValue for field %s: %v", fieldName, err)
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
					p.LogFunc(LevelDebug, "MATCH", "IP %s: Matched final step %d of chain '%s'. Chain completion detected.", entry.IP, state.CurrentStep+1, chain.Name)
				} else {
					p.LogFunc(LevelDebug, "MATCH", "IP %s: Matched step %d of chain '%s'. Progressing to step %d.", entry.IP, state.CurrentStep+1, chain.Name, state.CurrentStep+2)
				}

				state.CurrentStep++
				state.LastMatchTime = entry.Timestamp

				// 4. Check for Chain Completion
				if state.CurrentStep == len(chain.Steps) {
					isWhitelisted := p.IsWhitelistedFunc(entry.IP)

					if chain.Action == "block" {
						baseMessage := fmt.Sprintf("BLOCK! Chain: %s completed by IP %s. Attempting to block for %v.", chain.Name, entry.IP, chain.BlockDuration)

						if isWhitelisted {
							p.LogFunc(LevelCritical, "ALERT", "%s (IP is whitelisted: BLOCK ACTION SKIPPED)", baseMessage)
						} else {
							p.LogFunc(LevelCritical, "ALERT", baseMessage)

							// Use the injected Blocker interface
							if err := p.Blocker.Block(entry.IP, entry.IPVersion, chain.BlockDuration); err != nil {
								// Error is logged inside Blocker implementation (or we log it here if Blocker doesn't)
							}

							// Optimization: Mark the IP-ONLY key as blocked for skipping future log lines from this IP.
							ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}
							ipActivity := GetOrCreateActivityUnsafe(store, ipOnlyKey)
							ipActivity.IsBlocked = true
							ipActivity.BlockedUntil = time.Now().Add(chain.BlockDuration) // Set block expiration time
						}

					} else if chain.Action == "log" {
						baseMessage := fmt.Sprintf("LOG! Chain: %s completed by IP %s. Action set to 'log'.", chain.Name, entry.IP)

						if isWhitelisted {
							p.LogFunc(LevelCritical, "ALERT", "%s (IP is whitelisted: NO FURTHER ACTION TAKEN)", baseMessage)
						} else {
							p.LogFunc(LevelCritical, "ALERT", baseMessage)
						}
					}

					// Reset state *after* action is taken.
					state.CurrentStep = 0
				}
			}
		}

		// 5. Conditional Update and Cleanup of ChainProgress State (Memory Optimization)
		if state.CurrentStep > 0 {
			activity.ChainProgress[chain.Name] = state
		} else {
			delete(activity.ChainProgress, chain.Name)
		}

		mutex.Unlock()
	}
}

// CheckChains remains the global function and now acts as a wrapper.
func CheckChains(entry *LogEntry) {
	// 1. Determine which store/mutex to use based on the global DryRun flag (assumed to be defined).
	var store map[TrackingKey]*BotActivity
	var mutex *sync.RWMutex

	if DryRun {
		store = DryRunActivityStore
		mutex = &DryRunActivityMutex
	} else {
		store = ActivityStore
		mutex = &ActivityMutex
	}

	// 2. Instantiate the Processor struct, using globals for fields.
	p := &Processor{
		ActivityStore:     store,
		ActivityMutex:     mutex,
		Chains:            Chains,          // Global Chains
		ChainMutex:        &ChainMutex,     // Global ChainMutex
		DryRun:            DryRun,          // Global DryRun flag
		LogFunc:           LogOutput,       // Global LogOutput function
		IsWhitelistedFunc: IsIPWhitelisted, // Global IsIPWhitelisted function
		Blocker:           &GlobalBlocker{},
	}

	// 3. Call the new, testable method.
	p.CheckChains(entry)
}
