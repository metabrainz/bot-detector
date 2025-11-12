package utils

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	week = 7 * 24 * time.Hour
	day  = 24 * time.Hour
	hour = time.Hour
	min  = time.Minute
	sec  = time.Second
	msec = time.Millisecond
	usec = time.Microsecond
	nsec = time.Nanosecond
)

// FormatDuration converts a time.Duration into a canonical, extended Go duration string.
// It supports weeks (w) and days (d) and omits zero-value units for brevity.
// For example, 1h0m0s becomes "1h", and 24h becomes "1d".
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}

	var b strings.Builder

	units := []struct {
		unit time.Duration
		sym  string
	}{
		{week, "w"}, {day, "d"}, {hour, "h"}, {min, "m"}, {sec, "s"},
		{msec, "ms"}, {usec, "µs"}, {nsec, "ns"},
	}

	remaining := d
	for _, u := range units {
		if remaining >= u.unit {
			val := remaining / u.unit
			b.WriteString(fmt.Sprintf("%d%s", val, u.sym))
			remaining %= u.unit
		}
	}

	return b.String()
}

// ParseDuration extends time.ParseDuration to support 'd' for days and 'w' for weeks.
// It converts these units to hours ('h') before parsing, as Go's standard parser
// does not support them. It also requires units to be in descending order of magnitude.
func ParseDuration(durationStr string) (time.Duration, error) {
	// An empty string is not a valid duration.
	if durationStr == "" {
		return 0, fmt.Errorf("time: invalid duration %q", durationStr)
	}

	// Regex to find numbers (including decimals) followed by 'w' or 'd' units.
	re := regexp.MustCompile(`(\d*\.?\d+)([wd])`)

	var totalHours float64
	var lastUnit rune

	// Find all 'w' and 'd' matches to calculate total hours and check order.
	matches := re.FindAllStringSubmatch(durationStr, -1)
	for _, match := range matches {
		valueStr := match[1]
		unit := rune(match[2][0])

		value, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			// This is unlikely if the regex is correct, but handle it.
			return 0, fmt.Errorf("invalid number in duration: %s", valueStr)
		}

		// Check if units are in descending order of magnitude (w then d).
		if lastUnit != 0 && unit == 'w' && lastUnit == 'd' {
			return 0, fmt.Errorf("invalid duration: units must be in descending order of magnitude")
		}

		switch unit {
		case 'w':
			totalHours += value * 7 * 24
		case 'd':
			totalHours += value * 24
		}
		lastUnit = unit
	}

	// Remove the 'w' and 'd' parts from the original string.
	remainingStr := re.ReplaceAllString(durationStr, "")

	// Prepend the calculated hours to the remaining standard Go duration string.
	finalStr := fmt.Sprintf("%fh%s", totalHours, remainingStr)

	return time.ParseDuration(finalStr)
}
