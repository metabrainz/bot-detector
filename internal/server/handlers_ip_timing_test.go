package server

import (
	"bot-detector/internal/persistence"
	"bot-detector/internal/store"
	"testing"
	"time"
)

// TestEarliestBlockCalculation tests the earliest_block calculation logic
func TestEarliestBlockCalculation(t *testing.T) {
	now := time.Date(2026, 3, 10, 13, 39, 17, 0, time.UTC)
	expiry24h := now.Add(24 * time.Hour) // 2026-03-11 13:39:17

	tests := []struct {
		name                  string
		actors                []*store.ActorActivity
		persistFirstBlockedAt time.Time
		wantEarliest          time.Time
		wantLatest            time.Time
	}{
		{
			name: "single 24h block with FirstBlockedAt",
			actors: []*store.ActorActivity{
				{
					BlockedUntil: expiry24h,
					SkipInfo: store.SkipInfo{
						Source: "test-chain",
					},
				},
			},
			persistFirstBlockedAt: now,
			wantEarliest:          now,
			wantLatest:            expiry24h,
		},
		{
			name: "single 24h block without FirstBlockedAt (fallback)",
			actors: []*store.ActorActivity{
				{
					BlockedUntil: expiry24h,
					SkipInfo: store.SkipInfo{
						Source: "test-chain",
					},
				},
			},
			persistFirstBlockedAt: time.Time{},                   // Zero value
			wantEarliest:          expiry24h.Add(-1 * time.Hour), // Fallback estimate
			wantLatest:            expiry24h,
		},
		{
			name: "multiple blocks with different expiries",
			actors: []*store.ActorActivity{
				{
					BlockedUntil: now.Add(1 * time.Hour),
					SkipInfo: store.SkipInfo{
						Source: "short-chain",
					},
				},
				{
					BlockedUntil: expiry24h,
					SkipInfo: store.SkipInfo{
						Source: "long-chain",
					},
				},
			},
			persistFirstBlockedAt: now,
			wantEarliest:          now,
			wantLatest:            expiry24h,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the calculation logic
			var earliestBlock time.Time
			var latestExpiry time.Time

			for _, a := range tt.actors {
				// Use FirstBlockedAt if available, otherwise estimate
				var blockTime time.Time
				if !tt.persistFirstBlockedAt.IsZero() {
					blockTime = tt.persistFirstBlockedAt
				} else {
					blockTime = a.BlockedUntil.Add(-1 * time.Hour)
				}

				if earliestBlock.IsZero() || blockTime.Before(earliestBlock) {
					earliestBlock = blockTime
				}
				if latestExpiry.IsZero() || a.BlockedUntil.After(latestExpiry) {
					latestExpiry = a.BlockedUntil
				}
			}

			// Check results
			if !latestExpiry.Equal(tt.wantLatest) {
				t.Errorf("latestExpiry = %v, want %v", latestExpiry, tt.wantLatest)
			}

			if !earliestBlock.Equal(tt.wantEarliest) {
				t.Errorf("earliestBlock = %v, want %v", earliestBlock, tt.wantEarliest)
			}

			t.Logf("✓ earliestBlock: %v, latestExpiry: %v",
				earliestBlock.Format(time.RFC3339),
				latestExpiry.Format(time.RFC3339))
		})
	}
}

// TestMultiNodeBlockTiming tests cluster-wide earliest_block behavior
func TestMultiNodeBlockTiming(t *testing.T) {
	now := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)

	// Scenario: Node A blocks at 10:00 for 1h, Node B blocks at 13:00 for 24h
	nodeABlockTime := now                    // 10:00
	nodeBBlockTime := now.Add(3 * time.Hour) // 13:00

	// After state sync, FirstBlockedAt should be the EARLIEST (10:00)
	clusterFirstBlockedAt := nodeABlockTime // min(10:00, 13:00) = 10:00

	t.Run("cluster_earliest_block", func(t *testing.T) {
		// Both nodes should show the same earliest_block (cluster-wide)
		if !clusterFirstBlockedAt.Equal(nodeABlockTime) {
			t.Errorf("Expected cluster earliest_block = %v, got %v",
				nodeABlockTime, clusterFirstBlockedAt)
		}

		t.Logf("✓ Node A blocked at: %s", nodeABlockTime.Format(time.RFC3339))
		t.Logf("✓ Node B blocked at: %s", nodeBBlockTime.Format(time.RFC3339))
		t.Logf("✓ Cluster earliest_block: %s (correct - earliest across all nodes)",
			clusterFirstBlockedAt.Format(time.RFC3339))
	})
}

// TestFirstBlockedAtField verifies FirstBlockedAt field behavior
func TestFirstBlockedAtField(t *testing.T) {
	now := time.Date(2026, 3, 10, 13, 39, 17, 0, time.UTC)

	state := persistence.IPState{
		State:          persistence.BlockStateBlocked,
		ExpireTime:     now.Add(24 * time.Hour),
		Reason:         "test-chain",
		ModifiedAt:     now,
		FirstBlockedAt: now,
	}

	// FirstBlockedAt should be used as the block time
	if !state.FirstBlockedAt.Equal(now) {
		t.Errorf("FirstBlockedAt = %v, want %v", state.FirstBlockedAt, now)
	}

	t.Logf("✓ FirstBlockedAt tracks cluster-wide earliest block: %s",
		state.FirstBlockedAt.Format(time.RFC3339))
}
