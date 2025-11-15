package utils

import (
	"strconv"
)

// ParseInt64 parses a string to an int64, returning an error if parsing fails.
func ParseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// ParseInt parses a string to an int, returning an error if parsing fails.
func ParseInt(s string) (int, error) {
	val, err := strconv.ParseInt(s, 10, 0) // 0 for int, which is 32 or 64 bits depending on system
	return int(val), err
}
