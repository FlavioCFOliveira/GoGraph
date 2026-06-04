package snapshot

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// LoadedSnapshot is the result of [LoadSnapshotFull]: the parsed CSR
// arrays, the parsed labels readback (empty for v1 snapshots), the
// parsed properties readback (empty when properties.bin is absent),
// the parsed mapper readback (empty when mapper.bin is absent, e.g. a
// v1 CSR-only snapshot or a v2 snapshot written without a codec for a
// non-string key type), the optional per-index byte payloads (one
// entry per indexes/<name>.bin file referenced by the manifest), and
// the manifest that produced them.
//
// When mapper.bin is present, exactly one of [MapperReadback.Pairs]
// (version-1 string layout) and [MapperReadback.RawPairs] (version-2
// codec layout) is populated; see [MapperReadback].
//
// Each [IndexReadback].Bytes may be nil even when the manifest
// references the index — that signals the file was missing or its
// CRC32C did not validate. Callers must treat nil bytes as "rebuild
// from LPG" rather than as a fatal error; the corruption was already
// metered by [LoadIndexes] under `store.snapshot.indexes.corrupted`.
type LoadedSnapshot struct {
	Manifest   Manifest
	CSR        CSRReadback
	Labels     LabelsReadback
	Properties PropertiesReadback
	Mapper     MapperReadback
	Indexes    []IndexReadback
	// Tombstones is the node-removal set restored from tombstones.bin. It
	// is empty for snapshots that carry no tombstones.bin entry (older
	// snapshots, or any snapshot of a graph that never removed a node) —
	// the backward-compatibility contract.
	Tombstones TombstonesReadback
	// EdgeHandles is the per-handle edge metadata restored from
	// edgehandles.bin (each parallel edge's per-CREATE relationship type and
	// properties keyed by its stable handle). It is empty for snapshots that
	// carry no edgehandles.bin entry (older snapshots, or any snapshot of a
	// graph that never used the handle-keyed metadata stores) — the
	// backward-compatibility contract.
	EdgeHandles EdgeHandlesReadback
	// Constraints is the durable schema constraint set restored from
	// constraints.bin. It is empty for snapshots that carry no constraints.bin
	// entry (older snapshots, or any snapshot taken with no constraints
	// declared) — the backward-compatibility contract.
	Constraints ConstraintsReadback
}

// WriteSnapshotFull is the v2/v3 high-level helper: it lays out a
// snapshot directory containing csr.bin (legacy v1 component),
// labels.bin (v2 component), properties.bin (v2 component) and a
// manifest indexing them. When the underlying [graph.Mapper] is
// string-keyed (N=string) the writer additionally emits mapper.bin —
// the durable (NodeID -> natural key) interning table — and the
// manifest is stamped at [ManifestVersion] (v3). For any other N the
// writer falls back to the v2 layout (no mapper.bin) and the manifest
// records [manifestVersionV2]; recovery from a v2 snapshot continues
// to rely on WAL replay to re-intern keys.
//
// Atomic publication is achieved by assembling the snapshot under
// dir + ".tmp" and renaming it to dir on success — the same protocol
// used by [WriteSnapshotCSR].
//
// When g carries a non-nil [index.Manager] (set via
// [lpg.Graph.SetIndexManager]) with at least one registered index
// that implements [index.Serializer], an indexes/ sub-directory is
// also produced — one file per registered serializable index, each
// referenced from the manifest's Indexes field. Subscribers that do
// not implement [index.Serializer] are skipped (rebuild-on-restart).
//
// Callers that do not need durable LPG labels or properties can keep
// using [WriteSnapshotCSR]; it writes a v1-shaped directory that
// future readers (including this one) accept transparently.
func WriteSnapshotFull[N comparable, W any](dir string, c *csr.CSR[W], g *lpg.Graph[N, W]) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFull")()
	err := WriteSnapshotFullCtx(context.Background(), dir, c, g)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFull.errors", 1)
	}
	return err
}

// WriteSnapshotFullWithMapperCodec is the codec-aware variant of
// [WriteSnapshotFull]: it threads codec (the same [txn.Codec] the store
// uses to serialise node identifiers onto the WAL) into the mapper.bin
// writer so the durable NodeID->key interning table is emitted for ANY
// comparable key type N, not just string. A snapshot written this way
// is self-sufficient on load for every key type, which lets the
// checkpointer truncate the WAL instead of retaining it unboundedly
// (audit gap F3).
//
// For string-keyed graphs the mapper bytes remain byte-identical to the
// version-1 layout (see [WriteMapper]), so this entry point is a safe
// drop-in for the existing [WriteSnapshotFull] on string stores too.
//
// codec must not be nil; pass the store's [txn.Store.Codec].
func WriteSnapshotFullWithMapperCodec[N comparable, W any](
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	codec keyEncoder[N],
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullWithMapperCodec")()
	err := WriteSnapshotFullWithMapperCodecCtx(context.Background(), dir, c, g, codec)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodec.errors", 1)
	}
	return err
}

// WriteSnapshotFullCtx is the context-aware variant of
// [WriteSnapshotFull]. ctx.Err() is checked at five stage boundaries:
// before the CSR write, before the labels write, before the
// properties write, before the manifest write, and before the
// atomic rename. On cancellation the temporary staging directory is
// cleaned up and the wrapped ctx.Err is returned.
func WriteSnapshotFullCtx[N comparable, W any](
	ctx context.Context,
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullCtx")()
	// No codec: the mapper is persisted only for string-keyed graphs
	// (the historical v3 behaviour). writeMapperIfStringKeyed performs
	// the string-only probe.
	return writeSnapshotFullCore(ctx, dir, c, g, nil, func(c2 context.Context, tmp string) (int64, uint32, bool, error) {
		return writeMapperIfStringKeyed(c2, tmp, g)
	})
}

// WriteSnapshotFullWithMapperCodecCtx is the context-aware variant of
// [WriteSnapshotFullWithMapperCodec]. ctx cancellation is honoured at
// the same stage boundaries as [WriteSnapshotFullCtx]; the only
// difference is that the mapper.bin component is emitted for every key
// type via codec rather than for string alone.
func WriteSnapshotFullWithMapperCodecCtx[N comparable, W any](
	ctx context.Context,
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	codec keyEncoder[N],
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullWithMapperCodecCtx")()
	if codec == nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodecCtx.errors", 1)
		return errors.New("snapshot: nil mapper codec")
	}
	return writeSnapshotFullCore(ctx, dir, c, g, nil, func(c2 context.Context, tmp string) (int64, uint32, bool, error) {
		return writeMapperWithCodec(c2, tmp, g, codec)
	})
}

// WriteSnapshotFullWithConstraints is [WriteSnapshotFull] plus a durable
// constraints.bin component carrying the engine's schema constraint set. It is
// the snapshot entry point a checkpointer must use when the engine has
// constraints declared: without the component a checkpoint that truncated the
// WAL prefix which first declared a constraint would lose that constraint
// (a durability defect). The mapper.bin component is emitted for string-keyed
// graphs exactly as [WriteSnapshotFull] does.
//
// constraints may be nil or empty, in which case no constraints.bin is written
// and the output is byte-identical to [WriteSnapshotFull].
func WriteSnapshotFullWithConstraints[N comparable, W any](
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	constraints []ConstraintSpec,
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullWithConstraints")()
	err := writeSnapshotFullCore(context.Background(), dir, c, g, constraints,
		func(c2 context.Context, tmp string) (int64, uint32, bool, error) {
			return writeMapperIfStringKeyed(c2, tmp, g)
		})
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithConstraints.errors", 1)
	}
	return err
}

// WriteSnapshotFullWithMapperCodecAndConstraints is the codec-aware variant of
// [WriteSnapshotFullWithConstraints]: it threads codec into the mapper.bin
// writer (so the snapshot is self-sufficient for any key type) AND persists the
// constraint set. This is the entry point a checkpointer over a non-string
// store uses when constraints are declared.
//
// codec must not be nil. constraints may be nil or empty (no constraints.bin).
func WriteSnapshotFullWithMapperCodecAndConstraints[N comparable, W any](
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	codec keyEncoder[N],
	constraints []ConstraintSpec,
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullWithMapperCodecAndConstraints")()
	if codec == nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodecAndConstraints.errors", 1)
		return errors.New("snapshot: nil mapper codec")
	}
	err := writeSnapshotFullCore(context.Background(), dir, c, g, constraints,
		func(c2 context.Context, tmp string) (int64, uint32, bool, error) {
			return writeMapperWithCodec(c2, tmp, g, codec)
		})
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodecAndConstraints.errors", 1)
	}
	return err
}

// writeSnapshotFullCore is the shared implementation behind
// [WriteSnapshotFullCtx] and [WriteSnapshotFullWithMapperCodecCtx]. The
// only behaviour that varies between the two is how the mapper.bin
// component is produced, which the caller supplies as writeMapper:
// it writes mapper.bin under tmp and returns (size, crc, haveMapper,
// err). When haveMapper is false the snapshot is stamped v2 (no
// mapper.bin) exactly as before; when true it is stamped v3.
//
//nolint:gocyclo // snapshot publish: dir prep + CSR + labels + properties + mapper + constraints + manifest + atomic rename + ctx ticks
func writeSnapshotFullCore[N comparable, W any](
	ctx context.Context,
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	constraints []ConstraintSpec,
	writeMapper func(ctx context.Context, tmp string) (size int64, crc uint32, haveMapper bool, err error),
) error {
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// csr.bin
	csrPath := filepath.Join(tmp, CSRFile)
	csrSize, csrCRC, err := writeAndSync(csrPath, func(w io.Writer) (int64, uint32, error) {
		return WriteCSR(w, c)
	})
	if err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// labels.bin
	labelsPath := filepath.Join(tmp, LabelsFile)
	labelsSize, labelsCRC, err := writeAndSync(labelsPath, func(w io.Writer) (int64, uint32, error) {
		return WriteLabels(w, g)
	})
	if err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// properties.bin
	propertiesPath := filepath.Join(tmp, PropertiesFile)
	propsSize, propsCRC, err := writeAndSync(propertiesPath, func(w io.Writer) (int64, uint32, error) {
		return WriteProperties(w, g)
	})
	if err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// mapper.bin — durable (NodeID -> natural key) table. The supplied
	// writeMapper strategy decides whether it is emitted: the no-codec
	// caller ([WriteSnapshotFullCtx]) writes it only for string-keyed
	// graphs (the historical v3 behaviour); the codec-aware caller
	// ([WriteSnapshotFullWithMapperCodecCtx]) writes it for every key
	// type, making non-string snapshots self-sufficient too. When the
	// strategy returns haveMapper=false the snapshot stays v2 and
	// recovery rebuilds the mapper from the WAL — the documented v2
	// contract.
	mapperSize, mapperCRC, haveMapper, err := writeMapper(ctx, tmp)
	if err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// tombstones.bin — the durable node-removal set. Emitted ONLY when the
	// graph currently has at least one tombstoned node, so a snapshot of a
	// graph that never deleted a node is byte-identical to one produced
	// before this component existed (no behaviour change for the common
	// case). Without this component a committed node deletion would not
	// survive WAL truncation: the tombstone lives only in memory, and the
	// CSR/labels/properties writers treat a removed node as a live,
	// label-stripped one (the durability defect this fixes).
	var tombSize int64
	var tombCRC uint32
	haveTombstones := g.TombstoneCount() > 0
	if haveTombstones {
		tombstonesPath := filepath.Join(tmp, TombstonesFile)
		tombSize, tombCRC, err = writeAndSync(tombstonesPath, func(w io.Writer) (int64, uint32, error) {
			return WriteTombstones(w, g)
		})
		if err != nil {
			_ = os.RemoveAll(tmp)
			metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// edgehandles.bin — the durable per-handle edge metadata (each parallel
	// edge's per-CREATE relationship type and properties keyed by its stable
	// handle). Emitted ONLY when the graph carries at least one such record,
	// so a snapshot of a graph that never used the handle-keyed stores is
	// byte-identical to one produced before this component existed. csr.bin
	// already carries the handle COLUMN that re-stamps each recovered edge's
	// identity; this component restores the per-handle TYPE/PROPERTIES that
	// labels.bin / properties.bin deliberately collapse to a per-pair union.
	// Without it a self-sufficient snapshot would recover parallel edges with
	// the right handles but the wrong (unioned) per-edge type.
	var edgeHandleSize int64
	var edgeHandleCRC uint32
	var haveEdgeHandles bool
	edgeHandlesPath := filepath.Join(tmp, EdgeHandlesFile)
	edgeHandleSize, edgeHandleCRC, haveEdgeHandles, err = writeEdgeHandlesComponent(edgeHandlesPath, g)
	if err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// constraints.bin — the durable schema constraint set. Emitted ONLY when
	// the caller supplies at least one constraint, so a snapshot of a graph
	// with no constraints is byte-identical to one produced before this
	// component existed. Without this component a constraint declared and
	// committed before a checkpoint would not survive WAL truncation: the WAL
	// op that declared it lives only in the truncated prefix, so the
	// checkpoint must carry the constraint forward itself.
	var constraintsSize int64
	var constraintsCRC uint32
	haveConstraints := len(constraints) > 0
	if haveConstraints {
		constraintsPath := filepath.Join(tmp, ConstraintsFile)
		constraintsSize, constraintsCRC, err = writeAndSync(constraintsPath, func(w io.Writer) (int64, uint32, error) {
			return WriteConstraints(w, constraints)
		})
		if err != nil {
			_ = os.RemoveAll(tmp)
			metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// indexes/<name>.bin — one file per registered index that
	// implements [index.Serializer]. Subscribers without serializer
	// support are silently skipped (rebuild-on-restart contract).
	var idxEntries []IndexFileEntry
	if mgr := g.IndexManager(); mgr != nil && mgr.Count() > 0 {
		entries, ierr := WriteIndexes(tmp, mgr)
		if ierr != nil {
			_ = os.RemoveAll(tmp)
			metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
			return ierr
		}
		idxEntries = entries
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// Manifest version is v3 only when mapper.bin was emitted; non-
	// string-keyed graphs continue to produce v2 manifests so existing
	// recovery tests (which compare Manifest.Version against the
	// build's [ManifestVersion]) keep passing for every shape that
	// already worked before this change.
	manifestVersion := manifestVersionV2
	files := []FileEntry{
		{Name: CSRFile, Size: csrSize, CRC32C: csrCRC},
		{Name: LabelsFile, Size: labelsSize, CRC32C: labelsCRC},
		{Name: PropertiesFile, Size: propsSize, CRC32C: propsCRC},
	}
	if haveMapper {
		manifestVersion = ManifestVersion
		files = append(files, FileEntry{Name: MapperFile, Size: mapperSize, CRC32C: mapperCRC})
	}
	// The tombstones.bin entry is additive and does NOT change the manifest
	// version: it is an optional component (like indexes/<name>.bin),
	// present only when the graph has removed nodes. A reader that predates
	// it ignores the unknown file name; this reader restores the set.
	if haveTombstones {
		files = append(files, FileEntry{Name: TombstonesFile, Size: tombSize, CRC32C: tombCRC})
	}
	// The edgehandles.bin entry is likewise additive and does not change the
	// manifest version: present only when the graph carries per-handle edge
	// metadata. A reader that predates it ignores the unknown file name (and
	// reads parallel-edge types from the per-pair union); this reader restores
	// the per-handle types.
	if haveEdgeHandles {
		files = append(files, FileEntry{Name: EdgeHandlesFile, Size: edgeHandleSize, CRC32C: edgeHandleCRC})
	}
	// The constraints.bin entry is likewise additive and does not change the
	// manifest version: present only when constraints are declared. A reader
	// that predates it ignores the unknown file name; this reader restores the
	// set so a checkpoint + WAL truncate does not lose constraints.
	if haveConstraints {
		files = append(files, FileEntry{Name: ConstraintsFile, Size: constraintsSize, CRC32C: constraintsCRC})
	}

	// Persist the originating graph's directed/multigraph shape so
	// recovery reconstructs the same variant instead of hardcoding one.
	// The full writer always has the live graph in hand, so every NEW
	// full snapshot carries this; the legacy CSR-only writer cannot (it
	// has no graph) and omits it, falling back to the recovery default.
	cfg := g.Config()
	m := Manifest{
		Version:   manifestVersion,
		CreatedAt: time.Now().UTC(),
		Order:     c.Order(),
		Size:      c.Size(),
		Files:     files,
		Indexes:   idxEntries,
		GraphConfig: &GraphConfig{
			Directed:   cfg.Directed,
			Multigraph: cfg.Multigraph,
		},
	}

	manifestPath := filepath.Join(tmp, "manifest.json")
	mf, err := createSnapshotFile(manifestPath)
	if err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := WriteManifest(mf, m); err != nil {
		_ = mf.Close()        // best-effort: already on error path, WriteManifest err preserved
		_ = os.RemoveAll(tmp) // best-effort: tmp dir cleanup, WriteManifest err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := mf.Sync(); err != nil {
		_ = mf.Close()        // best-effort: already on error path, sync err preserved
		_ = os.RemoveAll(tmp) // best-effort: tmp dir cleanup, sync err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := mf.Close(); err != nil {
		_ = os.RemoveAll(tmp) // best-effort: tmp dir cleanup, close err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	// Make the staging directory's own inode durable BEFORE the publish
	// rename: each component file was fsync'd individually, but fsync(2)
	// on a file does not guarantee that the dirent linking it into the
	// staging directory is durable. On a filesystem that does not flush a
	// renamed directory's child dirents as part of the rename, a crash
	// after the rename (and after the checkpointer truncates the WAL)
	// could otherwise leave the published snapshot directory present but
	// its components missing or zero-length — total loss of every
	// transaction folded into the checkpoint. The canonical crash-safe
	// ordering is therefore: write+fsync components -> fsync staging dir
	// -> rename -> fsync parent. No-op on platforms without a directory
	// fsync primitive (Windows). See [dirFsync].
	if err := dirFsync(tmp); err != nil {
		_ = os.RemoveAll(tmp) // best-effort: staging cleanup, fsync err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return fmt.Errorf("snapshot: staging dir fsync: %w", err)
	}
	notePublishStep("staging-fsync", tmp)
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	notePublishStep("rename", tmp)
	if err := os.Rename(tmp, dir); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return fmt.Errorf("snapshot: publish rename: %w", err)
	}
	// Make the rename durable: fsync the parent directory so the
	// new directory entry survives a crash within the journal
	// writeback window. No-op on platforms that lack a directory
	// fsync primitive (Windows).
	if err := parentDirFsync(dir); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return fmt.Errorf("snapshot: publish parent fsync: %w", err)
	}
	return nil
}

// writeMapperIfStringKeyed inspects g's mapper and, when N=string,
// serialises it to mapper.bin under tmp. Returns (size, crc, true,
// nil) on success, (0, 0, false, nil) when N is not string (callers
// fall back to v2), or a non-nil error on a write failure (callers
// must clean tmp and surface the error).
//
// The function uses a type-switch on a sentinel pointer so the
// compiler can prove the conversion is well-typed without resorting
// to unsafe; it returns false for any N that is not the canonical
// string type. The fallback to v2 is documented at the writer
// godoc — non-string graphs keep producing the same on-disk layout as
// before this change.
func writeMapperIfStringKeyed[N comparable, W any](
	ctx context.Context,
	tmp string,
	g *lpg.Graph[N, W],
) (size int64, crc uint32, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, false, err
	}
	adj := g.AdjList()
	mapper := adj.Mapper()
	// Reflection-free probe: the writer only knows how to serialise
	// the string-keyed mapper. We tunnel the concrete pointer through
	// a type assertion on any() to avoid pulling in reflect for a one-
	// shot dispatch on the hot path.
	stringMapper, ok := any(mapper).(*graph.Mapper[string])
	if !ok {
		return 0, 0, false, nil
	}
	mapperPath := filepath.Join(tmp, MapperFile)
	mSize, mCRC, werr := writeAndSync(mapperPath, func(w io.Writer) (int64, uint32, error) {
		return WriteMapperString(w, stringMapper)
	})
	if werr != nil {
		return 0, 0, false, werr
	}
	return mSize, mCRC, true, nil
}

// writeMapperWithCodec serialises g's mapper to mapper.bin under tmp via
// codec, for ANY comparable key type N. It always emits the component
// (returning haveMapper=true on success), so the resulting snapshot is
// self-sufficient regardless of N. Returns a non-nil error on a write
// or encode failure (callers must clean tmp and surface the error).
func writeMapperWithCodec[N comparable, W any](
	ctx context.Context,
	tmp string,
	g *lpg.Graph[N, W],
	codec keyEncoder[N],
) (size int64, crc uint32, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, false, err
	}
	mapper := g.AdjList().Mapper()
	mapperPath := filepath.Join(tmp, MapperFile)
	mSize, mCRC, werr := writeAndSync(mapperPath, func(w io.Writer) (int64, uint32, error) {
		return WriteMapper(w, mapper, codec)
	})
	if werr != nil {
		return 0, 0, false, werr
	}
	return mSize, mCRC, true, nil
}

// writeEdgeHandlesComponent writes the optional edgehandles.bin component for
// g to path, returning (size, crc, emitted, err). When g carries no
// per-handle edge metadata, [WriteEdgeHandles] emits nothing; this helper then
// removes the (empty) file it created so the staging directory matches a
// graph that never used handles, and reports emitted=false so the caller omits
// the manifest entry. The byte-stable, version-tagged, CRC-covered shape
// mirrors the tombstones.bin component.
func writeEdgeHandlesComponent[N comparable, W any](path string, g *lpg.Graph[N, W]) (size int64, crc uint32, emitted bool, err error) {
	var produced bool
	size, crc, err = writeAndSync(path, func(w io.Writer) (int64, uint32, error) {
		s, c, e, werr := WriteEdgeHandles(w, g)
		if werr != nil {
			return 0, 0, werr
		}
		produced = e
		return s, c, nil
	})
	if err != nil {
		return 0, 0, false, err
	}
	if !produced {
		// Nothing to persist: drop the empty file so the snapshot omits the
		// component entirely (the absent-component backward-compat contract).
		_ = os.Remove(path) // best-effort: empty optional component cleanup
		return 0, 0, false, nil
	}
	return size, crc, true, nil
}

// writeAndSync creates path, hands the file handle to write, fsyncs
// and closes the file. It returns the (size, crc) tuple computed by
// write so the caller can record them in the manifest. The caller's
// path must reside under the staging .tmp directory; the function
// removes the file on any error (best effort) so a half-written
// component never lingers.
func writeAndSync(
	path string,
	write func(io.Writer) (int64, uint32, error),
) (size int64, crc uint32, err error) {
	f, err := createSnapshotFile(path)
	if err != nil {
		return 0, 0, err
	}
	size, crc, werr := write(f)
	if werr != nil {
		_ = f.Close()       // best-effort: already on error path, write err preserved
		_ = os.Remove(path) // best-effort: partial file cleanup, write err preserved
		return 0, 0, werr
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
	return size, crc, nil
}

// LoadSnapshotFull verifies and loads the snapshot rooted at dir,
// returning the CSR, the labels readback, and the properties
// readback. v1 snapshots are accepted transparently: their manifest
// has no labels.bin or properties.bin entry, and the returned
// [LoadedSnapshot.Labels] / [LoadedSnapshot.Properties] are zero
// values (empty tables, no records). v2 snapshots may carry any
// combination of labels.bin and properties.bin; each component is
// CRC-validated only when its manifest entry is present.
//
// CSR CRC verification mirrors [Open]; labels and properties CRC
// verification use the same TeeReader pattern so a corrupted
// component surfaces as [ErrCorrupted].
func LoadSnapshotFull(dir string) (LoadedSnapshot, error) {
	defer metrics.Time("store.snapshot.LoadSnapshotFull")()
	manifestPath := filepath.Join(dir, "manifest.json")
	m, err := ReadManifestFile(manifestPath)
	if err != nil {
		metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
		return LoadedSnapshot{}, err
	}

	csrEntry, labelsEntry, propsEntry, mapperEntry, tombEntry := findEntries(m.Files)
	if csrEntry == nil {
		metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
		return LoadedSnapshot{}, fmt.Errorf("%w: manifest missing %q", ErrCorrupted, CSRFile)
	}

	csrParsed, err := readVerifiedCSR(filepath.Join(dir, CSRFile), csrEntry.CRC32C, csrEntry.Size)
	if err != nil {
		metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
		return LoadedSnapshot{}, err
	}

	var labelsParsed LabelsReadback
	if labelsEntry != nil {
		labelsParsed, err = readVerifiedLabels(filepath.Join(dir, LabelsFile), labelsEntry.CRC32C)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	var propsParsed PropertiesReadback
	if propsEntry != nil {
		propsParsed, err = readVerifiedProperties(filepath.Join(dir, PropertiesFile), propsEntry.CRC32C)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	var mapperParsed MapperReadback
	if mapperEntry != nil {
		mapperPath := filepath.Join(dir, MapperFile)
		ver, verr := peekMapperVersion(mapperPath)
		if verr != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, verr
		}
		// Version 1 carries string keys (Pairs); version 2 carries
		// codec-encoded key bytes (RawPairs) that the recovery layer
		// decodes with the matching codec. Reading the version up front
		// lets a single LoadSnapshotFull serve both layouts without a
		// codec of its own.
		if ver == mapperFormatVersionCodec {
			mapperParsed, err = readVerifiedMapperBytes(mapperPath, mapperEntry.CRC32C)
		} else {
			mapperParsed, err = readVerifiedMapper(mapperPath, mapperEntry.CRC32C)
		}
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	// tombstones.bin — optional node-removal set. Absent for older
	// snapshots and for any graph that never deleted a node: the readback
	// stays empty (backward compatibility). When present its CRC32C is
	// verified exactly like the other components.
	var tombParsed TombstonesReadback
	if tombEntry != nil {
		tombParsed, err = readVerifiedTombstones(filepath.Join(dir, TombstonesFile), tombEntry.CRC32C)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	// edgehandles.bin — optional per-handle edge metadata. Absent for older
	// snapshots and for any graph that never used the handle-keyed stores:
	// the readback stays empty (backward compatibility). When present its
	// CRC32C is verified exactly like the other components.
	var edgeHandlesParsed EdgeHandlesReadback
	if ehEntry := findEntry(m.Files, EdgeHandlesFile); ehEntry != nil {
		edgeHandlesParsed, err = readVerifiedEdgeHandles(filepath.Join(dir, EdgeHandlesFile), ehEntry.CRC32C)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	// constraints.bin — optional schema constraint set. Absent for older
	// snapshots and for any snapshot taken with no constraints declared: the
	// readback stays empty (backward compatibility). When present its CRC32C
	// is verified exactly like the other components.
	var constraintsParsed ConstraintsReadback
	if ccEntry := findEntry(m.Files, ConstraintsFile); ccEntry != nil {
		constraintsParsed, err = readVerifiedConstraints(filepath.Join(dir, ConstraintsFile), ccEntry.CRC32C)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	// indexes/<name>.bin — best-effort load. Corruption surfaces as
	// nil Bytes on the IndexReadback so the recovery path can rebuild
	// from the LPG rather than aborting.
	var idxReadback []IndexReadback
	if len(m.Indexes) > 0 {
		idxReadback, err = LoadIndexes(dir, m.Indexes)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	return LoadedSnapshot{
		Manifest:    m,
		CSR:         csrParsed,
		Labels:      labelsParsed,
		Properties:  propsParsed,
		Mapper:      mapperParsed,
		Indexes:     idxReadback,
		Tombstones:  tombParsed,
		EdgeHandles: edgeHandlesParsed,
		Constraints: constraintsParsed,
	}, nil
}

// findEntry returns a pointer to the FileEntry named name, or nil when
// absent. Used for optional additive components (edgehandles.bin) that are
// not part of the fixed [findEntries] tuple.
func findEntry(files []FileEntry, name string) *FileEntry {
	for k := range files {
		if files[k].Name == name {
			return &files[k]
		}
	}
	return nil
}

// findEntries returns pointers to the csr.bin, labels.bin,
// properties.bin, mapper.bin and tombstones.bin entries in files, or nil
// for any that are absent. The slice is walked once and pointers index
// into the original storage so the caller can inspect them without
// copying.
func findEntries(files []FileEntry) (csrEntry, labelsEntry, propsEntry, mapperEntry, tombEntry *FileEntry) {
	for k := range files {
		switch files[k].Name {
		case CSRFile:
			csrEntry = &files[k]
		case LabelsFile:
			labelsEntry = &files[k]
		case PropertiesFile:
			propsEntry = &files[k]
		case MapperFile:
			mapperEntry = &files[k]
		case TombstonesFile:
			tombEntry = &files[k]
		}
	}
	return csrEntry, labelsEntry, propsEntry, mapperEntry, tombEntry
}

// readVerifiedCSR opens path, runs the file bytes through CRC32C and
// the structural CSR reader simultaneously, and returns the parsed
// snapshot iff the CRC matches expected. Any disagreement surfaces
// as [ErrCorrupted]. size is the manifest-recorded file size, passed as
// the precise remaining-bytes bound so a malicious header declaring more
// records than the file could hold is rejected before any allocation.
// The file is opened with O_NOFOLLOW (via openSnapshotComponent) so a
// component symlinked outside the snapshot dir is rejected.
func readVerifiedCSR(path string, expected uint32, size int64) (CSRReadback, error) {
	f, err := openSnapshotComponent(path)
	if err != nil {
		return CSRReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := readCSRLimited(tee, size)
	if err != nil {
		return CSRReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return CSRReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return CSRReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, CSRFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedLabels is the dual of [readVerifiedCSR] for labels.bin.
func readVerifiedLabels(path string, expected uint32) (LabelsReadback, error) {
	f, err := openSnapshotComponent(path)
	if err != nil {
		return LabelsReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadLabels(tee)
	if err != nil {
		return LabelsReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return LabelsReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return LabelsReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, LabelsFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedEdgeHandles is the dual of [readVerifiedCSR] for
// edgehandles.bin.
func readVerifiedEdgeHandles(path string, expected uint32) (EdgeHandlesReadback, error) {
	f, err := openSnapshotComponent(path)
	if err != nil {
		return EdgeHandlesReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadEdgeHandles(tee)
	if err != nil {
		return EdgeHandlesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return EdgeHandlesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return EdgeHandlesReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, EdgeHandlesFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedTombstones is the dual of [readVerifiedCSR] for
// tombstones.bin.
func readVerifiedTombstones(path string, expected uint32) (TombstonesReadback, error) {
	f, err := openSnapshotComponent(path)
	if err != nil {
		return TombstonesReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadTombstones(tee)
	if err != nil {
		return TombstonesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return TombstonesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return TombstonesReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, TombstonesFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedConstraints is the dual of [readVerifiedCSR] for
// constraints.bin.
func readVerifiedConstraints(path string, expected uint32) (ConstraintsReadback, error) {
	f, err := openSnapshotComponent(path)
	if err != nil {
		return ConstraintsReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadConstraints(tee)
	if err != nil {
		return ConstraintsReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return ConstraintsReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return ConstraintsReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, ConstraintsFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedProperties is the dual of [readVerifiedCSR] for
// properties.bin.
func readVerifiedProperties(path string, expected uint32) (PropertiesReadback, error) {
	f, err := openSnapshotComponent(path)
	if err != nil {
		return PropertiesReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadProperties(tee)
	if err != nil {
		return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return PropertiesReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, PropertiesFile, got, expected)
	}
	return parsed, nil
}
