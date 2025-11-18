package server

import (
	"archive/tar"
	"bot-detector/internal/logging"
	"bot-detector/internal/types"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
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

		// Create a buffer to hold the entire archive so we can compute a checksum.
		var buf bytes.Buffer

		// Create a gzip writer that writes to the buffer.
		gw := gzip.NewWriter(&buf)

		// Create a tar writer that writes to the gzip writer.
		tw := tar.NewWriter(gw)

		// Add the main config.yaml to the archive.
		if err := addBytesToTar(tw, "config.yaml", mainConfig, modTime); err != nil {
			http.Error(w, "Failed to create archive", http.StatusInternalServerError)
			p.Log(0, "ARCHIVE_ERROR", "Failed to add main config to archive: %v", err)
			return
		}

		// Add all file dependencies to the archive.
		configDir := filepath.Dir(configPath)
		for path, dep := range deps {
			// Only add files that are currently loaded to the archive.
			if dep.CurrentStatus != nil && dep.CurrentStatus.Status == types.FileStatusLoaded {
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
					http.Error(w, "Failed to create archive", http.StatusInternalServerError)
					p.Log(0, "ARCHIVE_ERROR", "Failed to add dependency '%s' to archive: %v", path, err)
					return
				}
			}
		}

		// Close the tar and gzip writers to flush all data to the buffer.
		if err := tw.Close(); err != nil {
			http.Error(w, "Failed to finalize archive", http.StatusInternalServerError)
			logging.LogOutput(logging.LevelError, "archiveHandler", "Error closing tar writer: %v", err)
			return
		}
		if err := gw.Close(); err != nil {
			http.Error(w, "Failed to finalize archive", http.StatusInternalServerError)
			logging.LogOutput(logging.LevelError, "archiveHandler", "Error closing gzip writer: %v", err)
			return
		}

		// Compute SHA256 checksum of the archive content.
		archiveBytes := buf.Bytes()
		hash := sha256.Sum256(archiveBytes)
		etag := fmt.Sprintf("\"%x\"", hash)

		// Set headers (including ETag with checksum).
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="config_archive.tar.gz"`)
		w.Header().Set("Last-Modified", latestModTime.UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", etag)

		// Write the buffered archive to the response.
		if _, err := w.Write(archiveBytes); err != nil {
			p.Log(0, "ARCHIVE_ERROR", "Failed to write archive to response: %v", err)
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
