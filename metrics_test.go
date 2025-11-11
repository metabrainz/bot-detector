package main

import (
	"testing"
)

func TestNewMetrics(t *testing.T) {
	// Act
	m := NewMetrics()

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
