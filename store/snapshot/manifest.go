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
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ManifestVersion is the highest on-disk schema version this build
// understands. The current build writes version 3 manifests via
// [WriteSnapshotFull] when N=string (CSR + labels + properties +
// mapper, fully self-sufficient on load), version 2 manifests via the
// same writer for non-string N (CSR + labels + properties, requires
// WAL replay to reconstruct the natural-key mapper), and version 1
// manifests via the legacy [WriteSnapshotCSR] code path (CSR-only
// snapshots). The loader transparently accepts all three.
const ManifestVersion = 3

// manifestVersionV2 is the schema version emitted by [WriteSnapshotFull]
// when the underlying [graph.Mapper] is keyed by a comparable type
// other than string (or any future type for which the writer cannot
// persist the interning table). v2 snapshots remain self-consistent
// for CSR + labels + properties but require the surrounding WAL to
// re-intern keys at recovery time.
const manifestVersionV2 = 2

// manifestVersionLegacy is the schema version emitted by
// [WriteSnapshotCSR] and [WriteSnapshotCSRCtx]. Those writers retain
// the v1 shape on disk so existing readers and the v1 fixture
// continue to load bit-for-bit unchanged.
const manifestVersionLegacy = 1

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

// GraphConfig is the JSON-persisted shape of the originating graph's
// adjacency-list configuration. It mirrors the directed/multigraph
// flags of [adjlist.Config] without importing that package, so the
// snapshot manifest stays decoupled from the graph backend. The
// snapshot writer fills it from the live graph; recovery reads it to
// reconstruct the same variant.
//
// Only the shape-defining flags are persisted. [adjlist.Config.MaxShardCapacity]
// is deliberately omitted: it is a runtime growth bound, not a property
// of the stored graph, and re-imposing it at recovery time could make
// recovery itself fail with [adjlist.ErrShardFull] while replaying data
// that legitimately exceeds the cap. A recovered graph is therefore
// always reconstructed unbounded.
type GraphConfig struct {
	// Directed records whether AddEdge was a directed insertion in the
	// originating graph.
	Directed bool `json:"directed"`
	// Multigraph records whether the originating graph allowed parallel
	// edges between the same ordered endpoint pair.
	Multigraph bool `json:"multigraph"`
	// Weightless records whether the originating graph stored no per-edge
	// weight column (adjlist.Config.Weightless, #1650). It is omitempty and
	// backward-compatible: a snapshot written before this field, or by a
	// weighted graph, omits it, so it decodes to false (weighted) — the prior
	// behaviour. A recovered weightless graph stays weightless, preserving the
	// per-edge memory saving across a restart rather than re-allocating a
	// zero-filled weight column.
	Weightless bool `json:"weightless,omitempty"`
}

// Manifest is the JSON-encoded index of a snapshot directory.
//
// Indexes is the secondary-index sub-manifest: it carries one
// [IndexFileEntry] per file written under indexes/<name>.bin. The
// field is omitted from the JSON form when empty so v2 manifests
// produced before this extension are byte-identical to the ones
// produced by current builds when no indexes are registered.
//
// GraphConfig records the originating graph's directed/multigraph
// shape. It is a pointer with omitempty so it is dropped from the JSON
// form entirely when nil — every snapshot written before this field
// existed (and the CSR-only legacy writer, which has no live graph to
// read) is therefore byte-identical to what it would have been. A
// reader that finds the field absent must default the configuration to
// the historical recovery behaviour ([adjlist.Config]{Directed: true,
// Multigraph: true}); see [store/recovery.Open]. Only NEW snapshots
// produced by the full writer carry the real config.
type Manifest struct {
	Version     int              `json:"version"`
	CreatedAt   time.Time        `json:"created_at"`
	Order       uint64           `json:"order"`
	Size        uint64           `json:"size"`
	Files       []FileEntry      `json:"files"`
	Indexes     []IndexFileEntry `json:"indexes,omitempty"`
	GraphConfig *GraphConfig     `json:"graph_config,omitempty"`
}

// WriteManifest writes m to w in canonical (pretty-printed) JSON.
//
//nolint:gocritic // public API: Manifest is passed by value to preserve the existing call sites; the encoder only reads from it.
func WriteManifest(w io.Writer, m Manifest) error {
	defer metrics.Time("store.snapshot.WriteManifest")()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		metrics.IncCounter("store.snapshot.WriteManifest.errors", 1)
		return err
	}
	return nil
}

// LoadManifest parses m from r. Returns [ErrManifestUnsupported]
// when the version is newer than this build.
func LoadManifest(r io.Reader) (Manifest, error) {
	defer metrics.Time("store.snapshot.LoadManifest")()
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		metrics.IncCounter("store.snapshot.LoadManifest.errors", 1)
		return Manifest{}, fmt.Errorf("%w: %w", ErrManifestCorrupted, err)
	}
	if m.Version > ManifestVersion {
		metrics.IncCounter("store.snapshot.LoadManifest.errors", 1)
		return Manifest{}, fmt.Errorf("%w: %d", ErrManifestUnsupported, m.Version)
	}
	return m, nil
}

// ReadManifestFile is a convenience wrapper around an O_NOFOLLOW open
// plus [LoadManifest]. The file is opened via [openSnapshotComponent] so
// a manifest.json that is a symlink in an untrusted snapshot directory is
// rejected rather than dereferenced.
func ReadManifestFile(path string) (Manifest, error) {
	defer metrics.Time("store.snapshot.ReadManifestFile")()
	f, err := openSnapshotComponent(path)
	if err != nil {
		metrics.IncCounter("store.snapshot.ReadManifestFile.errors", 1)
		return Manifest{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()
	m, err := LoadManifest(f)
	if err != nil {
		metrics.IncCounter("store.snapshot.ReadManifestFile.errors", 1)
	}
	return m, err
}
