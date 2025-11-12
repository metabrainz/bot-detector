package utils

import (
	"fmt"
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
