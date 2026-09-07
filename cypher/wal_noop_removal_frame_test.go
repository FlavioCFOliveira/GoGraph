package cypher_test

// wal_noop_removal_frame_test.go — regression gate for rmp #2734.
//
// A relationship removal that takes NOTHING out of the in-memory adjacency must
// not buy a durable WAL frame — a frame that removes nothing still costs bytes,
// an fsync's worth of I/O, and a replay step, and the WAL may only describe work
// actually done (the principle rmp #2694 / rmp #2725 set for the refused-removal
// case). Both adapters' write paths reach that state through ordinary,
// spec-legal openCypher:
//
//   - MATCH (a)-[r:R]-(b) DELETE r binds ONE stored relationship in BOTH
//     traversal directions, so the executor deletes it twice. openCypher makes
//     the second delete a no-op, and the engine reports relationships-deleted 1
//     — but before rmp #2734 it wrote two frames.
//   - The same query after a WITH projection loses the bound edge position, so
//     the delete takes the per-pair [walMutatorAdapter.RemoveEdge] path instead
//     of the by-handle one; an UNWIND that doubles the rows took it to four
//     frames for one relationship removed.
//
// The gate is NOT unconditional, and that is the load-bearing part of this file.
// It applies only where the WAL faithfully describes the graph's adjacency —
// i.e. on a DIRECTED graph. On an undirected one, RemoveAllEdgesFrom retires
// each mirror arc while emitting a frame only for the forward arc, so the
// in-memory presence probe stops predicting what a replay will find and a
// removal that is a no-op in memory can be the frame that performs the deletion
// on replay. TestUndirectedEngine_KeepsDescribingRemovalsThatTookNothing pins
// that, because suppressing those frames LOSES deletions.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// noopFrameEngine builds a WAL-backed engine over a multigraph with the
// requested directedness, returning the engine, the WAL writer and the store
// directory. The engine's construction warnings are silenced for the whole test
// (the undirected case emits one by design, #1892).
func noopFrameEngine(t *testing.T, directed bool) (*cypher.Engine, *wal.Writer, string) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: directed, Multigraph: true})
	st := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return cypher.NewEngineWithOptions(g, cypher.EngineOptions{Store: st}), w, dir
}

// noopFrameRun executes one write query to completion and returns its Result so
// the caller can read the openCypher side-effect counters.
func noopFrameRun(t *testing.T, eng *cypher.Engine, q string) *cypher.Result {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		t.Fatalf("RunInTx(%q): iterate: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("RunInTx(%q): close: %v", q, err)
	}
	return res
}

// noopRemovalFrame is one decoded edge-removal frame: which op kind it is and
// which ordered endpoint pair it names.
type noopRemovalFrame struct {
	src  string
	dst  string
	kind txn.OpKind
}

func (f noopRemovalFrame) String() string {
	name := "OpRemoveEdge"
	if f.kind == txn.OpRemoveEdgeByHandle {
		name = "OpRemoveEdgeByHandle"
	}
	return fmt.Sprintf("%s(%s -> %s)", name, f.src, f.dst)
}

// noopFrameRemovals decodes the WAL at path and returns every edge-removal frame
// in file order, plus the total number of frames. Only the two kinds this gate
// covers are collected; the endpoints are decoded with the same string codec the
// store wrote them with.
//
// Frame layout (store/txn): [version][kind]([txnSeq] for v3)[src][dst]…, where
// the string codec self-delimits each endpoint with a uint32 length prefix.
func noopFrameRemovals(t *testing.T, path string) (removals []noopRemovalFrame, total int) {
	t.Helper()
	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("wal.OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	codec := txn.NewStringCodec()
	for f := range r.Frames() {
		total++
		p := f.Payload
		if len(p) < 2 {
			continue
		}
		version, kind := p[0], txn.OpKind(p[1])
		if kind != txn.OpRemoveEdge && kind != txn.OpRemoveEdgeByHandle {
			continue
		}
		body := p[2:]
		if version == txn.OpRecordV3 {
			if len(body) < 8 {
				t.Fatalf("v3 frame shorter than its transaction sequence (%d bytes)", len(body))
			}
			body = body[8:]
		}
		src, rest, derr := codec.Decode(body)
		if derr != nil {
			t.Fatalf("decode src endpoint: %v", derr)
		}
		dst, _, derr := codec.Decode(rest)
		if derr != nil {
			t.Fatalf("decode dst endpoint: %v", derr)
		}
		removals = append(removals, noopRemovalFrame{kind: kind, src: src, dst: dst})
	}
	if err := r.TailError(); err != nil {
		t.Fatalf("WAL iteration stopped at %d: %v", r.TailOffset(), err)
	}
	return removals, total
}

// noopFrameRecoverEdges recovers the store directory and returns its live edges
// as sorted "src->dst" strings. dropFrame, when >= 0, is the whole-file index of
// one frame to omit while rebuilding the WAL into a scratch directory — the
// control that shows this oracle can fail.
func noopFrameRecoverEdges(t *testing.T, dir string, dropFrame int) []string {
	t.Helper()
	work := dir
	if dropFrame >= 0 {
		work = t.TempDir()
		src, err := wal.OpenReader(filepath.Join(dir, "wal"))
		if err != nil {
			t.Fatalf("wal.OpenReader: %v", err)
		}
		out, err := os.Create(filepath.Join(work, "wal")) //nolint:gosec // G304: work is this test's own t.TempDir() and the leaf name is a literal; no external input reaches the path
		if err != nil {
			_ = src.Close()
			t.Fatalf("create scratch wal: %v", err)
		}
		i := 0
		for f := range src.Frames() {
			if i != dropFrame {
				if _, werr := wal.Encode(out, f); werr != nil {
					t.Fatalf("re-encode frame %d: %v", i, werr)
				}
			}
			i++
		}
		tailErr := src.TailError()
		_ = src.Close()
		if cerr := out.Close(); cerr != nil {
			t.Fatalf("close scratch wal: %v", cerr)
		}
		if tailErr != nil {
			t.Fatalf("WAL iteration stopped: %v", tailErr)
		}
		if dropFrame >= i {
			t.Fatalf("drop index %d is past the end of a %d-frame WAL", dropFrame, i)
		}
	}
	rec, err := recovery.Open[string, float64](work, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	var edges []string
	rec.Graph.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		for nb := range rec.Graph.AdjList().Neighbours(key) {
			edges = append(edges, key+"->"+nb)
		}
		return true
	})
	sort.Strings(edges)
	return edges
}

// TestDeleteRelationship_PerPairRemovalThatTookNothingWritesNoWALFrame drives the
// per-pair [walMutatorAdapter.RemoveEdge] path. The WITH projection drops the
// bound edge position, so the delete cannot resolve a stable handle and falls to
// the endpoint-pair removal; the undirected pattern binds the one stored
// relationship twice and the UNWIND doubles that again.
//
// Before rmp #2734 this wrote FOUR OpRemoveEdge frames for the one relationship
// it removed — three of them removing nothing, 64.7% of the delete transaction's
// WAL bytes.
func TestDeleteRelationship_PerPairRemovalThatTookNothingWritesNoWALFrame(t *testing.T) {
	eng, w, dir := noopFrameEngine(t, true)

	noopFrameRun(t, eng, `CREATE (a:A {n:'a'})-[:R]->(b:B {n:'b'})`)
	if err := w.Sync(); err != nil {
		t.Fatalf("wal.Sync: %v", err)
	}
	_, before := noopFrameRemovals(t, filepath.Join(dir, "wal"))

	res := noopFrameRun(t, eng, `MATCH (a)-[r:R]-(b) WITH r UNWIND [1, 2] AS i DELETE r`)
	if got := res.Counters().RelationshipsDeleted; got != 1 {
		t.Fatalf("relationships-deleted = %d, want 1 (openCypher counts the one relationship once)", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	removals, total := noopFrameRemovals(t, filepath.Join(dir, "wal"))
	if total <= before {
		t.Fatalf("the delete transaction appended %d frames, want > 0", total-before)
	}
	if len(removals) != 1 {
		t.Fatalf("the WAL holds %d edge-removal frames for 1 relationship removed, want 1: %v", len(removals), removals)
	}
	if removals[0].kind != txn.OpRemoveEdge {
		t.Fatalf("removal frame is %v, want an OpRemoveEdge (the per-pair path this test drives)", removals[0])
	}
}

// TestDeleteRelationship_ByHandleRemovalThatTookNothingWritesNoWALFrame drives
// the [walMutatorAdapter.RemoveEdgeByHandle] twin with the plainest form of the
// same query: no projection, so the bound edge position survives and the delete
// resolves a stable handle.
//
// Before rmp #2734 this wrote TWO OpRemoveEdgeByHandle frames for the one
// relationship it removed — 39.2% of the delete transaction's WAL bytes.
func TestDeleteRelationship_ByHandleRemovalThatTookNothingWritesNoWALFrame(t *testing.T) {
	eng, w, dir := noopFrameEngine(t, true)

	noopFrameRun(t, eng, `CREATE (a:A {n:'a'})-[:R]->(b:B {n:'b'})`)
	res := noopFrameRun(t, eng, `MATCH (a)-[r:R]-(b) DELETE r`)
	if got := res.Counters().RelationshipsDeleted; got != 1 {
		t.Fatalf("relationships-deleted = %d, want 1", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	removals, _ := noopFrameRemovals(t, filepath.Join(dir, "wal"))
	if len(removals) != 1 {
		t.Fatalf("the WAL holds %d edge-removal frames for 1 relationship removed, want 1: %v", len(removals), removals)
	}
	if removals[0].kind != txn.OpRemoveEdgeByHandle {
		t.Fatalf("removal frame is %v, want an OpRemoveEdgeByHandle (the by-handle path this test drives)", removals[0])
	}
}

// TestDeleteRelationship_SuppressedFramesReplayToTheSameGraph is the durability
// half of the gate: the leaner WAL must still recover the graph the delete
// produced. The final assertion is the CONTROL — rebuilding the WAL without the
// one surviving removal frame must bring the relationship back. Without it,
// "recovery finds no edges" would pass even on a WAL that never described the
// deletion at all, and the oracle would prove nothing.
func TestDeleteRelationship_SuppressedFramesReplayToTheSameGraph(t *testing.T) {
	eng, w, dir := noopFrameEngine(t, true)

	noopFrameRun(t, eng, `CREATE (a:A {n:'a'})-[:R]->(b:B {n:'b'})`)
	noopFrameRun(t, eng, `MATCH (a)-[r:R]-(b) WITH r UNWIND [1, 2] AS i DELETE r`)
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	if edges := noopFrameRecoverEdges(t, dir, -1); len(edges) != 0 {
		t.Fatalf("recovery reconstructed %v, want no edges — the deletion did not survive the WAL", edges)
	}

	// Locate the single removal frame in the file so the control can drop it.
	dropAt := -1
	idx := 0
	r, err := wal.OpenReader(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.OpenReader: %v", err)
	}
	for f := range r.Frames() {
		if len(f.Payload) >= 2 {
			switch txn.OpKind(f.Payload[1]) {
			case txn.OpRemoveEdge, txn.OpRemoveEdgeByHandle:
				if dropAt < 0 {
					dropAt = idx
				}
			}
		}
		idx++
	}
	_ = r.Close()
	if dropAt < 0 {
		t.Fatal("no edge-removal frame in the WAL: the delete was never described durably")
	}

	// CONTROL: the same recovery, one real removal frame short, MUST differ.
	if edges := noopFrameRecoverEdges(t, dir, dropAt); len(edges) == 0 {
		t.Fatal("dropping the only real removal frame still recovered an edgeless graph: " +
			"this oracle cannot fail, so the assertion above proves nothing")
	}
}

// TestUndirectedEngine_KeepsDescribingRemovalsThatTookNothing guards the trap the
// rmp #2734 investigation walked into, and is the reason its gate is conditional.
//
// Cypher over an UNDIRECTED LPG is NOT a supported configuration: docs/cypher.md
// states Directed: true is required for openCypher semantics, cypher.NewEngine
// warns at construction (#1892), and the engine measurably miscounts there
// (one CREATE of one relationship makes `MATCH ()-[r]->() RETURN count(r)`
// report 2). This test does not bless it. It exists because the module still
// CONSTRUCTS it, and because gating the removal frame unconditionally on the
// in-memory presence probe silently turns it into DATA LOSS: RemoveAllEdgesFrom
// retires each mirror arc while emitting a frame only for the forward arc, so
// the frame that looks redundant in memory is the one that performs the deletion
// on replay. With the gate applied unconditionally this recovered a hub still
// holding all eight of its relationships.
func TestUndirectedEngine_KeepsDescribingRemovalsThatTookNothing(t *testing.T) {
	eng, w, dir := noopFrameEngine(t, false)

	noopFrameRun(t, eng, `CREATE (h:Hub {n:'hub'})`)
	for i := range 8 {
		noopFrameRun(t, eng, fmt.Sprintf(`MATCH (h:Hub) CREATE (h)<-[:R]-(:Leaf {i:%d})`, i))
	}
	res := noopFrameRun(t, eng, `MATCH (h:Hub) DETACH DELETE h`)
	if got := res.Counters().RelationshipsDeleted; got != 8 {
		t.Fatalf("relationships-deleted = %d, want 8", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	if edges := noopFrameRecoverEdges(t, dir, -1); len(edges) != 0 {
		t.Fatalf("recovery reconstructed %d edges (%v) after a DETACH DELETE that removed all 8: "+
			"a removal frame the undirected engine needs was suppressed — that is data loss, not a saving",
			len(edges), edges)
	}
}
