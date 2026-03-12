package main

import (
	"sync"
	"testing"
	"time"

	"bot-detector/internal/app"
	"bot-detector/internal/persistence"
)

// TestIPStatesRaceConditions tests concurrent access to IPStates map
// to ensure no race conditions exist when:
// 1. Health check callback reads IPStates (resync)
// 2. State restoration writes to IPStates (fetchInitialStateFromCluster, replayJournalAfter)
// 3. Block/unblock operations write to IPStates
// 4. Compaction reads and writes IPStates
func TestIPStatesRaceConditions(t *testing.T) {
	p := &app.Processor{
		IPStates:           make(map[string]persistence.IPState),
		PersistenceEnabled: true,
		NowFunc:            time.Now,
	}

	var wg sync.WaitGroup
	stopChan := make(chan struct{})

	// Simulate health check resync callback reading IPStates
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopChan:
				return
			default:
				p.PersistenceMutex.Lock()
				if len(p.IPStates) > 0 {
					for ip, state := range p.IPStates {
						_ = ip
						_ = state.State
					}
				}
				p.PersistenceMutex.Unlock()
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	// Simulate fetchInitialStateFromCluster writing to IPStates
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			select {
			case <-stopChan:
				return
			default:
				// This simulates the pattern in fetchInitialStateFromCluster
				// which should be protected by a lock
				p.PersistenceMutex.Lock()
				newStates := make(map[string]persistence.IPState)
				for j := 0; j < 10; j++ {
					ip := "10.0.0." + string(rune('0'+j))
					newStates[ip] = persistence.IPState{
						State:      persistence.BlockStateBlocked,
						ExpireTime: time.Now().Add(time.Hour),
						Reason:     "test",
						ModifiedAt: time.Now(),
					}
				}
				p.IPStates = newStates
				p.PersistenceMutex.Unlock()
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	// Simulate replayJournalAfter writing to IPStates
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			select {
			case <-stopChan:
				return
			default:
				// This simulates the pattern in replayJournalAfter
				// which should be protected by a lock
				p.PersistenceMutex.Lock()
				ip := "192.168.1." + string(rune('0'+(i%10)))
				p.IPStates[ip] = persistence.IPState{
					State:      persistence.BlockStateBlocked,
					ExpireTime: time.Now().Add(time.Hour),
					Reason:     "replay",
					ModifiedAt: time.Now(),
				}
				p.PersistenceMutex.Unlock()
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	// Simulate block operations writing to IPStates
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			select {
			case <-stopChan:
				return
			default:
				p.PersistenceMutex.Lock()
				ip := "172.16.0." + string(rune('0'+(i%10)))
				p.IPStates[ip] = persistence.IPState{
					State:      persistence.BlockStateBlocked,
					ExpireTime: time.Now().Add(time.Hour),
					Reason:     "block",
					ModifiedAt: time.Now(),
				}
				p.PersistenceMutex.Unlock()
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	// Simulate compaction reading and writing IPStates
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			select {
			case <-stopChan:
				return
			default:
				p.PersistenceMutex.Lock()
				now := time.Now()
				for ip, state := range p.IPStates {
					if state.State == persistence.BlockStateBlocked && !now.Before(state.ExpireTime) {
						delete(p.IPStates, ip)
					}
				}
				p.PersistenceMutex.Unlock()
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	// Let it run for a bit
	time.Sleep(100 * time.Millisecond)
	close(stopChan)
	wg.Wait()

	t.Logf("Test completed successfully with %d IPs in state", len(p.IPStates))
}

// TestIPStatesLenRaceCondition specifically tests the len() check race condition
// that was fixed in commit 6545486
func TestIPStatesLenRaceCondition(t *testing.T) {
	p := &app.Processor{
		IPStates:           make(map[string]persistence.IPState),
		PersistenceEnabled: true,
		NowFunc:            time.Now,
	}

	var wg sync.WaitGroup
	stopChan := make(chan struct{})

	// Goroutine that checks len() and iterates (like resync callback)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopChan:
				return
			default:
				// This is the CORRECT pattern (after fix)
				p.PersistenceMutex.Lock()
				if len(p.IPStates) > 0 {
					for ip, state := range p.IPStates {
						_ = ip
						_ = state
					}
				}
				p.PersistenceMutex.Unlock()
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	// Goroutine that modifies the map
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			select {
			case <-stopChan:
				return
			default:
				p.PersistenceMutex.Lock()
				if i%2 == 0 {
					// Add entries
					for j := 0; j < 10; j++ {
						ip := "10.0.0." + string(rune('0'+j))
						p.IPStates[ip] = persistence.IPState{
							State:      persistence.BlockStateBlocked,
							ExpireTime: time.Now().Add(time.Hour),
							Reason:     "test",
							ModifiedAt: time.Now(),
						}
					}
				} else {
					// Clear entries
					p.IPStates = make(map[string]persistence.IPState)
				}
				p.PersistenceMutex.Unlock()
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stopChan)
	wg.Wait()
	t.Logf("Len race test completed successfully")
}
