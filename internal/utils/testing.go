package utils

import (
	"os"
	"strings"
)

// IsTesting returns true if the code is running as part of a "go test" command.
// It works by checking for the presence of the "-test.v" or "-test.run" arguments,
// which are automatically added by the Go testing framework. This is more robust
// than `flag.Lookup` when the global flag set is manipulated during tests.
func IsTesting() bool {
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "-test.") {
			return true
		}
	}
	return false
}
