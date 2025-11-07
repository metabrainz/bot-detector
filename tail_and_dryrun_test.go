package main

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadLineWithLimit(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		limit         int
		expectedLine  string
		expectedError error
	}{
		{
			name:          "Line within limit",
			input:         "hello world\n",
			limit:         100,
			expectedLine:  "hello world",
			expectedError: nil,
		},
		{
			name:          "Line at limit",
			input:         "1234567890\n",
			limit:         10,
			expectedLine:  "1234567890",
			expectedError: nil,
		},
		{
			name:          "Line one byte over limit",
			input:         "12345678901\n",
			limit:         10,
			expectedLine:  "1234567890",
			expectedError: ErrLineSkipped,
		},
		{
			name:          "Line exceeds limit",
			input:         "this line is too long\n",
			limit:         10,
			expectedLine:  "this line ",
			expectedError: ErrLineSkipped,
		},
		{
			name:          "EOF without newline",
			input:         "eof",
			limit:         100,
			expectedLine:  "eof",
			expectedError: io.EOF, // Correctly expect EOF
		},
		{
			name:          "Empty input",
			input:         "",
			limit:         100,
			expectedLine:  "",
			expectedError: io.EOF,
		},
		{
			name:          "Windows EOL (CRLF)",
			input:         "windows line\r\n",
			limit:         100,
			expectedLine:  "windows line",
			expectedError: nil,
		},
		{
			name:          "Windows EOL over limit",
			input:         "this is a long windows line\r\n",
			limit:         10,
			expectedLine:  "this is a ",
			expectedError: ErrLineSkipped,
		},
		{
			name:          "Classic Mac EOL (CR)",
			input:         "mac line\rnext line",
			limit:         100,
			expectedLine:  "mac line",
			expectedError: nil,
		},
		{
			name:          "Classic Mac EOL over limit",
			input:         "this is a long mac line\rnext line",
			limit:         10,
			expectedLine:  "this is a ",
			expectedError: ErrLineSkipped,
		},
		{
			name:          "Mixed EOLs (Windows then Unix)",
			input:         "line1\r\nline2\n",
			limit:         100,
			expectedLine:  "line1",
			expectedError: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			line, err := ReadLineWithLimit(reader, tt.limit)

			if line != tt.expectedLine {
				t.Errorf("Line content mismatch. Expected '%s', got '%s'", tt.expectedLine, line)
			}

			if !errors.Is(err, tt.expectedError) {
				t.Errorf("Expected error '%v', got '%v'", tt.expectedError, err)
			}
		})
	}
}
