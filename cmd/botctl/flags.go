package main

import (
	"fmt"
	"strings"
)

// extractStringFlag pulls a --name <value> (or --name=value) flag out of args,
// returning the value and the remaining args. If the flag is absent, value is
// empty. An error is returned when the flag is given without a value.
func extractStringFlag(args []string, name string) (string, []string, error) {
	value := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == name:
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s requires a value", name)
			}
			value = args[i+1]
			i++
		case strings.HasPrefix(arg, name+"="):
			value = strings.TrimPrefix(arg, name+"=")
		default:
			rest = append(rest, arg)
		}
	}
	return value, rest, nil
}

// extractBoolFlag pulls a boolean --name flag out of args, returning whether it
// was present and the remaining args.
func extractBoolFlag(args []string, name string) (bool, []string) {
	found := false
	rest := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == name {
			found = true
			continue
		}
		rest = append(rest, arg)
	}
	return found, rest
}
