package snapshot

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

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
// from the same instant. A Capture is taken by the caller INSIDE its own
// exclusion window — for the checkpointer, under the store's commit
// serialisation and inside [lpg.Graph.View] — and the publish step
// ([WriteCapture]) touches no graph at all. Publishing stays lock-free, because
// what phase 2 writes is bytes, not a graph.
//
// Concurrency: a Capture is an immutable value once returned; it shares no
// state with the graph it was taken from and is safe to publish from another
// goroutine. Taking one requires the caller to hold whatever exclusion makes
// the read atomic — see [CaptureGraph].
//
// Cost: a Capture holds the whole serialised snapshot in memory until it is
// published. That is the price of an atomic image; see [CaptureGraph].
type Capture[W any] struct {
	csr         *csr.CSR[W]
	labels      component
	properties  component
	mapper      component
	tombstones  component
	edgeHandles component
	indexes     []capturedIndex
	config      GraphConfig
}

// Order reports the vertex count of the captured CSR adjacency.
func (c *Capture[W]) Order() uint64 { return c.csr.Order() }

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
// # Caller's obligation
//
// CaptureGraph performs NO locking of its own. The caller MUST hold whatever
// exclusion makes the read of g atomic with respect to writers, and must have
// built cs inside that same window. For the checkpointer that is the store's
// commit serialisation plus [lpg.Graph.View]; for an offline or
// single-goroutine caller it is trivially satisfied. Capturing without such
// exclusion reintroduces exactly the cross-component skew this type exists to
// prevent (rmp #2269).
//
// The component writers this calls take only their own per-shard read locks and
// never re-enter the visibility barrier, so calling CaptureGraph inside
// [lpg.Graph.View] is deadlock-free and does not trip the barrier's
// re-entrancy guard.
//
// # Cost
//
// The returned Capture holds the fully serialised snapshot in memory. This is
// deliberate: it is what lets the publish step run lock-free while remaining
// atomic. The peak cost is the on-disk snapshot size, held for the duration of
// the publish, on top of the CSR the caller already built.
func CaptureGraph[N comparable, W any](
	g *lpg.Graph[N, W],
	cs *csr.CSR[W],
	codec keyEncoder[N],
) (*Capture[W], error) {
	defer metrics.Time("store.snapshot.CaptureGraph").Stop()
	c, err := captureGraph(g, cs, codec)
	if err != nil {
		metrics.IncCounter("store.snapshot.CaptureGraph.errors", 1)
	}
	return c, err
}

//nolint:gocyclo // one capture per component: labels + properties + mapper + tombstones + edgehandles + indexes
func captureGraph[N comparable, W any](
	g *lpg.Graph[N, W],
	cs *csr.CSR[W],
	codec keyEncoder[N],
) (*Capture[W], error) {
	if cs == nil {
		return nil, errors.New("snapshot: nil CSR capture")
	}
	out := &Capture[W]{csr: cs}

	var err error
	// labels.bin — always emitted (possibly empty), matching the writer.
	if out.labels, err = captureComponent(func(w io.Writer) (int64, uint32, error) {
		return WriteLabels(w, g)
	}); err != nil {
		return nil, fmt.Errorf("snapshot: capture %s: %w", LabelsFile, err)
	}

	// properties.bin — always emitted (possibly empty), matching the writer.
	if out.properties, err = captureComponent(func(w io.Writer) (int64, uint32, error) {
		return WriteProperties(w, g)
	}); err != nil {
		return nil, fmt.Errorf("snapshot: capture %s: %w", PropertiesFile, err)
	}

	// mapper.bin — emitted for every key type when a codec is supplied,
	// otherwise for string-keyed graphs only (the v2 fallback).
	if out.mapper, err = captureMapper(g, codec); err != nil {
		return nil, fmt.Errorf("snapshot: capture %s: %w", MapperFile, err)
	}

	// tombstones.bin — emitted ONLY when the graph currently has a removed
	// node, so a graph that never deleted one produces a byte-identical
	// snapshot to the pre-component layout. The count probe and the write must
	// observe the same instant, which the caller's exclusion guarantees.
	if g.TombstoneCount() > 0 {
		if out.tombstones, err = captureComponent(func(w io.Writer) (int64, uint32, error) {
			return WriteTombstones(w, g)
		}); err != nil {
			return nil, fmt.Errorf("snapshot: capture %s: %w", TombstonesFile, err)
		}
	}

	// edgehandles.bin — emitted ONLY when the graph carries per-handle edge
	// metadata; WriteEdgeHandles reports that itself via emitted.
	var buf bytes.Buffer
	size, crc, emitted, ehErr := WriteEdgeHandles(&buf, g)
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
func captureMapper[N comparable, W any](g *lpg.Graph[N, W], codec keyEncoder[N]) (component, error) {
	mapper := g.AdjList().Mapper()
	if codec != nil {
		return captureComponent(func(w io.Writer) (int64, uint32, error) {
			return WriteMapper(w, mapper, codec)
		})
	}
	// Reflection-free probe, mirroring writeMapperIfStringKeyed: only the
	// string-keyed mapper has a codec-free serialisation.
	stringMapper, ok := any(mapper).(*graph.Mapper[string])
	if !ok {
		return component{}, nil
	}
	return captureComponent(func(w io.Writer) (int64, uint32, error) {
		return WriteMapperString(w, stringMapper)
	})
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
