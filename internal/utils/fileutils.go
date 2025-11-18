package utils

import (
	"bufio"
	"os"
	"strings"
)

func ReadLinesFromFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		rawLine := scanner.Text()

		// Check if the line is empty or a comment after trimming for check.
		trimmedForCheck := strings.TrimSpace(rawLine)
		if trimmedForCheck == "" || strings.HasPrefix(trimmedForCheck, "#") {
			continue
		}

		// Add the raw line to be processed by the caller.
		lines = append(lines, rawLine)
	}
	return lines, scanner.Err()
}
