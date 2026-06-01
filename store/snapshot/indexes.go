package snapshot

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gograph/graph/index"
	"gograph/internal/metrics"
)

// validateIndexName rejects any index name that, when joined with the
// snapshot's indexes/ directory, could escape that directory. Index
// names originate from the (attacker-controlled) manifest of an
// untrusted snapshot, so an entry such as "../../../../tmp/pwned" or an
// absolute path would otherwise let [LoadIndexes] read — and
// [WriteIndexes] write — an arbitrary file outside idxDir.
//
// A legitimate index name is a single, slash-free path element (the name
// it was registered under with [index.Manager.CreateIndex]). The guard
// therefore rejects "", ".", and "..", and accepts only names that:
//   - equal their own [filepath.Base] (no directory component),
//   - satisfy [fs.ValidPath] (rules out leading/trailing slashes and any
//     ".." element; note [fs.ValidPath] alone still accepts "."),
//   - contain no path separator ('/' or '\\', the latter for Windows),
//   - contain no ".." substring, and
//   - are not absolute.
//
// A rejection surfaces as [ErrManifestCorrupted] so callers classify it
// alongside any other manifest/disk disagreement.
func validateIndexName(name string) error {
	if name == "" ||
		name == "." ||
		name == ".." ||
		name != filepath.Base(name) ||
		!fs.ValidPath(name) ||
		strings.ContainsAny(name, `/\`) ||
		strings.Contains(name, "..") ||
		filepath.IsAbs(name) {
		return fmt.Errorf("%w: illegal index name %q", ErrManifestCorrupted, name)
	}
	return nil
}

// IndexesDir is the conventional sub-directory inside a v2 snapshot
// that holds one [index.Serializer]-encoded file per registered
// secondary index. The file name is <indexName>.bin; the manifest
// records the size and CRC32C of every entry under [Manifest.Indexes].
const IndexesDir = "indexes"

// IndexFileEntry pairs an index file's logical name (the name it was
// registered under with [index.Manager.CreateIndex]) with its
// on-disk size and CRC32C. It is the secondary-index analogue of
// [FileEntry] and travels in [Manifest.Indexes].
type IndexFileEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	CRC32C uint32 `json:"crc32c"`
}

// IndexReadback is the raw byte payload of one secondary index file
// returned by [LoadSnapshotFull]. The bytes are passed verbatim to
// [index.Serializer.Deserialize] by [store/recovery.Open*]; the
// snapshot loader does not interpret them further.
type IndexReadback struct {
	Name  string
	Bytes []byte
}

// WriteIndexes serialises every registered index in m to one file
// per index under dir/IndexesDir. Returns one [IndexFileEntry] per
// successfully serialised index, which the caller threads into the
// manifest. Subscribers that do not implement [index.Serializer] are
// silently skipped (rebuild-on-restart contract).
//
// On any I/O error the partial directory under dir/IndexesDir is
// removed (best effort) so the caller does not need to clean up.
//
//nolint:gocyclo // per-index write + per-index serialize + per-index sync
func WriteIndexes(dir string, m *index.Manager) ([]IndexFileEntry, error) {
	defer metrics.Time("store.snapshot.WriteIndexes")()
	if m == nil || m.Count() == 0 {
		return nil, nil
	}
	idxDir := filepath.Join(dir, IndexesDir)
	if err := os.MkdirAll(idxDir, 0o750); err != nil {
		metrics.IncCounter("store.snapshot.WriteIndexes.errors", 1)
		return nil, err
	}
	names := m.ListIndexes()
	out := make([]IndexFileEntry, 0, len(names))
	for _, name := range names {
		sub, err := m.GetIndex(name)
		if err != nil {
			// Race: the index was dropped between ListIndexes and
			// GetIndex. Skip silently — the manager is the source of
			// truth, and a dropped index has no on-disk state to keep.
			continue
		}
		ser, ok := sub.(index.Serializer)
		if !ok {
			// Subscriber does not implement Serializer; rebuild on
			// restart is acceptable per the index Manager contract.
			continue
		}
		// Defensive: index names from a live manager are trusted, but
		// validating here keeps the on-disk invariant — that every
		// indexes/<name>.bin stays inside idxDir — enforced at both the
		// write and the read boundary.
		if err := validateIndexName(name); err != nil {
			_ = os.RemoveAll(idxDir)
			metrics.IncCounter("store.snapshot.WriteIndexes.errors", 1)
			return nil, err
		}
		filename := filepath.Join(idxDir, name+".bin")
		size, crc, werr := writeAndSyncIndex(filename, ser)
		if werr != nil {
			_ = os.RemoveAll(idxDir)
			metrics.IncCounter("store.snapshot.WriteIndexes.errors", 1)
			return nil, fmt.Errorf("snapshot: index %q: %w", name, werr)
		}
		out = append(out, IndexFileEntry{Name: name, Size: size, CRC32C: crc})
	}
	metrics.IncCounter("store.snapshot.indexes.loaded", uint64(len(out)))
	return out, nil
}

// writeAndSyncIndex creates path, asks ser to populate it, fsyncs,
// closes, and returns the file size plus the CRC32C of the entire
// on-disk payload (including the magic header and the index's own
// internal CRC trailer). Mirroring [writeAndSync] used for the other
// snapshot components keeps the layout uniform.
func writeAndSyncIndex(path string, ser index.Serializer) (size int64, crc uint32, err error) {
	f, err := createSnapshotFile(path)
	if err != nil {
		return 0, 0, err
	}
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(f, hasher)
	if serr := ser.Serialize(tee); serr != nil {
		_ = f.Close()       // best-effort: already on error path, serialize err preserved
		_ = os.Remove(path) // best-effort: partial file cleanup, serialize err preserved
		return 0, 0, serr
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()       // best-effort: already on error path, stat err preserved
		_ = os.Remove(path) // best-effort: partial file cleanup, stat err preserved
		return 0, 0, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()       // best-effort: already on error path, sync err preserved
		_ = os.Remove(path) // best-effort: partial file cleanup, sync err preserved
		return 0, 0, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path) // best-effort: partial file cleanup, close err preserved
		return 0, 0, err
	}
	return st.Size(), hasher.Sum32(), nil
}

// LoadIndexes reads every entry in entries from dir/IndexesDir and
// returns the raw bytes for each. Files whose on-disk CRC32C does
// not match the manifest record surface a metric warning via
// `store.snapshot.indexes.corrupted` and are reported with
// [IndexReadback.Bytes] == nil; the caller treats nil bytes as
// "rebuild from LPG" rather than as a fatal error.
//
// A missing indexes/ directory is not an error: it simply means the
// snapshot does not carry persisted indexes (forward compat with
// snapshots produced before this format extension).
func LoadIndexes(dir string, entries []IndexFileEntry) ([]IndexReadback, error) {
	defer metrics.Time("store.snapshot.LoadIndexes")()
	if len(entries) == 0 {
		return nil, nil
	}
	idxDir := filepath.Join(dir, IndexesDir)
	if _, err := os.Stat(idxDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Manifest references indexes but the directory is gone.
			// Treat every entry as corrupted (rebuild path).
			out := make([]IndexReadback, 0, len(entries))
			for _, e := range entries {
				metrics.IncCounter("store.snapshot.indexes.corrupted", 1)
				out = append(out, IndexReadback{Name: e.Name})
			}
			return out, nil
		}
		metrics.IncCounter("store.snapshot.LoadIndexes.errors", 1)
		return nil, err
	}
	out := make([]IndexReadback, 0, len(entries))
	for _, e := range entries {
		// e.Name comes from the (attacker-controlled) manifest. A name
		// that escapes idxDir is a security event, not benign corruption,
		// so fail-stop with a typed error rather than degrading to the
		// rebuild path — that guarantees we read NOTHING outside idxDir.
		if err := validateIndexName(e.Name); err != nil {
			metrics.IncCounter("store.snapshot.LoadIndexes.errors", 1)
			return nil, err
		}
		filename := filepath.Join(idxDir, e.Name+".bin")
		buf, err := readIndexFile(filename)
		if err != nil {
			metrics.IncCounter("store.snapshot.indexes.corrupted", 1)
			out = append(out, IndexReadback{Name: e.Name})
			continue
		}
		if got := crc32.Checksum(buf, castagnoli); got != e.CRC32C {
			metrics.IncCounter("store.snapshot.indexes.corrupted", 1)
			out = append(out, IndexReadback{Name: e.Name})
			continue
		}
		out = append(out, IndexReadback{Name: e.Name, Bytes: buf})
	}
	return out, nil
}

// readIndexFile reads an index component file in full, opening it via
// [openSnapshotComponent] so an indexes/<name>.bin that is a symlink in
// an untrusted snapshot directory is rejected (O_NOFOLLOW) rather than
// dereferenced. It is the symlink-safe replacement for os.ReadFile on the
// snapshot read path.
func readIndexFile(path string) ([]byte, error) {
	f, err := openSnapshotComponent(path)
	if err != nil {
		return nil, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}
