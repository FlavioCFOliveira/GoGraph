// Package snapshot serialises the durable on-disk representation of
// a gograph snapshot (CSR + LPG + schema) and reads it back into a
// fresh process.
//
// A snapshot is a directory containing a manifest.json plus one
// binary file per kept-on-disk component. Publication is atomic on
// any POSIX filesystem: the writer assembles the new directory under
// a sibling .tmp path, fsyncs every file, then renames the .tmp
// directory to its final name. Concurrent readers continue using
// the previous directory until they re-open.
package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ManifestVersion is the on-disk schema version this build writes.
const ManifestVersion = 1

// ErrManifestUnsupported is returned by [LoadManifest] when the
// manifest version is newer than this build understands.
var ErrManifestUnsupported = errors.New("snapshot: manifest version unsupported")

// ErrManifestCorrupted is returned when the manifest does not parse
// as JSON or its file list disagrees with what is on disk.
var ErrManifestCorrupted = errors.New("snapshot: manifest corrupted")

// FileEntry records one component file inside a snapshot directory.
type FileEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	CRC32C uint32 `json:"crc32c"`
}

// Manifest is the JSON-encoded index of a snapshot directory.
type Manifest struct {
	Version   int         `json:"version"`
	CreatedAt time.Time   `json:"created_at"`
	Order     uint64      `json:"order"`
	Size      uint64      `json:"size"`
	Files     []FileEntry `json:"files"`
}

// WriteManifest writes m to w in canonical (pretty-printed) JSON.
func WriteManifest(w io.Writer, m Manifest) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// LoadManifest parses m from r. Returns [ErrManifestUnsupported]
// when the version is newer than this build.
func LoadManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrManifestCorrupted, err)
	}
	if m.Version > ManifestVersion {
		return Manifest{}, fmt.Errorf("%w: %d", ErrManifestUnsupported, m.Version)
	}
	return m, nil
}

// ReadManifestFile is a convenience wrapper around [os.Open] +
// [LoadManifest].
func ReadManifestFile(path string) (Manifest, error) {
	f, err := os.Open(path) //nolint:gosec // caller-supplied path
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = f.Close() }()
	return LoadManifest(f)
}
