package types

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"time"
)

// FileDependencyStatus holds the complete state of a file at a specific point in time.
type FileDependencyStatus struct {
	ModTime  time.Time
	Status   FileStatus
	Checksum string // SHA256 checksum of the content to detect no-op changes.
	Error    error  // Store the error if status is Error
}

// Clone creates a deep copy of the FileDependencyStatus object.
func (fds *FileDependencyStatus) Clone() *FileDependencyStatus {
	if fds == nil {
		return nil
	}
	return &FileDependencyStatus{
		ModTime:  fds.ModTime,
		Status:   fds.Status,
		Checksum: fds.Checksum,
		Error:    fds.Error, // errors are immutable
	}
}

// FileDependency represents a file that the configuration depends on, tracking its state over time.
type FileDependency struct {
	Path           string
	PreviousStatus *FileDependencyStatus
	CurrentStatus  *FileDependencyStatus
	Content        []string // Cached content from the last successful load
}

// Clone creates a deep copy of the FileDependency object.
func (fd *FileDependency) Clone() *FileDependency {
	if fd == nil {
		return nil
	}
	// Content is a slice of strings, which is a reference type, but since the
	// content itself is immutable strings, a shallow copy of the slice is sufficient.
	contentCopy := make([]string, len(fd.Content))
	copy(contentCopy, fd.Content)

	return &FileDependency{
		Path:           fd.Path,
		PreviousStatus: fd.PreviousStatus.Clone(),
		CurrentStatus:  fd.CurrentStatus.Clone(),
		Content:        contentCopy,
	}
}

// UpdateStatus polls the file on disk and updates its CurrentStatus.
// It always preserves the previous state before updating.
func (fd *FileDependency) UpdateStatus() {
	// Preserve the last known state.
	fd.PreviousStatus = fd.CurrentStatus

	newStatus := &FileDependencyStatus{}

	info, err := os.Stat(fd.Path)
	if err != nil {
		if os.IsNotExist(err) {
			newStatus.Status = FileStatusMissing
			newStatus.Error = err
		} else {
			// Another error occurred (e.g., permissions).
			newStatus.Status = FileStatusError
			newStatus.Error = err
		}
		fd.CurrentStatus = newStatus
		return
	}

	// File exists, update basic info.
	newStatus.Status = FileStatusLoaded
	newStatus.ModTime = info.ModTime()

	// Always attempt to read the file if it exists and is loaded, to detect read errors (e.g., permissions).
	// This also ensures the checksum is always up-to-date if content changes without ModTime update (rare, but possible).
	content, readErr := ReadLinesFromFile(fd.Path)
	if readErr != nil {
		newStatus.Status = FileStatusError
		newStatus.Error = readErr
	} else {
		newStatus.Checksum = calculateChecksum(content)
	}

	fd.CurrentStatus = newStatus
}

// HasChanged compares the PreviousStatus and CurrentStatus to see if a reload is warranted.
func (fd *FileDependency) HasChanged() bool {
	if fd.PreviousStatus == nil {
		// If there's no previous state, any loaded state is a "change".
		return fd.CurrentStatus.Status == FileStatusLoaded
	}

	// A change is warranted if the status is different, or if the checksums don't match.
	// A simple `touch` will change ModTime but not Checksum, so we rely on checksum.
	return fd.CurrentStatus.Status != fd.PreviousStatus.Status ||
		fd.CurrentStatus.Checksum != fd.PreviousStatus.Checksum
}

// calculateChecksum computes the SHA256 checksum of a slice of strings.
func calculateChecksum(lines []string) string {
	h := sha256.New()
	h.Write([]byte(strings.Join(lines, "\n")))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ReadLinesFromFile is a helper to read a file into a slice of strings, ignoring comments and empty lines.
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

// FileStatus indicates the current state of a file dependency.
type FileStatus int

const (
	FileStatusUnknown FileStatus = iota
	FileStatusLoaded
	FileStatusMissing
	FileStatusError
)

func (fs FileStatus) String() string {
	switch fs {
	case FileStatusUnknown:
		return "Unknown"
	case FileStatusLoaded:
		return "Loaded"
	case FileStatusMissing:
		return "Missing"
	case FileStatusError:
		return "Error"
	default:
		return "Unknown"
	}
}
