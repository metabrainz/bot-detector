package metrics_test

import (
	"bot-detector/internal/metrics"
	"testing"
)

func TestNewMetrics(t *testing.T) {
	// Act
	m := metrics.NewMetrics()

	// Assert
	if m == nil {
		t.Fatal("NewMetrics() returned nil")
	}

	// Check that all sync.Map fields are initialized.
	if m.ChainsCompleted == nil {
		t.Error("ChainsCompleted map was not initialized")
	}
	if m.ChainsReset == nil {
		t.Error("ChainsReset map was not initialized")
	}
	if m.ChainsHits == nil {
		t.Error("ChainsHits map was not initialized")
	}
	if m.MatchKeyHits == nil {
		t.Error("MatchKeyHits map was not initialized")
	}
	if m.BlockDurations == nil {
		t.Error("BlockDurations map was not initialized")
	}
	if m.CmdsPerBlocker == nil {
		t.Error("CmdsPerBlocker map was not initialized")
	}

	// Check a few atomic counters to ensure they are zero.
	if m.LinesProcessed.Load() != 0 {
		t.Errorf("Expected LinesProcessed to be 0, got %d", m.LinesProcessed.Load())
	}
	if m.ParseErrors.Load() != 0 {
		t.Errorf("Expected ParseErrors to be 0, got %d", m.ParseErrors.Load())
	}
	if m.BlockActions.Load() != 0 {
		t.Errorf("Expected BlockActions to be 0, got %d", m.BlockActions.Load())
	}
}

func TestParseErrorBuffer(t *testing.T) {
	t.Run("nil when disabled", func(t *testing.T) {
		if b := metrics.NewParseErrorBuffer(0); b != nil {
			t.Error("expected nil for cap=0")
		}
		if b := metrics.NewParseErrorBuffer(-1); b != nil {
			t.Error("expected nil for cap=-1")
		}
	})

	t.Run("partial fill", func(t *testing.T) {
		b := metrics.NewParseErrorBuffer(5)
		b.Add("a")
		b.Add("b")
		entries := b.Entries()
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(entries))
		}
		if entries[0] != "b" || entries[1] != "a" {
			t.Errorf("expected [b a], got %v", entries)
		}
	})

	t.Run("wrap around", func(t *testing.T) {
		b := metrics.NewParseErrorBuffer(3)
		b.Add("a")
		b.Add("b")
		b.Add("c")
		b.Add("d") // overwrites "a"
		entries := b.Entries()
		if len(entries) != 3 {
			t.Fatalf("expected 3 entries, got %d", len(entries))
		}
		if entries[0] != "d" || entries[1] != "c" || entries[2] != "b" {
			t.Errorf("expected [d c b], got %v", entries)
		}
	})
}
