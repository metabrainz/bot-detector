package utils

// SkipType defines the reason an actor's log entry was skipped.
type SkipType byte

const (
	// SkipTypeNone is the zero value, indicating no skip.
	SkipTypeNone SkipType = iota
	SkipTypeGoodActor
	SkipTypeBlocked
)
