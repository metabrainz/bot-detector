package main

import (
	"bufio"
	"errors"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// DryRunLogProcessor reads and processes a static log file for testing.
func DryRunLogProcessor(done chan<- struct{}) {
	LogOutput(LevelInfo, "DRYRUN", "MODE: Reading test logs from %s...", TestLogPath)

	file, err := os.Open(TestLogPath)
	if err != nil {
		log.Fatalf("[FATAL] Dry Run Failed: Could not open test log file %s: %v", TestLogPath, err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNumber := 0

	for {
		lineNumber++

		line, err := ReadLineWithLimit(reader, MaxLogLineSize)

		if err != nil {
			if errors.Is(err, io.EOF) {
				// Process final line fragment if present
				if line != "" {
					ProcessLogLine(line, lineNumber)
				}
				break
			}
			if errors.Is(err, ErrLineSkipped) {
				LogOutput(LevelWarning, "SKIPPED", "Line %d exceeded critical limit and was skipped.", lineNumber)
				lineNumber--
				continue
			}

			LogOutput(LevelError, "DRYRUN_ERROR", "Reading log file: %v. Exiting dry-run loop.", err)
			break
		}

		ProcessLogLine(line, lineNumber)
	}

	LogOutput(LevelInfo, "DRYRUN", "COMPLETED: Processed all lines in test log.")
	LogOutput(LevelDebug, "DRYRUN", "Total lines processed: %d", lineNumber)

	close(done)
}

// RunDryRun orchestrates the dry-run and manages graceful shutdown.
func RunDryRun() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// Channel to signal completion of log processing.
	done := make(chan struct{})

	go func() {
		DryRunLogProcessor(done)
	}()

	LogOutput(LevelInfo, "INFO", "Running in Dry-Run Mode. Log level set to %s. Log line critical limit: %dKB.", strings.ToUpper(LogLevelStr), MaxLogLineSize/1024)

	select {
	case <-done:
		LogOutput(LevelInfo, "DRYRUN", "COMPLETE: Dry-run successfully finished processing log file.")
		LogOutput(LevelInfo, "INFO", "Total distinct IP/UA keys processed: %d", len(DryRunActivityStore))
		LogOutput(LevelCritical, "SHUTDOWN", "Dry-run complete. Exiting.")
	case <-stop:
		LogOutput(LevelCritical, "SHUTDOWN", "Interrupt signal received during dry-run. Shutting down...")
	}

	os.Exit(0)
}

// TailLogWithRotation tails a log file indefinitely, supporting rotation via inode checks.
func TailLogWithRotation() {
	if DryRun {
		return
	}

	LogOutput(LevelInfo, "TAIL", "Starting live log tailing on %s with rotation support...", LogFilePath)

	for {
		file, err := os.OpenFile(LogFilePath, os.O_RDONLY, 0644)
		if err != nil {
			LogOutput(LevelError, "TAIL_ERROR", "Failed to open log file %s: %v. Retrying in 5s.", LogFilePath, err)
			time.Sleep(5 * time.Second)
			continue
		}

		// Seek to the end of the file
		_, err = file.Seek(0, 2)
		if err != nil {
			LogOutput(LevelError, "TAIL_ERROR", "Failed to seek to end of log file: %v. Closing and retrying.", err)
			file.Close()
			time.Sleep(1 * time.Second)
			continue
		}

		initialStat, err := file.Stat()
		if err != nil {
			LogOutput(LevelError, "TAIL_ERROR", "Failed to stat open file: %v. Closing and retrying.", err)
			file.Close()
			time.Sleep(1 * time.Second)
			continue
		}
		// On Linux/Unix, Stat().Sys() returns *syscall.Stat_t
		initialSysStat := initialStat.Sys().(*syscall.Stat_t)
		initialDev := initialSysStat.Dev
		initialIno := initialSysStat.Ino

		reader := bufio.NewReader(file)
		LogOutput(LevelInfo, "TAIL", "Now tailing (Dev: %d, Inode: %d)", initialDev, initialIno)

		lineNumber := 0

		for {
			lineNumber++

			line, err := ReadLineWithLimit(reader, MaxLogLineSize)
			finalErr := err

			if errors.Is(finalErr, ErrLineSkipped) {
				LogOutput(LevelWarning, "SKIPPED", "Live log line exceeded critical limit of %dKB and was skipped.", MaxLogLineSize/1024)
				continue
			}

			if finalErr != nil {
				if finalErr == io.EOF { // Standard check for live tail: sleep and check rotation
					currentStat, statErr := os.Stat(LogFilePath)
					if statErr == nil {
						if currentStat.Size() < initialStat.Size() {
							LogOutput(LevelDebug, "TAIL", "Detected log file size reduction (truncation/rotation). Reopening file.")
							file.Close()
							break
						}
						currentSysStat := currentStat.Sys().(*syscall.Stat_t)
						if currentSysStat.Dev != initialDev || currentSysStat.Ino != initialIno {
							LogOutput(LevelInfo, "TAIL", "Detected log file rotation (Inode changed from %d to %d). Reopening file.", initialIno, currentSysStat.Ino)
							file.Close()
							break
						}
					} else {
						LogOutput(LevelError, "TAIL_ERROR", "Failed to stat log path during EOF check: %v. Reopening in 1s.", statErr)
						time.Sleep(1 * time.Second)
						file.Close()
						break
					}
					time.Sleep(200 * time.Millisecond)
					continue
				} else {
					LogOutput(LevelError, "TAIL_ERROR", "Reading log file: %v. Reopening in 1s.", finalErr)
					time.Sleep(1 * time.Second)
					file.Close()
					break
				}
			}

			ProcessLogLine(line, lineNumber)
		}

		time.Sleep(100 * time.Millisecond)
	}
}
