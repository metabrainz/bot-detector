package main

import "flag"

// isTesting returns true if the code is running as part of a "go test" command.
// It works by checking for the presence of the "test.v" flag, which is
// automatically added by the Go testing framework.
func isTesting() bool {
	return flag.Lookup("test.v") != nil
}
