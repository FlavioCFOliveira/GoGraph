package snapshot

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// component is one fully-serialised snapshot component held in memory: the
// exact bytes the streaming writer emitted, together with the (size, CRC32C)
// pair that writer computed over them. Holding the writer's own tuple — rather
// than re-deriving it at publish time — makes a captured snapshot byte-identical
// to one written straight to disk by construction, not by inspection.
//
// present distinguishes "this optional component was not emitted" (tombstones
// for a graph that never deleted a node, mapper for a non-string key type with
// no codec) from "emitted and empty".
type component struct {
	bytes   []byte
	size    int64
	crc     uint32
	present bool
}

// ErrCaptureNotQuiesced is returned by [CaptureGraph] when the instant it was given
// was opened while a write transaction was still open, so the image it would produce
// cannot be loaded back.
//
// # The precondition
//
// A NodeID is packed as (intra << shardBits) | shard and intra is assigned when the
// key is INTERNED, not when the transaction commits. [graph.Mapper.LoadFrom] — the
// function recovery seeds the interning table with — requires the intra indexes it
// receives to form the contiguous sequence 0..N-1 within each shard, and rejects the
// whole snapshot otherwise.
//
// A capture at an instant must drop the ids that instant cannot see. That is safe
// only while the dropped ids form a per-shard SUFFIX, which holds exactly when every
// interned id belongs to an already-committed transaction — because then the only
// invisible ids are those interned after the instant, and interning is monotone
// within a shard. An id interned by a transaction that is still open breaks it: the
// id sits below ids that later transactions have already interned and committed, so
// dropping it leaves a hole in the middle.
//
// # Why this fails rather than compensating
//
// There is no correct image to produce. Including the open transaction's node would
// put a node in the image that did not exist at the instant. Excluding every id above
// the hole would drop nodes that were COMMITTED before the instant — and the
// checkpointer is about to truncate the WAL prefix that holds them, so those commits
// would be lost outright. Fail-stop is the only sound answer, and a checkpoint that
// returns an error has published nothing and truncated nothing.
//
// # How the checkpointer satisfies it
//
// It opens the instant inside the commit serialiser, which closes writer admission
// and drains the admitted writers to zero before running. A writer's registration
// spans its whole commit, so when the drain completes there is no open transaction
// and therefore no interned-but-uncommitted id. The same drain is what makes the
// durable-offset watermark and the instant describe one transaction boundary.
var ErrCaptureNotQuiesced = errors.New("snapshot: capture instant taken while a write transaction was open")

// capturedIndex is one serialisable secondary index's payload, captured with
// the same by-value discipline as [component].
type capturedIndex struct {
	name string
	comp component
}

// Capture is a point-in-time, fully-serialised image of EVERY snapshot
// component that derives from the live graph: csr.bin, labels.bin,
// properties.bin, mapper.bin, tombstones.bin, edgehandles.bin, the
// indexes/<name>.bin payloads, and the graph's directed/multigraph/weightless
// shape.
//
// # Why this type exists (ACID Atomicity, rmp #2269)
//
// A checkpoint is an OBSERVER of the graph, and the file it publishes is what a
// crash recovery replays. The snapshot must therefore be an image of ONE
// transaction-boundary state — never a mixture of two.
//
// Before this type, the checkpointer captured only the CSR adjacency under the
// commit lock and then handed the LIVE graph to the writer, which walked it for
// the mapper, labels, properties, tombstones, edge handles and index payloads
// during the deliberately lock-free snapshot write. Those walks therefore
// observed a LATER state than the CSR. For a workload of
// `CREATE (a)-[:R]->(b)` transactions the published snapshot carried the node
// pairs of transactions that committed DURING the write while carrying none of
// their edges, so a snapshot-only recovery reconstructed a graph with
// Order > 2*Size — an artefact no serial schedule could produce, and a partial
// transaction made durable. The same skew could publish a node's labels or
// properties from after a mutation whose edge changes were not captured, and
// (worse) could capture the eagerly-applied writes of a transaction whose WAL
// commit had not yet succeeded and might still be rolled back.
//
// The invariant this type enforces is therefore: every byte of a snapshot comes
// from the same instant. The publish step ([WriteCapture]) touches no graph at all,
// so publishing stays lock-free because what phase 2 writes is bytes, not a graph.
//
// # How the one instant is obtained (rmp #2310)
//
// Originally by EXCLUSION: the caller took the Capture inside its own window, which
// for the checkpointer meant the store's commit serialisation plus the old
// lpg.Graph.View,
// held for the whole serialisation. That made the stall proportional to the graph,
// and it was the last place in the module where a reader excluded writers.
//
// Now by VERSIONING: the caller opens an MVCC snapshot and passes it as `at`, every
// component resolves through it, and writers commit throughout. Exclusion is still
// available for callers that want the present (`at == nil`) and is what the offline
// one-shot writer uses. See [CaptureGraph] for the obligations each mode carries and
// [ErrCaptureNotQuiesced] for the one precondition the versioned mode keeps.
//
// Concurrency: a Capture is an immutable value once returned; it shares no
// state with the graph it was taken from and is safe to publish from another
// goroutine.
//
// Cost: a Capture holds the whole serialised snapshot in memory until it is
// published. That is the price of an atomic image; see [CaptureGraph].
type Capture[W any] struct {
	csr *csr.CSR[W]
	// wenc, when non-nil, is the weight codec [WriteCapture] serialises the
	// CSR's weights column through for weight types the fixed-width layout
	// cannot size (rmp #2526). Nil means the fixed-width layout is the only one
	// available, and publishing a capture whose weights carry information it
	// cannot express fails with [ErrWeightNotPersistable] rather than silently
	// publishing a weightless image.
	wenc weightEncoder[W]
	// order is how many nodes the image carries — the count admitted by the same
	// instant filter the mapper used. orderKnown distinguishes "not computed" (the
	// present-time capture, which falls back to the CSR) from a genuine zero.
	order       uint64
	orderKnown  bool
	labels      component
	properties  component
	mapper      component
	tombstones  component
	edgeHandles component
	indexes     []capturedIndex
	config      GraphConfig
	// commitTS is the MVCC instant this image was taken at, or 0 for a graph with
	// no MVCC clock (rmp #2309, MVCC C3d).
	//
	// # Why a snapshot has to name its instant
	//
	// Recovery DERIVES the MVCC clock rather than reading a persisted counter, by
	// folding a maximum over the commit timestamps it can see. A snapshot TRUNCATES
	// the WAL prefix, so after a checkpoint the instants of everything it folded are
	// no longer anywhere in the log — and a derivation that only reads the WAL would
	// restore a clock far below what the snapshot already contains, then re-mint
	// instants that are durably in the image.
	//
	// That is not a hypothetical gap: rmp #2309's C3c measured that WAL-only replay
	// overshoots the durable maximum on its own (an instant per op against a maximum
	// counting transactions), so the WAL half of the derivation is unobservable and
	// the SNAPSHOT half is the one that actually carries it in a checkpointed
	// directory — which is the normal production shape.
	//
	// It is the same quantity Memgraph reads back as info.start_timestamp from its
	// snapshot, and restores as timestamp_ = max(timestamp_, next_timestamp).
	commitTS uint64
}

// Order reports how many NODES this image carries.
//
// # It is the mapper's count, not the CSR's vertex-array length (rmp #2310)
//
// The CSR's array is sized from the present id space, because node ids are packed as
// (intra << shardBits) | shard and an id-indexed array must span that space whatever
// instant is being read. A concurrent capture therefore has vertex SLOTS for ids
// interned after its instant — empty slots naming no node, because the mapper the image
// carries is filtered at the instant and does not hold them.
//
// Reporting the array length here made the manifest disagree with the image it
// describes: measured as manifest Order=2178 against a reconstructed 2176, two slots
// belonging to one transaction that was still in flight. What a consumer means by
// "Order" is how many nodes it will get back, so that is what this returns.
func (c *Capture[W]) Order() uint64 {
	if c.orderKnown {
		return c.order
	}
	return c.csr.Order()
}

// CommitTS reports the MVCC instant this image was captured at, or 0 when the
// originating graph had no MVCC clock.
//
// It is what [Manifest.CommitTS] records and what recovery folds into the derived
// clock floor. Exported so a caller that publishes a capture through its own path
// can carry the instant, and so a test can assert what an image claims.
func (c *Capture[W]) CommitTS() uint64 { return c.commitTS }

// Size reports the edge count of the captured CSR adjacency.
func (c *Capture[W]) Size() uint64 { return c.csr.Size() }

// CaptureGraph serialises every live-graph-derived snapshot component of g
// into memory, producing an atomic image that [WriteCapture] can publish
// without touching g again.
//
// cs is the CSR adjacency the caller has already built for this same instant
// (via [csr.BuildFromAdjList]); it is adopted, not rebuilt, so the caller pays
// the adjacency cost once.
//
// codec, when non-nil, serialises the NodeID->key interning table for ANY key
// type N, making the snapshot self-sufficient so a checkpointer may truncate
// the WAL. When nil the mapper is emitted only for string-keyed graphs (the
// historical v3 behaviour) and non-string snapshots stay v2.
//
// at is the MVCC instant every component is resolved at. When non-nil, writers may
// commit freely throughout the capture and the image still describes exactly that
// instant (rmp #2310). When nil, the capture reads the PRESENT and the caller must
// hold its own exclusion for the read to be atomic — which is the offline and
// single-goroutine shape, and the one [WriteSnapshotFull] uses.
//
// # Caller's obligation
//
// CaptureGraph performs NO locking of its own, and which obligation the caller
// carries depends on whether it supplies an instant.
//
// With at == nil the caller MUST hold whatever exclusion makes the read of g atomic
// with respect to writers, and must have built cs inside that same window. Capturing
// the present without such exclusion reintroduces exactly the cross-component skew
// this type exists to prevent (rmp #2269).
//
// With at != nil the caller does NOT need to exclude writers for the capture — that
// is the whole point — but it MUST have opened at while no write transaction was
// open, and must have built cs at the same instant. See [ErrCaptureNotQuiesced] for
// what that precondition is and why it cannot be dropped.
//
// The component writers this calls take only their own per-shard read locks and
// never re-enter the visibility barrier, so calling CaptureGraph from inside a
// barrier hold is deadlock-free and does not trip the barrier's re-entrancy
// guard. (This used to name lpg.Graph.View as the enclosing hold; rmp #2344
// removed it, and the checkpointer now pins a snapshot with
// [lpg.Graph.BeginRead] instead.)
//
// # Cost
//
// The returned Capture holds the fully serialised snapshot in memory. This is
// deliberate: it is what lets the publish step run lock-free while remaining
// atomic. The peak cost is the on-disk snapshot size, held for the duration of
// the publish, on top of the CSR the caller already built.
// A Capture taken through this entry point carries NO weight codec, so
// publishing it fails with [ErrWeightNotPersistable] when the graph holds
// weights of a type the fixed-width layout cannot size. Use
// [CaptureGraphWithWeightCodec] for those. See csr_weight_codec.go.
func CaptureGraph[N comparable, W any](
	g *lpg.Graph[N, W],
	cs *csr.CSR[W],
	codec keyEncoder[N],
	at *lpg.Snapshot,
) (*Capture[W], error) {
	return CaptureGraphWithWeightCodec[N, W](g, cs, codec, nil, at)
}

// CaptureGraphWithWeightCodec is the weight-codec-aware variant of
// [CaptureGraph]. wcodec is adopted by the returned Capture and used by
// [WriteCapture] to serialise the CSR's weights column for any weight type W —
// pass the owning store's codec, [txn.Store.WeightCodec].
//
// The codec is consulted only for weight types the fixed-width layout cannot
// size, so a float64, int64 or int32 graph publishes byte-identical bytes with
// or without it. A nil wcodec is accepted and behaves exactly as
// [CaptureGraph].
//
// The codec is captured here rather than passed at publish time so that a
// Capture stays what it claims to be: a self-contained image that
// [WriteCapture] can publish without any further input about how to encode it.
func CaptureGraphWithWeightCodec[N comparable, W any](
	g *lpg.Graph[N, W],
	cs *csr.CSR[W],
	codec keyEncoder[N],
	wcodec weightEncoder[W],
	at *lpg.Snapshot,
) (*Capture[W], error) {
	defer metrics.Time("store.snapshot.CaptureGraph").Stop()
	c, err := captureGraph(g, cs, codec, at)
	if err != nil {
		metrics.IncCounter("store.snapshot.CaptureGraph.errors", 1)
		return c, err
	}
	c.wenc = wcodec
	return c, nil
}

//nolint:gocyclo // one capture per component: labels + properties + mapper + tombstones + edgehandles + indexes
func captureGraph[N comparable, W any](
	g *lpg.Graph[N, W],
	cs *csr.CSR[W],
	codec keyEncoder[N],
	at *lpg.Snapshot,
) (*Capture[W], error) {
	if cs == nil {
		return nil, errors.New("snapshot: nil CSR capture")
	}
	// The instant this image is being taken at. Read FIRST, before any component is
	// serialised, so the recorded instant can only be at or BEFORE the state the
	// image contains — never after it. Getting that direction wrong would let
	// recovery restore a clock below data the snapshot already holds.
	//
	// AS OF THE CAPTURE'S OWN INSTANT (rmp #2310). Writers no longer stop for a
	// capture, so "the present" is a moving target and reading it here would record an
	// instant the components do not describe. The snapshot's own start timestamp IS the
	// instant every component below is read at, so it is exactly what the image
	// contains — not merely at-or-before it.
	//
	// A nil snapshot means the caller is reading the present under its own exclusion,
	// which is the only remaining shape that can do so; then the clock is the instant.
	commitTS := g.MVCCStats().Now
	if at != nil {
		commitTS = at.StartTS()
	}
	out := &Capture[W]{csr: cs, commitTS: commitTS}

	// ONE WALK OF THE MAPPER, and every number below derives from it (rmp #2310).
	//
	// A concurrent capture must not compute the same set twice: writers commit between
	// two walks, so a mapper written at one moment and a dead set counted at another do
	// not agree. That disagreement was measured and narrowed three times — 8 nodes, then
	// 10, then 2 — without ever closing, because the window IS the design. So the walk
	// happens once here and its results are handed to the writers.
	//
	// interned is membership as of the instant: the ids the image carries at all. dead
	// is the subset of those the instant sees as removed. Order is their difference,
	// which is exactly what a recovery reconstructs — it interns every mapper entry and
	// then applies the tombstones.
	var interned map[graph.NodeID]struct{}
	var dead []graph.NodeID
	if at != nil {
		interned = make(map[graph.NodeID]struct{}, g.AdjList().Mapper().Len())
		// skippedAt records, per mapper shard, the FIRST id the instant filter dropped.
		// It exists to enforce the contiguity precondition described on
		// [ErrCaptureNotQuiesced]: once a shard has dropped an id, every later id in
		// that shard must be dropped too, or the image carries a hole recovery cannot
		// load. A 256-entry slice rather than a map, because this is walked once per
		// node on every checkpoint.
		skippedAt := make([]graph.NodeID, graph.MapperShardCount())
		skipped := make([]bool, graph.MapperShardCount())
		var gapErr error
		g.AdjList().Mapper().Walk(func(id graph.NodeID, _ N) bool {
			shard := graph.MapperShardOf(id)
			if !g.NodeInternedAsOf(id, at) {
				if !skipped[shard] {
					skipped[shard], skippedAt[shard] = true, id
				}
				return true
			}
			if skipped[shard] {
				// A VISIBLE id above a DROPPED one in the same shard. See
				// [ErrCaptureNotQuiesced] for why no correct image exists here and why
				// failing is the only sound answer.
				gapErr = fmt.Errorf("%w: shard %d drops node %d (interned, not visible at "+
					"instant %d) but keeps node %d above it — the image would have an "+
					"intra-index hole that graph.Mapper.LoadFrom rejects",
					ErrCaptureNotQuiesced, shard, uint64(skippedAt[shard]), at.StartTS(), uint64(id))
				return false
			}
			interned[id] = struct{}{}
			if !g.NodeExistsAsOf(id, at) {
				dead = append(dead, id)
			}
			return true
		})
		if gapErr != nil {
			return nil, gapErr
		}
		// tombstones.bin's input contract is ASCENDING ids, and the walk above does not
		// produce them in that order: a NodeID packs as (intra << shardBits) | shard and
		// Walk is shard-major, so it yields 0, 256, 512, …, 1, 257, … on any graph with
		// more than one node in a shard. The present-time path gets its ascending order
		// from the roaring bitmap's ToArray; this one has to sort. O(D log D) in the
		// tombstone count, after an O(V) walk.
		slices.Sort(dead)
		// Order is NOT computed here. It comes from the two writers' own emitted counts
		// below, because a number counted here and bytes written later are two moments,
		// and under -race that gap reopened the disagreement this task spent four fixes
		// narrowing. The walk's job is to decide MEMBERSHIP once; the writers report how
		// much of it they actually serialised.
	}

	var err error
	// labels.bin — always emitted (possibly empty), matching the writer.
	if out.labels, err = captureComponent(func(w io.Writer) (int64, uint32, error) {
		return WriteLabels(w, g, at)
	}); err != nil {
		return nil, fmt.Errorf("snapshot: capture %s: %w", LabelsFile, err)
	}

	// properties.bin — always emitted (possibly empty), matching the writer.
	if out.properties, err = captureComponent(func(w io.Writer) (int64, uint32, error) {
		return WriteProperties(w, g, at)
	}); err != nil {
		return nil, fmt.Errorf("snapshot: capture %s: %w", PropertiesFile, err)
	}

	// mapper.bin — emitted for every key type when a codec is supplied,
	// otherwise for string-keyed graphs only (the v2 fallback).
	var mapperNodes uint64
	if out.mapper, mapperNodes, err = captureMapper(g, codec, interned); err != nil {
		return nil, fmt.Errorf("snapshot: capture %s: %w", MapperFile, err)
	}

	// tombstones.bin — emitted ONLY when the instant sees a removed node, from the set
	// the single walk above produced. A present-time capture keeps the old behaviour.
	var deadWritten uint64
	if at != nil {
		if len(dead) > 0 {
			if out.tombstones, err = captureComponent(func(w io.Writer) (int64, uint32, error) {
				size, crc, n, werr := writeTombstonesFromN(w, dead)
				deadWritten = n
				return size, crc, werr
			}); err != nil {
				return nil, fmt.Errorf("snapshot: capture %s: %w", TombstonesFile, err)
			}
		}
		// ORDER, from the two writers' OWN numbers: every mapper entry the image carries
		// minus every tombstone it carries. That is exactly what a recovery reconstructs
		// — it interns the first set and then applies the second — so the manifest cannot
		// describe a graph different from the one the bytes produce.
		//
		// Only when a mapper was actually EMITTED. Without one the image names no nodes
		// of its own (a codec-less, non-string-keyed graph), recovery reconstructs the
		// node set from the CSR alone, and mapperNodes is zero because nothing was
		// written — not because the graph is empty. Claiming a known order of zero there
		// would report an empty graph for a populated image.
		if out.mapper.present && mapperNodes >= deadWritten {
			out.order, out.orderKnown = mapperNodes-deadWritten, true
		}

	} else if g.TombstoneCount() > 0 {
		if out.tombstones, err = captureComponent(func(w io.Writer) (int64, uint32, error) {
			return WriteTombstones(w, g, nil)
		}); err != nil {
			return nil, fmt.Errorf("snapshot: capture %s: %w", TombstonesFile, err)
		}
	}

	// edgehandles.bin — emitted ONLY when the graph carries per-handle edge
	// metadata; WriteEdgeHandles reports that itself via emitted.
	var buf bytes.Buffer
	size, crc, emitted, ehErr := WriteEdgeHandles(&buf, g, at)
	if ehErr != nil {
		return nil, fmt.Errorf("snapshot: capture %s: %w", EdgeHandlesFile, ehErr)
	}
	if emitted {
		out.edgeHandles = component{bytes: buf.Bytes(), size: size, crc: crc, present: true}
	}

	// indexes/<name>.bin — one payload per registered index that implements
	// [index.Serializer]. Captured here so the payloads match the adjacency
	// they index rather than a later state.
	if out.indexes, err = captureIndexes(g.IndexManager()); err != nil {
		return nil, err
	}

	cfg := g.Config()
	out.config = GraphConfig{
		Directed:   cfg.Directed,
		Multigraph: cfg.Multigraph,
		Weightless: cfg.Weightless,
	}
	return out, nil
}

// captureComponent runs a streaming component writer against an in-memory
// buffer and records the (bytes, size, CRC32C) triple it produced. The writer's
// own size and CRC are kept verbatim, so the captured component is byte- and
// manifest-identical to one streamed straight to a file.
func captureComponent(write func(io.Writer) (int64, uint32, error)) (component, error) {
	var buf bytes.Buffer
	size, crc, err := write(&buf)
	if err != nil {
		return component{}, err
	}
	return component{bytes: buf.Bytes(), size: size, crc: crc, present: true}, nil
}

// captureMapper serialises the NodeID->key interning table. With a codec it is
// emitted for every key type (a self-sufficient snapshot); without one it is
// emitted only when N is string, and the returned component is absent
// otherwise, which keeps the snapshot at v2 exactly as before.
func captureMapper[N comparable, W any](g *lpg.Graph[N, W], codec keyEncoder[N], interned map[graph.NodeID]struct{}) (comp component, emitted uint64, err error) {
	mapper := g.AdjList().Mapper()
	// BOUNDED AT THE CAPTURED INSTANT (rmp #2310). An id interned after `at` must not
	// reach the image: the recovered graph would hold a node that did not exist at the
	// instant the image claims. An id interned before it stays even if its node was
	// already removed — that pairing of mapper entry plus tombstone is how a removal
	// survives a restart.
	var include func(graph.NodeID) bool
	if interned != nil {
		include = func(id graph.NodeID) bool { _, ok := interned[id]; return ok }
	}
	if codec != nil {
		comp, err = captureComponent(func(w io.Writer) (int64, uint32, error) {
			var n uint64
			size, crc, n, werr := writeMapperN(w, mapper, codec, include)
			emitted = n
			return size, crc, werr
		})
		return comp, emitted, err
	}
	// Reflection-free probe, mirroring writeMapperIfStringKeyed: only the
	// string-keyed mapper has a codec-free serialisation.
	stringMapper, ok := any(mapper).(*graph.Mapper[string])
	if !ok {
		// No mapper is emitted at all, so the image names no nodes of its own and
		// [Capture.Order] falls back to the CSR, exactly as it did before.
		return component{}, 0, nil
	}
	comp, err = captureComponent(func(w io.Writer) (int64, uint32, error) {
		size, crc, n, werr := writeMapperStringN(w, stringMapper, include)
		emitted = n
		return size, crc, werr
	})
	return comp, emitted, err
}

// captureIndexes serialises each registered index that supports serialisation.
// It mirrors writeIndexesWith's selection rules — skip a subscriber that is not
// an [index.Serializer] (rebuild-on-restart), skip an index dropped between
// ListIndexes and GetIndex, and reject a name that would escape the indexes/
// directory — so the published set is identical, only captured earlier.
func captureIndexes(m *index.Manager) ([]capturedIndex, error) {
	if m == nil || m.Count() == 0 {
		return nil, nil
	}
	names := m.ListIndexes()
	out := make([]capturedIndex, 0, len(names))
	for _, name := range names {
		sub, err := m.GetIndex(name)
		if err != nil {
			// Race: dropped between ListIndexes and GetIndex. The manager is
			// the source of truth and a dropped index has no state to keep.
			continue
		}
		ser, ok := sub.(index.Serializer)
		if !ok {
			continue
		}
		if err := validateIndexName(name); err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if serr := ser.Serialize(&buf); serr != nil {
			return nil, fmt.Errorf("snapshot: index %q: %w", name, serr)
		}
		b := buf.Bytes()
		out = append(out, capturedIndex{
			name: name,
			comp: component{
				bytes:   b,
				size:    int64(len(b)),
				crc:     crc32.Checksum(b, castagnoli),
				present: true,
			},
		})
	}
	return out, nil
}
