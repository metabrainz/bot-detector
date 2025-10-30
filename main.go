package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	flag.Parse()

	if err := ParseDurations(); err != nil {
		log.Fatalf("[FATAL] Configuration Error: %v", err)
	}

	var err error
	Chains, err = LoadChainsFromYAML()
	if err != nil {
		log.Fatalf("[FATAL] Initial chain load failed: %v", err)
	}
	LogOutput(LevelInfo, "LOAD", "Initial configuration loaded. Loaded %d behavioral chains.", len(Chains))

	if DryRun {
		RunDryRun()
		return
	} else {
		LogOutput(LevelInfo, "INFO", "Running in Production Mode with per-attempt HAProxy Fail-Safe. Log level set to %s. Log line critical limit: %dKB.", strings.ToUpper(LogLevelStr), MaxLogLineSize/1024)

		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

		if fileInfo, err := os.Stat(YAMLFilePath); err == nil {
			LastModTime = fileInfo.ModTime()
		}

		go ChainWatcher()
		go CleanUpIdleActivity()
		go TailLogWithRotation()

		<-stop
		LogOutput(LevelCritical, "SHUTDOWN", "Interrupt signal received. Shutting down gracefully...")
		LogOutput(LevelCritical, "SHUTDOWN", "Exiting.")
	}
}