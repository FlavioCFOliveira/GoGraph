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
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
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

// ErrManifestTooLarge is returned by [LoadManifest] — and so by the file-backed
// readers [ReadManifestFile] / [ReadManifestFileFS] above it — when a
// manifest.json exceeds [DefaultMaxManifestBytes]. Inspect with [errors.Is]. It
// bounds the transient allocation an attacker-supplied or corrupt snapshot
// directory can force at store-open time, mirroring the DefaultMaxBytes ceiling
// every sibling loader (csv/jsonl/graphml, the WAL frame decoder, the csrfile
// loader) already applies.
//
// The ceiling bounds the BYTES READ, not merely the bytes the JSON decoder
// consumes. It bounded only consumption before the checksum trailer existed,
// because the decoder stops at the closing brace and so never read trailing
// padding — which meant a manifest.json of any length on disk was accepted.
// Verifying a trailer requires reading to the end of the file, so the ceiling
// now covers the file, which is both the stricter reading and the one the stated
// purpose implies.
var ErrManifestTooLarge = errors.New("snapshot: manifest exceeds maximum size")

// DefaultMaxManifestBytes is the upper bound the file-backed manifest readers
// impose on a manifest.json. A manifest is a small JSON document — a version
// header plus one FileEntry/IndexFileEntry per snapshot component — so even a
// graph with thousands of on-disk index files stays in the low single-digit
// MiB. 32 MiB is far above any legitimate manifest yet stops a hostile or
// corrupt manifest.json (a giant array or string field) from driving a
// multi-gigabyte transient decode allocation at recovery before any version or
// CRC check bounds it.
const DefaultMaxManifestBytes = 32 << 20 // 32 MiB

// manifestLimitReader wraps an [io.Reader] and returns [ErrManifestTooLarge]
// once total consumption would exceed maxBytes. Unlike [io.LimitReader] — which
// reports a clean EOF at the limit and would surface as a truncation-induced
// JSON parse error — it fails with a distinct typed error so the fail-stop is
// non-silent. It mirrors graph/io/csv.limitReader.
type manifestLimitReader struct {
	r         io.Reader
	remaining int64
}

// Read implements [io.Reader]. It never returns more than remaining bytes and
// fails with [ErrManifestTooLarge] the moment the underlying reader would push
// total consumption past the configured ceiling.
func (l *manifestLimitReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, ErrManifestTooLarge
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	return n, err
}

// ─────────────────────────────────────────────────────────────────────────────
// manifest integrity: the CRC32C trailer
// ─────────────────────────────────────────────────────────────────────────────

// IntegrityCRC32CTrailer is the value [Manifest.Integrity] carries when the
// writer framed the manifest with a checksum trailer. It is the only integrity
// scheme this build emits; see [Manifest.Integrity] for why the field exists at
// all when the trailer is self-identifying.
const IntegrityCRC32CTrailer = "crc32c-trailer"

// manifestTrailerSize is the on-disk width of the trailer [WriteManifest]
// appends after the JSON value:
//
//	offset  width  field
//	     0      8  magic     manifestTrailerMagic
//	     8      4  algorithm uint32 LE, manifestTrailerAlgoCRC32C
//	    12      4  checksum  uint32 LE, CRC32C over every byte before the trailer
//
// The checksum covers the payload — the whole JSON document, every key name and
// every value, plus any whitespace between the closing brace and the trailer —
// and NOT the trailer itself, so it is never self-referential. A checksum stored
// as a field INSIDE the JSON would have to be excluded from its own computation,
// which leaves its own key name unprotected and reopens exactly the hole this
// trailer closes.
const manifestTrailerSize = 16

// manifestTrailerMagic identifies the trailer. It opens with a NUL so it can
// never be mistaken for JSON whitespace by [splitManifestTrailer], which is what
// makes "there is no trailer" a decidable state rather than a guess.
var manifestTrailerMagic = [8]byte{0x00, 'G', 'G', 'M', 'A', 'N', 'I', 'F'}

// manifestTrailerAlgoCRC32C is the algorithm identifier for CRC32C
// (Castagnoli), the checksum every other component in the directory already
// uses. Recording it explicitly means a future scheme can be introduced without
// a second magic, and an unrecognised value is a refusal rather than a silent
// skip.
const manifestTrailerAlgoCRC32C uint32 = 1

// appendManifestTrailer returns payload followed by its trailer. payload must be
// the complete JSON document; the checksum is computed over all of it.
func appendManifestTrailer(payload []byte) []byte {
	var tr [manifestTrailerSize]byte
	copy(tr[0:8], manifestTrailerMagic[:])
	binary.LittleEndian.PutUint32(tr[8:12], manifestTrailerAlgoCRC32C)
	binary.LittleEndian.PutUint32(tr[12:16], crc32.Checksum(payload, castagnoli))
	return append(payload, tr[:]...)
}

// splitManifestTrailer reports whether data carries a trailer region and, when
// it does, verifies it.
//
// end is the offset one past the JSON value, as reported by
// [json.Decoder.InputOffset]. The bytes after it are the trailer region.
//
// The adjudication is deliberately asymmetric, and that asymmetry is the whole
// mechanism:
//
//   - An EMPTY region (nothing but ASCII whitespace — every manifest ends with
//     the newline [json.Encoder.Encode] appends) means the file was written by a
//     build that predates the trailer. It is accepted, unverified.
//   - A NON-EMPTY region MUST be a well-formed, verifying trailer. There is no
//     third outcome.
//
// Without that rule a single byte flipped in the magic would degrade a protected
// manifest to "legacy, accepted unverified" — the same silent acceptance the
// trailer exists to remove, merely relocated into the trailer. With it, every
// byte of a trailer-bearing manifest is adjudicated: a flip in the payload fails
// the checksum, a flip in the magic or the algorithm identifier fails the shape,
// and a flip in the checksum fails the comparison.
func splitManifestTrailer(data []byte, end int64) (verified bool, err error) {
	if end < 0 || end > int64(len(data)) {
		// Defensive: InputOffset is always within the buffer it decoded.
		return false, fmt.Errorf("%w: JSON value ends at %d of %d bytes", ErrManifestCorrupted, end, len(data))
	}
	if len(bytes.TrimSpace(data[end:])) == 0 {
		return false, nil
	}
	if len(data) < manifestTrailerSize {
		return false, fmt.Errorf("%w: trailing bytes are too short to be a manifest trailer (%d bytes)",
			ErrManifestCorrupted, len(data))
	}
	tr := data[len(data)-manifestTrailerSize:]
	if !bytes.Equal(tr[0:8], manifestTrailerMagic[:]) {
		return false, fmt.Errorf("%w: manifest trailer magic %x, want %x",
			ErrManifestCorrupted, tr[0:8], manifestTrailerMagic)
	}
	if algo := binary.LittleEndian.Uint32(tr[8:12]); algo != manifestTrailerAlgoCRC32C {
		return false, fmt.Errorf("%w: manifest trailer checksum algorithm %d unrecognised", ErrManifestCorrupted, algo)
	}
	payload := data[:len(data)-manifestTrailerSize]
	got := crc32.Checksum(payload, castagnoli)
	if want := binary.LittleEndian.Uint32(tr[12:16]); got != want {
		return false, fmt.Errorf("%w: manifest.json crc32c=%d want=%d", ErrManifestCorrupted, got, want)
	}
	return true, nil
}

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
// # Integrity, and why it does not fight forward compatibility
//
// The manifest is read FIRST, before any other component can be trusted: it
// carries the CRC32C of every component file, so a manifest that is itself wrong
// makes every downstream check meaningless. Integrity is therefore enforced at
// the FILE FRAMING layer and never at the JSON SCHEMA layer, and keeping those
// two layers apart is what lets both properties hold at once:
//
//   - The FRAMING layer is closed. [WriteManifest] appends a fixed 16-byte
//     trailer holding a CRC32C over the entire JSON document — every key name,
//     every value, every byte of indentation. [LoadManifest] verifies it before
//     it looks at a single field. Nothing in the JSON is trusted to decide
//     whether the JSON is intact.
//   - The SCHEMA layer stays open. Unknown fields are still ignored, and an
//     absent field still decodes to its zero value — the "absent means no value"
//     policy [Manifest.CommitTS] documents is unchanged.
//
// A required-field check would have pulled against that policy, because it makes
// absence itself an error and so forbids the very evolution the policy grants.
// The trailer does not, because it never asks what the fields MEAN. It only
// establishes that the bytes are the bytes the writer produced. Once that holds,
// "absent" can only mean the writer genuinely omitted the field — never that
// corruption renamed its key — so the two properties stop being in tension and
// become complementary: the framing makes the schema's permissiveness SAFE.
//
// The trailer costs no compatibility in either direction:
//
//   - Older snapshot, newer reader. A manifest written before the trailer
//     existed has no trailer region, is accepted unverified, and reports
//     [Manifest.IntegrityVerified] false. No version step, no migration; the
//     frozen v1 fixture in testdata still loads byte-for-byte unchanged.
//   - Newer snapshot, older reader. [json.Decoder] stops at the end of the first
//     complete value, so a build that predates the trailer never reads past the
//     closing brace and is unaffected by what follows it. [ManifestVersion] is
//     therefore deliberately NOT bumped: the trailer is a framing change, not a
//     schema change, and bumping the schema version would make older readers
//     reject the file with [ErrManifestUnsupported] for no reason.
//
// # Prior art
//
// The layering above is RocksDB's MANIFEST, generalised. RocksDB is the one
// engine surveyed that gets BOTH properties, and it gets them exactly this way:
// a VersionEdit is a tag-length-value document whose unknown tags are skipped
// (kTagSafeIgnoreMask, db/version_edit.h — its own comment calls these "forward
// compatible (aka ignorable) records"), while the CRC32c that protects it lives
// in the log-record framing OUTSIDE the payload (db/log_format.h: "Header is
// checksum (4 bytes), length (2 bytes), type (1 byte)"), so a reader that skips
// a field it does not understand still validates every byte of it.
//
// The trailer shape is Lucene's. CodecUtil.writeFooter appends a fixed 16-byte
// footer — magic, algorithm identifier, checksum — to every file including
// segments_N, and CodecUtil.footerLength() is a compile-time constant. That
// constancy is what lets checkFooter detect a file that was EXTENDED as well as
// one that was truncated, which is why manifestTrailerSize is a constant here
// too. Lucene's CRC additionally covers its own magic and algorithm identifier;
// this trailer instead adjudicates those two fields by shape, which detects the
// same flips (proven byte-by-byte in manifest_integrity_test.go) and reports
// them more precisely.
//
// Two negative results shaped the decision as much as the positive ones. No
// engine surveyed puts a checksum in a NAMED FIELD inside a variable-shaped,
// self-describing serialization and obtains full byte integrity from it: etcd
// comes closest and its walpb.Record.Crc covers only the opaque Data blob, not
// the record type and not the protobuf tags. And RocksDB's own backup metadata
// file — a text manifest with genuine ignore-unknown-field machinery — reserves
// a "// FOOTER" section documented for "a checksum of the meta file" that is
// emitted only under test options; the slot was designed and never filled, so
// in production that manifest has no integrity over itself. Both are the shape
// this manifest was in before the trailer.
//
// PostgreSQL's pg_control is the closest fixed-shape analogue (crc is the last
// struct field, covering offsetof(ControlFileData, crc)) and it deliberately
// checks the VERSION before the CRC, because the number of bytes to checksum
// depends on the struct layout. The order is reversed here on purpose: this
// checksum covers the file minus a constant, so it does not need the schema to
// be interpreted first, and the stronger ordering — bytes before fields — is
// available.
//
// # Scope of the guarantee
//
// CRC32C detects accidental corruption — a flipped bit, a torn or partially
// rewritten block, a bad cable. It is NOT a message authentication code: an
// attacker who can rewrite manifest.json can also recompute the trailer. The
// defence against a hostile store directory remains the surrounding controls
// (O_NOFOLLOW component opens, [DefaultMaxManifestBytes], the per-component
// allocation bounds), not this checksum.
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
	CreatedAt   time.Time        `json:"created_at"`
	GraphConfig *GraphConfig     `json:"graph_config,omitempty"`
	Files       []FileEntry      `json:"files"`
	Indexes     []IndexFileEntry `json:"indexes,omitempty"`
	Version     int              `json:"version"`
	Order       uint64           `json:"order"`
	Size        uint64           `json:"size"`
	// CommitTS is the MVCC instant the image was captured at, or 0 / absent when
	// the originating graph had no MVCC clock or the writer had no graph in hand
	// (the legacy CSR-only writer, which omits GraphConfig for the same reason).
	//
	// Recovery folds it into the derived clock floor, so a reopened graph never
	// re-mints an instant the image already contains (rmp #2309, MVCC C3d). It is
	// the quantity Memgraph reads back as info.start_timestamp.
	//
	// NO MANIFEST VERSION BUMP. The manifest is JSON, so an older reader ignores an
	// unknown field and a newer reader on an older manifest decodes the zero value —
	// which is exactly the "absent means no timestamp" policy the OpCommit body
	// uses. `omitempty` keeps a timestamp-less manifest byte-identical to what
	// previous builds wrote, so no fixture or golden file moves.
	//
	// Corruption can no longer zero this field silently: it lies inside the
	// region the trailer checksums, so a flip in the `commit_ts` KEY — which
	// leaves valid JSON whose renamed key encoding/json would drop, decoding the
	// timestamp as 0 and skipping RestoreMVCCClock — now fails the manifest
	// checksum and fail-stops recovery. See the integrity section above.
	CommitTS uint64 `json:"commit_ts,omitempty"`

	// Integrity names the framing scheme the writer used, or is empty for a
	// manifest written before the trailer existed. The current writer always sets
	// [IntegrityCRC32CTrailer]; `omitempty` keeps a legacy manifest — and the
	// frozen v1 fixture — byte-identical to what previous builds wrote.
	//
	// The trailer identifies itself, so this field is not how a reader FINDS it.
	// It exists for the one case the trailer cannot speak for: a manifest whose
	// trailer has been lost entirely (a zeroed tail block, a truncating copy).
	// Without it that file is indistinguishable from a legacy manifest and would
	// be accepted unverified; with it, a manifest that says it was framed and
	// arrives unframed is refused. Losing the protection therefore requires TWO
	// independent damages — the trailer AND this marker — rather than one.
	//
	// Whenever a trailer is present this field sits inside the checksummed region
	// like any other, so reading it is an assertion, never a trust decision.
	Integrity string `json:"integrity,omitempty"`

	// IntegrityVerified reports that [LoadManifest] verified a checksum trailer
	// over these bytes. It is a decode-time result, never serialised and never
	// read from the file (`json:"-"`), so a corrupt manifest cannot assert its
	// own soundness. False means the manifest predates the trailer and was
	// accepted on its JSON syntax alone.
	IntegrityVerified bool `json:"-"`
}

// WriteManifest writes m to w in canonical (pretty-printed) JSON followed by a
// CRC32C trailer over the whole document (see the integrity section on
// [Manifest]). It sets [Manifest.Integrity] on its own copy of m, so callers
// need not — and must not rely on the value they passed in surviving.
//
// The document is buffered rather than streamed because the checksum must be
// computed over the finished bytes; it is also what makes the file reach w in a
// single Write.
//
//nolint:gocritic // public API: Manifest is passed by value to preserve the existing call sites; the encoder only reads from it.
func WriteManifest(w io.Writer, m Manifest) error {
	defer metrics.Time("store.snapshot.WriteManifest").Stop()
	m.Integrity = IntegrityCRC32CTrailer
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		metrics.IncCounter("store.snapshot.WriteManifest.errors", 1)
		return err
	}
	if _, err := w.Write(appendManifestTrailer(buf.Bytes())); err != nil {
		metrics.IncCounter("store.snapshot.WriteManifest.errors", 1)
		return err
	}
	return nil
}

// LoadManifest parses a manifest from r.
//
// The order of the checks is load-bearing: the bytes are bounded, then their
// framing is verified, and only then is any FIELD consulted. A manifest is the
// first thing a store open reads, so no decision may rest on a field whose bytes
// have not yet been established — the version check included.
//
// Returns [ErrManifestTooLarge] above [DefaultMaxManifestBytes],
// [ErrManifestCorrupted] when the JSON does not parse or the trailer does not
// verify, and [ErrManifestUnsupported] when the version is newer than this
// build.
func LoadManifest(r io.Reader) (Manifest, error) {
	defer metrics.Time("store.snapshot.LoadManifest").Stop()
	// Bound the read here rather than at the call site: the trailer lives at the
	// END of the file, so verifying it requires the whole document in hand, and a
	// public streaming entry point must not become an unbounded read because of
	// it. Callers that already wrap r in a manifestLimitReader simply apply the
	// same ceiling twice, which is harmless.
	data, err := io.ReadAll(&manifestLimitReader{r: r, remaining: DefaultMaxManifestBytes})
	if err != nil {
		metrics.IncCounter("store.snapshot.LoadManifest.errors", 1)
		if errors.Is(err, ErrManifestTooLarge) {
			return Manifest{}, err
		}
		return Manifest{}, fmt.Errorf("%w: %w", ErrManifestCorrupted, err)
	}

	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&m); err != nil {
		metrics.IncCounter("store.snapshot.LoadManifest.errors", 1)
		return Manifest{}, fmt.Errorf("%w: %w", ErrManifestCorrupted, err)
	}

	verified, err := splitManifestTrailer(data, dec.InputOffset())
	if err != nil {
		metrics.IncCounter("store.snapshot.LoadManifest.errors", 1)
		return Manifest{}, err
	}
	switch {
	case verified:
		// The bytes are established. Everything below reads authenticated fields.
		if m.Integrity != IntegrityCRC32CTrailer {
			metrics.IncCounter("store.snapshot.LoadManifest.errors", 1)
			return Manifest{}, fmt.Errorf("%w: manifest carries a verified crc32c trailer but declares integrity %q",
				ErrManifestCorrupted, m.Integrity)
		}
		m.IntegrityVerified = true
	case m.Integrity != "":
		// It says it was framed and it arrived unframed: the trailer was lost.
		// Accepting this as "legacy" is exactly the silent acceptance the trailer
		// removes, so it is a refusal.
		metrics.IncCounter("store.snapshot.LoadManifest.errors", 1)
		return Manifest{}, fmt.Errorf("%w: manifest declares integrity %q but carries no trailer",
			ErrManifestCorrupted, m.Integrity)
	default:
		// A manifest written before the trailer existed. Accepted unverified so
		// existing snapshot directories keep opening; counted so the exposure is
		// observable rather than assumed.
		metrics.IncCounter("store.snapshot.LoadManifest.unverified", 1)
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
	return readManifestFileWith(osBackend{}, path)
}

// ReadManifestFileFS is the filesystem-seam variant of [ReadManifestFile]:
// it opens the manifest through fsys instead of the default OS backend. It
// is the entry point the deterministic-simulation harness (internal/sim)
// uses to read a manifest backed by an in-memory disk. Passing osBackend{}
// reproduces [ReadManifestFile] exactly.
func ReadManifestFileFS(fsys fileSystem, path string) (Manifest, error) {
	return readManifestFileWith(fsys, path)
}

// readManifestFileWith is the seam-threaded implementation behind
// [ReadManifestFile] and [ReadManifestFileFS]: the manifest open routes
// through fsys, so the OS backend (which calls [openSnapshotComponent] with
// its O_NOFOLLOW guard) reproduces the historical behaviour exactly.
func readManifestFileWith(fsys fileSystem, path string) (Manifest, error) {
	defer metrics.Time("store.snapshot.ReadManifestFile").Stop()
	f, err := fsys.OpenComponent(path)
	if err != nil {
		metrics.IncCounter("store.snapshot.ReadManifestFile.errors", 1)
		return Manifest{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()
	// Bound the read: manifest.json is an untrusted store file (an attacker or
	// corruption controls the whole snapshot directory), and the decode grows
	// slices/strings proportionally to the input, so a giant manifest would drive
	// a multi-gigabyte transient allocation at open before any version or CRC
	// check runs. Cap it with DefaultMaxManifestBytes, mirroring the
	// DefaultMaxBytes ceiling every sibling loader applies. A manifest above the
	// ceiling fails with ErrManifestTooLarge (non-silent).
	//
	// LoadManifest applies the same ceiling internally, so this wrap is redundant
	// for the OS path; it is kept because it is the ceiling this seam promises
	// regardless of what the decoder above it does.
	lr := &manifestLimitReader{r: f, remaining: DefaultMaxManifestBytes}
	m, err := LoadManifest(lr)
	if err != nil {
		metrics.IncCounter("store.snapshot.ReadManifestFile.errors", 1)
	}
	return m, err
}
