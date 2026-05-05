package main

import (
	"sync"
	"testing"
	"time"

	"bot-detector/internal/app"
	"bot-detector/internal/persistence"
)

// TestSQLiteRaceConditions tests concurrent access to SQLite database
// to ensure no race conditions exist when:
// 1. Health check callback reads state (resync)
// 2. State restoration writes state (fetchInitialStateFromCluster, replayJournalAfter)
// 3. Block/unblock operations write state
// 4. Cleanup reads and deletes state
func TestSQLiteRaceConditions(t *testing.T) {
	db, err := persistence.OpenDB("", true)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer func() { _ = persistence.CloseDB(db) }()

	p := &app.Processor{
		DB:                 db,
		ReadDB:             db,
		PersistenceEnabled: true,
		NowFunc:            time.Now,
	}

	var wg sync.WaitGroup
	stopChan := make(chan struct{})

	// Simulate health check resync callback reading state
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopChan:
				return
			default:
				p.PersistenceMutex.Lock()
				states, _ := persistence.GetAllIPStates(p.DB)
				_ = len(states)
				p.PersistenceMutex.Unlock()
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	// Simulate block operations writing state
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			select {
			case <-stopChan:
				return
			default:
				now := time.Now()
				ip := "172.16.0." + string(rune('0'+(i%10)))
				p.PersistenceMutex.Lock()
				_ = persistence.UpsertIPState(p.DB, ip, persistence.BlockStateBlocked, now.Add(time.Hour), "block", now, now)
				_ = persistence.InsertEvent(p.DB, now, persistence.EventTypeBlock, ip, "block", time.Hour, "")
				p.PersistenceMutex.Unlock()
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	// Simulate unblock operations
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			select {
			case <-stopChan:
				return
			default:
				now := time.Now()
				ip := "172.16.0." + string(rune('0'+(i%10)))
				p.PersistenceMutex.Lock()
				_ = persistence.UpsertIPState(p.DB, ip, persistence.BlockStateUnblocked, now, "unblock", now, time.Time{})
				p.PersistenceMutex.Unlock()
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	// Simulate cleanup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			select {
			case <-stopChan:
				return
			default:
				_, _ = persistence.CleanupExpiredBlocks(p.DB, time.Now())
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stopChan)
	wg.Wait()

	states, _ := persistence.GetAllIPStates(p.DB)
	t.Logf("Test completed successfully with %d IPs in state", len(states))
}
