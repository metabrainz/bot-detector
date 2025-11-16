package server

import (
	"archive/tar"
	"bot-detector/internal/logging"
	"compress/gzip"
	"fmt"
	"net/http"
	"path/filepath"
	"time"
)

// archiveHandler creates an HTTP handler that serves a .tar.gz archive of the
// main configuration file and all its file dependencies.
func archiveHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mainConfig, modTime, deps, configPath, err := p.GetConfigForArchive()
		if err != nil {
			http.Error(w, "Failed to get configuration for archive", http.StatusInternalServerError)
			p.Log(0, "ARCHIVE_ERROR", "Failed to get config for archive: %v", err)
			return
		}

		// Determine the latest modification time among the main config and all dependencies.
		latestModTime := modTime
		for _, dep := range deps {
			if dep.CurrentStatus != nil && dep.CurrentStatus.ModTime.After(latestModTime) {
				latestModTime = dep.CurrentStatus.ModTime
			}
		}

		// Set headers
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="config_archive.tar.gz"`)
		w.Header().Set("Last-Modified", latestModTime.UTC().Format(http.TimeFormat))

		// Create a gzip writer that writes to the HTTP response writer.
		gw := gzip.NewWriter(w)
		defer func() {
			if err := gw.Close(); err != nil {
				logging.LogOutput(logging.LevelError, "archiveHandler", "Error closing gzip writer: %v", err)
			}
		}()

		// Create a tar writer that writes to the gzip writer.
		tw := tar.NewWriter(gw)
		defer func() {
			if err := tw.Close(); err != nil {
				logging.LogOutput(logging.LevelError, "archiveHandler", "Error closing tar writer: %v", err)
			}
		}()

		// Add the main config.yaml to the archive.
		if err := addBytesToTar(tw, "config.yaml", mainConfig, modTime); err != nil {
			p.Log(0, "ARCHIVE_ERROR", "Failed to add main config to archive: %v", err)
			// Don't write an HTTP error here as headers are likely already sent.
			return
		}

		// Add all file dependencies to the archive.
		configDir := filepath.Dir(configPath)
		for path, dep := range deps {
			if dep.CurrentStatus.Status != 0 { // Assuming 0 is FileStatusUnknown/FileStatusLoaded
				// Get the relative path of the dependency from the main config directory.
				relPath, err := filepath.Rel(configDir, path)
				if err != nil {
					p.Log(0, "ARCHIVE_WARN", "Could not determine relative path for '%s': %v", path, err)
					// Use the absolute path as a fallback.
					relPath = path
				}

				// Join the content of the file (slice of strings) into a single string.
				content := ""
				for _, line := range dep.Content {
					content += line + "\n"
				}

				if err := addBytesToTar(tw, relPath, []byte(content), dep.CurrentStatus.ModTime); err != nil {
					p.Log(0, "ARCHIVE_ERROR", "Failed to add dependency '%s' to archive: %v", path, err)
					return // Stop processing if there's an error.
				}
			}
		}
	}
}

// addBytesToTar is a helper function to add a file (from a byte slice) to a tar archive.
func addBytesToTar(tw *tar.Writer, name string, content []byte, modTime time.Time) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0644,
		Size:    int64(len(content)),
		ModTime: modTime,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("could not write tar header for '%s': %w", name, err)
	}
	if _, err := tw.Write(content); err != nil {
		return fmt.Errorf("could not write content for '%s' to tar archive: %w", name, err)
	}
	return nil
}
