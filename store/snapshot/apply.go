package snapshot

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ErrMapperApply is returned by [ApplyMapperToGraph] when the supplied
// readback violates an invariant the writer is responsible for
// upholding (intra-shard gap, hash/shard mismatch, duplicate key, or a
// non-empty target mapper). It wraps the underlying [graph.ErrMapper…]
// sentinels so callers can branch on the typed cause via [errors.Is].
var ErrMapperApply = errors.New("snapshot: cannot apply mapper")

// ApplyMapperToGraph rebuilds g's underlying [graph.Mapper] from the
// snapshot readback. It is only meaningful for string-keyed graphs:
// any other N type returns nil without touching g, because no v3
// mapper.bin is ever produced for non-string graphs. The caller is
// expected to invoke this function before [ApplyCSRToGraph],
// [ApplyLabelsToGraph], or [ApplyPropertiesToGraph] so subsequent
// resolution calls see the restored interning table.
//
// Pre-condition: g must hold a fresh (empty) mapper. Calling on a
// graph that already has interned values returns [ErrMapperApply]
// wrapping [graph.ErrMapperNotEmpty] so the caller can distinguish a
// programmer error from a corruption error.
//
// Concurrency: [ApplyMapperToGraph] is not safe to call concurrently
// with mutations or reads on g. It is intended for the one-shot
// snapshot-load phase of recovery.
func ApplyMapperToGraph[N comparable, W any](g *lpg.Graph[N, W], rb MapperReadback) error {
	defer metrics.Time("store.snapshot.ApplyMapperToGraph").Stop()
	if len(rb.Pairs) == 0 {
		return nil
	}
	mapper := g.AdjList().Mapper()
	stringMapper, ok := any(mapper).(*graph.Mapper[string])
	if !ok {
		// A v3 snapshot mapper.bin only exists for string-keyed graphs;
		// the recovery wiring should never call this with N!=string.
		// Treat as a logic error rather than a corruption: fail loudly.
		metrics.IncCounter("store.snapshot.ApplyMapperToGraph.errors", 1)
		return fmt.Errorf("%w: non-string-keyed graph received v3 mapper readback", ErrMapperApply)
	}
	entries := make([]graph.MapperEntry[string], len(rb.Pairs))
	for i := range rb.Pairs {
		entries[i] = graph.MapperEntry[string]{
			ID:  rb.Pairs[i].ID,
			Key: rb.Pairs[i].Key,
		}
	}
	if err := stringMapper.LoadFrom(entries); err != nil {
		metrics.IncCounter("store.snapshot.ApplyMapperToGraph.errors", 1)
		return fmt.Errorf("%w: %w", ErrMapperApply, err)
	}
	return nil
}

// ApplyMapperToGraphWithCodec rebuilds g's underlying [graph.Mapper]
// from a version-2 (codec) snapshot readback for ANY comparable key
// type N. Each [MapperRawPair] carries the codec-encoded key bytes the
// snapshot writer produced via [WriteMapper]; this function decodes
// them back into N via the supplied codec (the same one the store uses
// on the WAL) and seeds the interning table through [graph.Mapper.LoadFrom].
//
// It is the codec-aware dual of [ApplyMapperToGraph]: recovery calls
// this when the loaded readback carries RawPairs (non-string keys) and
// the string-specialised path when it carries Pairs. An empty readback
// is a no-op.
//
// Pre-condition and concurrency contract match [ApplyMapperToGraph]: g
// must hold a fresh (empty) mapper, and the call must not race with any
// other access to g. A decode failure surfaces as [ErrMapperApply]
// wrapping the codec error; a structural violation surfaces as
// [ErrMapperApply] wrapping the relevant [graph.ErrMapper…] sentinel.
func ApplyMapperToGraphWithCodec[N comparable, W any](g *lpg.Graph[N, W], rb MapperReadback, codec keyDecoder[N]) error {
	defer metrics.Time("store.snapshot.ApplyMapperToGraphWithCodec").Stop()
	if len(rb.RawPairs) == 0 {
		return nil
	}
	if codec == nil {
		metrics.IncCounter("store.snapshot.ApplyMapperToGraphWithCodec.errors", 1)
		return fmt.Errorf("%w: nil codec", ErrMapperApply)
	}
	mapper := g.AdjList().Mapper()
	entries := make([]graph.MapperEntry[N], len(rb.RawPairs))
	for i := range rb.RawPairs {
		key, rest, derr := codec.Decode(rb.RawPairs[i].Key)
		if derr != nil {
			metrics.IncCounter("store.snapshot.ApplyMapperToGraphWithCodec.errors", 1)
			return fmt.Errorf("%w: decode key for node %d: %w",
				ErrMapperApply, uint64(rb.RawPairs[i].ID), derr)
		}
		if len(rest) != 0 {
			// The writer encoded exactly one key per record; trailing bytes
			// mean the on-disk record and the codec disagree on framing.
			metrics.IncCounter("store.snapshot.ApplyMapperToGraphWithCodec.errors", 1)
			return fmt.Errorf("%w: trailing bytes after key for node %d (%d left)",
				ErrMapperApply, uint64(rb.RawPairs[i].ID), len(rest))
		}
		entries[i] = graph.MapperEntry[N]{ID: rb.RawPairs[i].ID, Key: key}
	}
	if err := mapper.LoadFrom(entries); err != nil {
		metrics.IncCounter("store.snapshot.ApplyMapperToGraphWithCodec.errors", 1)
		return fmt.Errorf("%w: %w", ErrMapperApply, err)
	}
	return nil
}

// ApplyCSRToGraph replays the adjacency in rb into g. The pre-
// condition is that g's underlying mapper has already been populated
// with every NodeID referenced by rb — typically by an immediately-
// preceding [ApplyMapperToGraph] (v3 snapshots) or by a WAL replay
// (v2 snapshots that pair with a WAL prefix). Records whose
// endpoints the mapper cannot resolve are skipped and counted via
// `store.snapshot.ApplyCSR.unresolved`; the function does not return
// an error for them so a partial mapper degrades cleanly rather than
// aborting recovery mid-way.
//
// Weights in the DENSE native layout are decoded for the fixed-width
// primitives (int8/uint8/bool, int16/uint16, int32/uint32/float32,
// int/uint/int64/uint64/float64/uintptr). Weights in the codec-encoded
// layout are decoded through the supplied weight codec, which covers
// every other W.
//
// The metric `store.snapshot.ApplyCSR.weightFallback` counts dense-layout
// slots that decoded to the zero value for want of bytes. It is NOT a
// signal that weights were lost to an unsupported type: that loss used to
// happen at WRITE time, where the file recorded "no weights" and there
// was nothing here to count. rmp #2526 removed that path — the writer now
// refuses — so this counter means a malformed file, not an unsupported
// weight type.
//
// ApplyCSRToGraph is idempotent against a freshly-loaded mapper but
// not against a graph that already contains edges: re-applying a CSR
// to a graph with existing edges may duplicate them in multigraph
// mode or no-op in simple-graph mode. Callers should run this exactly
// once per recovery, immediately after the mapper restore and before
// any WAL replay.
//
// rb is passed by pointer to avoid copying the three slices in the
// readback (vertices, edges, weight bytes) on every call. The
// function does not mutate rb.
//
// A snapshot whose weights are codec-encoded cannot be applied through this
// entry point, which has no codec to decode them: it returns
// [ErrWeightCodecRequired]. Use [ApplyCSRToGraphWithWeightCodec].
func ApplyCSRToGraph[N comparable, W any](g *lpg.Graph[N, W], rb *CSRReadback) error {
	return ApplyCSRToGraphWithWeightCodec[N, W](g, rb, nil)
}

// ApplyCSRToGraphWithWeightCodec is the weight-codec-aware variant of
// [ApplyCSRToGraph]. wdec decodes the variable-width weights section written by
// [WriteCSRWithWeightCodec]; pass the owning store's codec,
// [txn.Store.WeightCodec].
//
// A nil wdec is accepted and behaves exactly as [ApplyCSRToGraph] for every
// snapshot whose weights are in the dense native layout (the fixed-width
// primitives) or absent. It is refused with [ErrWeightCodecRequired] for a
// snapshot whose weights ARE codec-encoded: those weights are durably present
// on disk, and applying zero weights instead would discard committed data
// (rmp #2526).
//
//nolint:gocyclo // CSR apply walks every src slot, resolves endpoints, decodes weight by W type
func ApplyCSRToGraphWithWeightCodec[N comparable, W any](g *lpg.Graph[N, W], rb *CSRReadback, wdec weightDecoder[W]) error {
	defer metrics.Time("store.snapshot.ApplyCSRToGraph").Stop()
	if len(rb.Vertices) == 0 {
		return nil
	}
	mapper := g.AdjList().Mapper()

	// Guard the on-disk weight width against this store's actual W before
	// any weight decode. rb.WeightSize is attacker-controlled on the
	// recovery trust boundary: a forged/tampered snapshot supplies the CSR
	// weights-flag byte directly (writer.go readCSRLimited reads flag[1]
	// without validating it against W). A width that disagrees with
	// csrWeightSize[W]() means the whole weights section is misaligned, and
	// a width narrower than the native W (e.g. 1 while W is int64) drives
	// decodeCSRWeight to read past a short per-edge slice
	// (binary.LittleEndian.Uint64 over 1 byte) and panic at store-open,
	// outside the per-connection recover guards — crashing the process. Fail
	// stop with a typed error instead. A weightless snapshot carries no
	// weight bytes (HasWeights == false), so the width is irrelevant there.
	//
	// The codec-encoded section (rmp #2526) is exempt from the WIDTH check —
	// it has no fixed width by construction — but is subject to a stricter one
	// of its own: it can only be applied by a caller that supplied a decoder.
	// Falling back to zero weights here would discard weights that are durably
	// on disk, which is the read-side form of the very loss #2526 closes.
	switch {
	case rb.CodecWeights():
		if wdec == nil {
			metrics.IncCounter("store.snapshot.ApplyCSR.weightCodecMissing", 1)
			return fmt.Errorf("%w (weight type %T, %d edges): supply the store's WeightCodec"+
				" (recovery.Options.WeightCodec / checkpoint.WithWeightCodec)",
				ErrWeightCodecRequired, *new(W), len(rb.Edges))
		}
		// The offsets array must index the edge column exactly. readCodecWeights
		// already proved it is zero-based, monotonic and within the payload; this
		// pins the remaining degree of freedom, that it describes THIS many edges.
		if len(rb.WeightOffsets) != len(rb.Edges)+1 {
			metrics.IncCounter("store.snapshot.ApplyCSR.corrupt", 1)
			return fmt.Errorf("%w: CSR codec weights offsets length %d, want %d (edges+1)",
				ErrCorrupted, len(rb.WeightOffsets), len(rb.Edges)+1)
		}
	case rb.HasWeights && rb.WeightSize != csrWeightSize[W]():
		metrics.IncCounter("store.snapshot.ApplyCSR.corrupt", 1)
		return fmt.Errorf("%w: CSR weight width %d does not match store weight size %d",
			ErrCorrupted, rb.WeightSize, csrWeightSize[W]())
	}

	// CSR vertices is the offset array: vertices[i]..vertices[i+1] is
	// the half-open edge slice for source NodeID i. Walk every src
	// slot up to len(vertices)-1. Slots without an interned value are
	// silently skipped (they exist only because the mapper packs
	// NodeIDs into 256 shards, so the addressable range typically
	// overshoots Order()).
	maxSrc := uint64(len(rb.Vertices) - 1)
	weightSize := uint64(rb.WeightSize)
	codecWeights := rb.CodecWeights()

	// Validate the offset array once, up front. The vertices slice is
	// file-controlled: in v3-snapshot recovery an attacker supplies both
	// the interned NodeIDs and the CSR arrays. Without this guard the
	// inner loop would index rb.Edges[k] for k in [start,end) using a
	// hostile, non-monotonic, or out-of-range offset (e.g. 1<<40),
	// causing an out-of-bounds panic instead of a clean error. A
	// legitimate CSR is always monotonic (vertices[i] <= vertices[i+1])
	// and its last offset never exceeds len(Edges), so valid snapshots
	// are unaffected. The check is a single O(n) pass over the offset
	// array, negligible next to the edge replay it precedes.
	edgeCount := uint64(len(rb.Edges))
	for i := uint64(0); i < maxSrc; i++ {
		if rb.Vertices[i] > rb.Vertices[i+1] {
			metrics.IncCounter("store.snapshot.ApplyCSR.corrupt", 1)
			return fmt.Errorf("%w: non-monotonic vertex offsets at index %d (%d > %d)",
				ErrCorrupted, i, rb.Vertices[i], rb.Vertices[i+1])
		}
	}
	if last := rb.Vertices[maxSrc]; last > edgeCount {
		metrics.IncCounter("store.snapshot.ApplyCSR.corrupt", 1)
		return fmt.Errorf("%w: final vertex offset %d exceeds edge count %d",
			ErrCorrupted, last, edgeCount)
	}

	// SEED THE HANDLE COUNTER ABOVE THE WHOLE IMAGE, BEFORE ANY EDGE IS INSERTED
	// (rmp #2317).
	//
	// The loop below re-stamps a slot's original handle when the image carries one
	// and mints a fresh one when it does not, and it used to seed the counter
	// per-edge as each original was restored. That ordering has a collision: an
	// edge minted at position k can be handed a handle that a LATER position
	// restores, and AddEdgeHIfAbsent is idempotent against a handle already
	// present — so the later edge would silently no-op onto the earlier slot
	// instead of being inserted.
	//
	// Folding the maximum first makes every minted handle strictly greater than
	// every restored one, which removes the collision class rather than making it
	// unlikely. It costs one pass over a slice the loop below already walks.
	var maxHandle uint64
	for _, h := range rb.Handles {
		if h > maxHandle {
			maxHandle = h
		}
	}
	if maxHandle > 0 {
		g.SeedEdgeHandle(maxHandle + 1)
	}

	for src := uint64(0); src < maxSrc; src++ {
		start := rb.Vertices[src]
		end := rb.Vertices[src+1]
		if start >= end {
			// start == end is an empty edge slice for this src. The
			// up-front validation guarantees start > end never reaches
			// here, but the >= comparison also guards the end-start
			// metric subtraction below against an unsigned underflow.
			continue
		}
		srcN, ok := mapper.Resolve(graph.NodeID(src))
		if !ok {
			metrics.IncCounter("store.snapshot.ApplyCSR.unresolved", end-start)
			continue
		}
		for k := start; k < end; k++ {
			dstID := rb.Edges[k]
			dstN, ok := mapper.Resolve(dstID)
			if !ok {
				metrics.IncCounter("store.snapshot.ApplyCSR.unresolved", 1)
				continue
			}
			var weight W
			switch {
			case codecWeights:
				// Random access by slot: the offsets array was validated
				// zero-based, monotonic, within the payload and exactly
				// edges+1 long, so this slice is unconditionally in range.
				var werr error
				if weight, werr = decodeCSRWeightCodec(
					rb.WeightBytes[rb.WeightOffsets[k]:rb.WeightOffsets[k+1]], wdec, k); werr != nil {
					metrics.IncCounter("store.snapshot.ApplyCSR.corrupt", 1)
					return werr
				}
			case rb.HasWeights && len(rb.WeightBytes) > 0:
				off := k * weightSize
				if uint64(len(rb.WeightBytes)) >= off+weightSize {
					weight = decodeCSRWeight[W](rb.WeightBytes[off : off+weightSize])
				}
			}
			// When the snapshot carries a handle column, re-insert the edge
			// with its ORIGINAL stable handle so the recovered parallel edge
			// keeps its per-CREATE identity (the per-handle type/properties
			// the edgehandles.bin component reattaches resolve by this same
			// handle). AddEdgeHIfAbsent is idempotent against a handle already
			// present, and re-seeds the high-water counter (invariant I5) so a
			// post-recovery edge creation never re-mints a live handle. A
			// snapshot without a handle column (legacy, or a graph that never
			// used AddEdgeH) falls back to the plain handle-less AddEdge.
			if len(rb.Handles) > int(k) && rb.Handles[k] != 0 {
				h := rb.Handles[k]
				if _, err := g.AddEdgeHIfAbsent(srcN, dstN, weight, h); err != nil {
					metrics.IncCounter("store.snapshot.ApplyCSR.addEdgeErrors", 1)
					return fmt.Errorf("snapshot.ApplyCSRToGraph: AddEdgeH: %w", err)
				}
				g.SeedEdgeHandle(h + 1)
				continue
			}
			if err := g.AddEdge(srcN, dstN, weight); err != nil {
				metrics.IncCounter("store.snapshot.ApplyCSR.addEdgeErrors", 1)
				return fmt.Errorf("snapshot.ApplyCSRToGraph: AddEdge: %w", err)
			}
		}
	}
	// Ensure every interned-but-isolated node is present in the
	// adjacency layer. Isolated nodes have no entry in vertices, so
	// the AddEdge loop above never touched them. AdjList.AddNode is
	// idempotent (it just calls mapper.Intern, which is a cache hit
	// here) and the LPG's Order() already counts them via the mapper.
	// We still walk the mapper to maintain symmetry with the write
	// path's expectations for any future AdjList that materialises
	// per-node state.
	mapper.Walk(func(_ graph.NodeID, n N) bool {
		_ = g.AddNode(n)
		return true
	})
	return nil
}

// decodeCSRWeight reconstructs the W value previously serialised by
// [WriteCSR] for one edge. The conversion mirrors the [csrWeightSize]
// type switch in writer.go: writer and reader must agree on width and
// endianness for each W. Unsupported W types return the zero value
// and bump the `store.snapshot.ApplyCSR.weightFallback` counter so
// observability surfaces the loss.
//
//nolint:gocyclo // CSR weight decode: one branch per supported W type
func decodeCSRWeight[W any](buf []byte) W {
	var zero W
	if _, ok := any(zero).(struct{}); ok {
		return zero
	}
	if len(buf) == 0 {
		metrics.IncCounter("store.snapshot.ApplyCSR.weightFallback", 1)
		return zero
	}
	// We use a typed-result switch driven by the W zero value: each
	// arm decodes the on-disk bytes and writes back through a
	// pointer to a fresh W instance, then returns it. The any()
	// indirection here is unavoidable for the type switch; the hot
	// recovery loop runs this once per edge, not in any inner search
	// pass, so the cost is acceptable.
	var out W
	switch v := any(&out).(type) {
	case *int8:
		*v = int8(buf[0])
	case *uint8:
		*v = buf[0]
	case *bool:
		*v = buf[0] != 0
	case *int16:
		*v = int16(binary.LittleEndian.Uint16(buf))
	case *uint16:
		*v = binary.LittleEndian.Uint16(buf)
	case *int32:
		*v = int32(binary.LittleEndian.Uint32(buf))
	case *uint32:
		*v = binary.LittleEndian.Uint32(buf)
	case *float32:
		*v = math.Float32frombits(binary.LittleEndian.Uint32(buf))
	case *int:
		*v = int(int64(binary.LittleEndian.Uint64(buf))) //nolint:gosec // round-trip of writer-emitted bytes
	case *uint:
		*v = uint(binary.LittleEndian.Uint64(buf))
	case *int64:
		*v = int64(binary.LittleEndian.Uint64(buf))
	case *uint64:
		*v = binary.LittleEndian.Uint64(buf)
	case *float64:
		*v = math.Float64frombits(binary.LittleEndian.Uint64(buf))
	case *uintptr:
		*v = uintptr(binary.LittleEndian.Uint64(buf))
	default:
		metrics.IncCounter("store.snapshot.ApplyCSR.weightFallback", 1)
	}
	return out
}
