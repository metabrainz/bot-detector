package utils

import (
	"html"
	"strings"
	"unicode"
)

// ForLog removes non-printable characters from a string, making it safe for
// internal use and logging. It allows common characters and replaces others.
func ForLog(s string) string {
	// Fast path: scan first, only allocate if needed.
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return strings.Map(func(r rune) rune {
				if unicode.IsPrint(r) {
					return r
				}
				return -1
			}, s)
		}
	}
	return s
}

// ForHTML prepares a string for safe rendering within HTML content. It first
// performs the basic log sanitization and then escapes special HTML characters
// to prevent XSS attacks.
func ForHTML(s string) string {
	// First, do the basic sanitization to remove control characters.
	sanitized := ForLog(s)
	// Then, escape characters that have special meaning in HTML.
	return html.EscapeString(sanitized)
}
