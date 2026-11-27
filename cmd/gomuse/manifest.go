package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gomuse/internal/muse"
)

// manifestFile is one recorded file as listed in the manifest.
type manifestFile struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	Rows   int    `json:"rows"`
	SHA256 string `json:"sha256"`
}

// manifest describes one recording: what was written, when, how it ended, and
// a SHA-256 of every file, so a later copy can be checked against the original.
type manifest struct {
	Tool        string    `json:"tool"`
	StartedUTC  time.Time `json:"started_utc"`
	FinishedUTC time.Time `json:"finished_utc"`
	StopReason  string    `json:"stop_reason"`
	Demo        bool      `json:"demo"`
	Dropped     uint64    `json:"dropped_chunks"`
	Malformed   int       `json:"malformed_chunks"`
	// Timing says how the timestamps in the CSV files were produced. Absent in
	// demo mode, which has no headband.
	Timing *muse.Timing   `json:"timing,omitempty"`
	Files  []manifestFile `json:"files"`
}

// hashFile returns the SHA-256 and size of the file at path.
func hashFile(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err = io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// writeManifest writes manifest.json next to the recordings, via a temporary
// file and a rename so a reader never sees a half-written manifest. The file
// names are stored relative to dir, so the folder can be moved or copied.
func writeManifest(dir string, m manifest, paths []string, rows func(path string) int) (string, error) {
	for _, p := range paths {
		sum, size, err := hashFile(p)
		if err != nil {
			return "", fmt.Errorf("manifest: hash %s: %w", p, err)
		}
		m.Files = append(m.Files, manifestFile{
			Name:   filepath.Base(p),
			Bytes:  size,
			Rows:   rows(p),
			SHA256: sum,
		})
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	final := filepath.Join(dir, "manifest.json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("manifest: write: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", fmt.Errorf("manifest: rename: %w", err)
	}
	return final, nil
}
