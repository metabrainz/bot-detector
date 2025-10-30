package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// --- CONFIGURATION GLOBAL VARS (Set by CLI flags) ---
var (
	LogFilePath    string
	BlockedMapPath string

	YAMLFilePath       string
	PollingIntervalStr string

	CleanupIntervalStr string
	IdleTimeoutStr     string // Duration an IP must be inactive before its state is purged.

	LogLevelStr string
	DryRun      bool
	TestLogPath string

	PollingInterval time.Duration
	CleanupInterval time.Duration
	IdleTimeout     time.Duration
)

func init() {
	flag.StringVar(&LogFilePath, "log-path", "/var/log/http/access.log", "Path to the live access log file to tail (ignored in dry-run).")
	flag.StringVar(&BlockedMapPath, "map-path", "/etc/haproxy/maps/blocked_ips.map", "Path to the HAProxy map file used for dynamic IP blocking (ignored in dry-run).")

	flag.StringVar(&YAMLFilePath, "yaml-path", "chains.yaml", "Path to the YAML configuration file defining behavioral chains.")
	flag.StringVar(&PollingIntervalStr, "poll-interval", "5s", "Interval (e.g., '10s', '1m') to check the YAML file for changes (ignored in dry-run).")

	flag.StringVar(&CleanupIntervalStr, "cleanup-interval", "1m", "Interval (e.g., '5m') to run the routine that cleans up idle IP state.")
	flag.StringVar(&IdleTimeoutStr, "idle-timeout", "30m", "Duration (e.g., '45m') an IP must be inactive before its state is purged from memory.")

	flag.StringVar(&LogLevelStr, "log-level", "warning", "Set minimum log level to display: critical, error, warning, info, debug.")
	flag.BoolVar(&DryRun, "dry-run", false, "If true, runs in test mode: skips HAProxy/live logging, ignores cleanup/polling, and uses --test-log.")
	flag.StringVar(&TestLogPath, "test-log", "test_access.log", "Path to a static file containing log lines for dry-run testing.")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "A behavioral bot detection tool that monitors logs and blocks malicious IPs via the HAProxy Runtime API.\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nMemory and CPU are optimized by pre-compiling regexes and using the cleanup routine.\n")
	}
}

// ParseDurations validates and parses CLI duration flags.
func ParseDurations() error {
	var err error

	if level, ok := LogLevelMap[strings.ToLower(LogLevelStr)]; ok {
		CurrentLogLevel = level
	} else {
		return fmt.Errorf("invalid log-level '%s'. Must be one of: critical, error, warning, info, debug", LogLevelStr)
	}

	if !DryRun {
		PollingInterval, err = time.ParseDuration(PollingIntervalStr)
		if err != nil {
			return fmt.Errorf("invalid poll-interval format: %w", err)
		}
		CleanupInterval, err = time.ParseDuration(CleanupIntervalStr)
		if err != nil {
			return fmt.Errorf("invalid cleanup-interval format: %w", err)
		}
		IdleTimeout, err = time.ParseDuration(IdleTimeoutStr)
		if err != nil {
			return fmt.Errorf("invalid idle-timeout format: %w", err)
		}
	}
	return nil
}
