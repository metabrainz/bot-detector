package server

import (
	"testing"
	"time"

	"bot-detector/internal/persistence"
	"bot-detector/internal/store"
)

func TestEarliestBlockCalculation(t *testing.T) {
	now := time.Date(2026, 3, 10, 13, 39, 17, 0, time.UTC)
	expiry24h := now.Add(24 * time.Hour) // 2026-03-11 13:39:17

	tests := []struct {
		name              string
		actors            []*store.ActorActivity
		persistModifiedAt time.Time
		wantEarliest      time.Time
		wantLatest        time.Time
	}{
		{
			name: "single 24h block with ModifiedAt",
			actors: []*store.ActorActivity{
				{
					BlockedUntil: expiry24h,
					SkipInfo: store.SkipInfo{
						Source: "test-chain",
					},
				},
			},
			persistModifiedAt: now,
			wantEarliest:      now,
			wantLatest:        expiry24h,
		},
		{
			name: "single 24h block without ModifiedAt (fallback)",
			actors: []*store.ActorActivity{
				{
					BlockedUntil: expiry24h,
					SkipInfo: store.SkipInfo{
						Source: "test-chain",
					},
				},
			},
			persistModifiedAt: time.Time{},                   // Zero value
			wantEarliest:      expiry24h.Add(-1 * time.Hour), // Fallback estimate
			wantLatest:        expiry24h,
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
			persistModifiedAt: now,
			wantEarliest:      now,
			wantLatest:        expiry24h,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the new calculation logic
			var earliestBlock time.Time
			var latestExpiry time.Time

			for _, a := range tt.actors {
				// Use ModifiedAt if available, otherwise estimate
				var blockTime time.Time
				if !tt.persistModifiedAt.IsZero() {
					blockTime = tt.persistModifiedAt
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

func TestEarliestBlockWithIPState(t *testing.T) {
	// Test that demonstrates the issue: we don't have block duration in IPState
	now := time.Date(2026, 3, 10, 13, 39, 17, 0, time.UTC)

	state := persistence.IPState{
		State:      persistence.BlockStateBlocked,
		ExpireTime: now.Add(24 * time.Hour),
		Reason:     "test-chain",
		ModifiedAt: now, // This is when it was blocked!
	}

	// We could use ModifiedAt as the block time
	blockTime := state.ModifiedAt
	expiry := state.ExpireTime

	if !blockTime.Equal(now) {
		t.Errorf("blockTime from ModifiedAt = %v, want %v", blockTime, now)
	}

	if !expiry.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("expiry = %v, want %v", expiry, now.Add(24*time.Hour))
	}

	t.Logf("ModifiedAt can be used as block time: %v", blockTime.Format(time.RFC3339))
}
