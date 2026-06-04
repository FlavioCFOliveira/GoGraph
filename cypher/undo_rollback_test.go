package cypher_test

// undo_rollback_test.go — regression tests for task #1282: a Cypher write query
// that errors or panics mid-pipeline must leave NO partial mutation visible in
// the live in-memory graph, because rows already applied before the failure are
// eagerly written under the visibility barrier. Before the fix, only the WAL
// transaction and the secondary-index buffer rolled back; the in-memory graph
// stayed dirty (in-memory-vs-durable divergence) until the process restarted —
// an Atomicity violation observable by concurrent View readers and the next
// query.
//
// These tests drive the PUBLIC Cypher engine and assert the live graph is clean
// after a failed write, plus (for the AC) that a fresh recovery.Open of the WAL
// observes none of the partial writes.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// nthSetRejector is a lpg.SchemaValidator that rejects the Nth Validate call for
// the named property key, allowing every other write. It is the deterministic
// mid-pipeline failure seam the AC requires: installed after seeding, it lets a
// 5-row `SET n.x = …` apply rows 1..2 and fail on row 3.
//
// It is safe for concurrent use (the count is atomic), though the engine drives
// SET serially under the write barrier.
type nthSetRejector struct {
	key   string
	rejN  int64
	count atomic.Int64
}

func (v *nthSetRejector) Validate(propertyName string, _ lpg.PropertyValue) error {
	if propertyName != v.key {
		return nil
	}
	if v.count.Add(1) == v.rejN {
		return fmt.Errorf("nthSetRejector: rejecting %s write #%d", v.key, v.rejN)
	}
	return nil
}

// walEngineWithGraph builds a WAL-backed engine over a fresh directed graph and
// returns the engine, the graph (so a test can install a validator or inspect
// it directly), the WAL writer, and the directory holding the WAL. The writer is
// NOT auto-closed: the recovery tests close it explicitly before reopening.
func walEngineWithGraph(t *testing.T) (*cypher.Engine, *lpg.Graph[string, float64], *wal.Writer, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return cypher.NewEngineWithStore(store), g, w, dir
}

// walMultigraphEngineWithGraph mirrors [walEngineWithGraph] but builds the
// graph with adjlist.Config.Multigraph enabled — the openCypher TCK storage
// model, in which each CREATE of a relationship between the same endpoints
// becomes a distinct parallel slot carrying its own stable handle and its own
// per-instance type/properties. The multigraph mode is what makes the
// "remove one parallel edge, then fail a later row" interleaving of #1327
// reachable; a simple graph collapses parallel CREATEs onto one slot.
func walMultigraphEngineWithGraph(t *testing.T) (*cypher.Engine, *lpg.Graph[string, float64], *wal.Writer, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return cypher.NewEngineWithStore(store), g, w, dir
}

// runWrite runs a write query to completion and returns the error surfaced by
// the result (drain error preferred, else Close error). A successful write
// returns nil.
func runWrite(t *testing.T, eng *cypher.Engine, query string) error {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		return err
	}
	for res.Next() { //nolint:revive // intentional full drain
	}
	drainErr := res.Err()
	closeErr := res.Close()
	if drainErr != nil {
		return drainErr
	}
	return closeErr
}

// recOpts returns recovery options matching the store codecs.
func recOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// TestRunInTx_ErrorRollsBackInMemoryMutations is the acceptance-criteria test
// for #1282. A SchemaValidator rejects the 3rd SET; `MATCH (n) SET n.x=1` over 5
// nodes must error AND leave x unset on ALL 5 nodes in the live in-memory graph,
// while a fresh recovery.Open of the WAL observes none.
func TestRunInTx_ErrorRollsBackInMemoryMutations(t *testing.T) {
	eng, g, w, dir := walEngineWithGraph(t)

	// Seed five nodes n0..n4 with no x property. Done before the validator is
	// installed so the seeding writes are not counted by the rejector.
	for i := 0; i < 5; i++ {
		if err := runWrite(t, eng, fmt.Sprintf("CREATE (:Item {id:%d})", i)); err != nil {
			t.Fatalf("seed CREATE %d: %v", i, err)
		}
	}

	// Install the rejector: reject the 3rd `x` write of the failing statement.
	g.SetValidator(&nthSetRejector{key: "x", rejN: 3})

	// The failing write: set x on all five matched nodes. Rows 1..2 apply
	// eagerly; row 3 is rejected by the validator, so the statement errors.
	err := runWrite(t, eng, "MATCH (n:Item) SET n.x = 99")
	if err == nil {
		t.Fatal("expected the write to error on the 3rd SET, got nil")
	}

	// (1) The live in-memory graph must show x UNSET on all five nodes: the two
	// rows applied before the rejection were rolled back inside the barrier.
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		if _, ok := g.GetNodeProperty(key, "x"); ok {
			t.Errorf("node %q still has x set after rollback (in-memory divergence)", key)
		}
		return true
	})

	// Drop the validator so the checkpoint/close path is unconstrained, then
	// flush the WAL and reopen from disk.
	g.SetValidator(nil)
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// (2) A fresh recovery from the WAL must observe NO node carrying x: the
	// failed statement never committed, so its (rolled-back) writes are not in
	// the log.
	res, err := recovery.Open[string, float64](dir, recOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	rg := res.Graph
	withX := 0
	rg.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		if _, ok := rg.GetNodeProperty(key, "x"); ok {
			withX++
		}
		return true
	})
	if withX != 0 {
		t.Errorf("recovered graph has %d nodes with x set, want 0 (durable divergence)", withX)
	}
}

// TestRunInTx_PanicRollsBackInMemoryMutationsAndReleasesWriter is the panic
// variant. A write that panics mid-pipeline (after applying earlier rows) must
//
//	(a) leave NO partial in-memory mutation, and
//	(b) release the store's single-writer mutex (proven by a subsequent write
//	    completing under a watchdog).
//
// Run under -race to confirm the in-barrier undo replay does not race a reader.
func TestRunInTx_PanicRollsBackInMemoryMutationsAndReleasesWriter(t *testing.T) {
	quietLogs(t)
	eng, g, w, _ := walEngineWithGraph(t)
	t.Cleanup(func() { _ = w.Close() })

	// Seed two nodes a and b.
	if err := runWrite(t, eng, "CREATE (:N {name:'a'})"); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := runWrite(t, eng, "CREATE (:N {name:'b'})"); err != nil {
		t.Fatalf("seed b: %v", err)
	}

	// boom() (registered in panic_boundary_test.go init) panics during exec.
	// Setting it as a property value drives the panic AFTER the SET operator has
	// bound the row, i.e. mid-pipeline. We also set a plain property on a
	// matched node first via a separate clause so a partial mutation would be
	// observable if the rollback were missing.
	err := runWrite(t, eng, "MATCH (n:N) SET n.touched = 1, n.bad = boom()")
	if err == nil {
		t.Fatal("expected panic-converted error, got nil")
	}
	if !errors.Is(err, cypher.ErrInternalPanic) {
		t.Fatalf("error %v does not wrap ErrInternalPanic", err)
	}

	// (a) No node may carry `touched` or `bad`: the panic mid-statement rolled
	// the eager writes back inside the barrier.
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		if _, ok := g.GetNodeProperty(key, "touched"); ok {
			t.Errorf("node %q still has touched set after panic rollback", key)
		}
		if _, ok := g.GetNodeProperty(key, "bad"); ok {
			t.Errorf("node %q still has bad set after panic rollback", key)
		}
		return true
	})

	// (b) The single-writer mutex must have been released on the panic path. A
	// subsequent ordinary write must complete; a leaked mutex would deadlock
	// RunInTx's Begin, so the watchdog fails deterministically.
	done := make(chan error, 1)
	go func() { done <- runWrite(t, eng, "CREATE (:N {name:'c'})") }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("subsequent write failed: %v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("subsequent write deadlocked: single-writer mutex leaked on panic path")
	}
}

// TestRunInTx_PartialCreateRollsBackNodesEdgesLabelsProps covers the structural
// case: a CREATE that builds nodes, an edge, labels, and properties and then
// errors on a later property write must leave the graph completely empty of the
// partial structure.
func TestRunInTx_PartialCreateRollsBackNodesEdgesLabelsProps(t *testing.T) {
	eng, g, w, _ := walEngineWithGraph(t)
	t.Cleanup(func() { _ = w.Close() })

	// Reject the write of property key "trip" so a CREATE that builds
	// (a:A)-[:R {trip:…}]->(b:B) fails AFTER the nodes, the edge, and the labels
	// are applied (the relationship property is written last).
	g.SetValidator(&nthSetRejector{key: "trip", rejN: 1})

	err := runWrite(t, eng, "CREATE (a:A {name:'x'})-[:R {trip:1}]->(b:B {name:'y'})")
	if err == nil {
		t.Fatal("expected the CREATE to error on the rejected edge property, got nil")
	}

	// The whole structure must be gone: no live nodes, no edge.
	g.SetValidator(nil)
	live := 0
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		if id, ok := g.AdjList().Mapper().Lookup(key); ok && !g.IsTombstoned(id) {
			live++
		}
		return true
	})
	if live != 0 {
		t.Errorf("live node count = %d after rollback, want 0", live)
	}
	if g.AdjList().HasEdge("a", "b") {
		t.Error("edge a->b still present after rollback")
	}
	// Side-effect counters must be back to zero: a rolled-back CREATE must not
	// inflate the per-query +nodes / +relationships the TCK asserts.
	na, nr, ea, er := g.SideEffectCounters()
	if na != 0 || nr != 0 || ea != 0 || er != 0 {
		t.Errorf("side-effect counters = (na=%d nr=%d ea=%d er=%d), want all 0", na, nr, ea, er)
	}
}

// TestRunInTx_DeleteThenFailRestoresEdge proves the per-pair edge restore. A
// statement deletes an edge on an early row and then fails on a later row; the
// edge — together with its weight, per-pair label, and per-pair properties —
// must be restored exactly as it was before the DELETE.
//
// Nodes are keyed internally by synthetic identifiers (not the Cypher variable
// or the `name` property), so the test resolves the real endpoint keys from the
// graph and compares the restored edge against the pre-delete snapshot captured
// against those keys, rather than hard-coding endpoint names or a weight.
func TestRunInTx_DeleteThenFailRestoresEdge(t *testing.T) {
	eng, g, w, _ := walEngineWithGraph(t)
	t.Cleanup(func() { _ = w.Close() })

	// Seed a-[:LINK {w:7}]->b plus a node c to give the failing statement a
	// second matched row to reach after the DELETE.
	if err := runWrite(t, eng, "CREATE (a:N {name:'a'})-[:LINK {w:7}]->(b:N {name:'b'})"); err != nil {
		t.Fatalf("seed edge: %v", err)
	}
	if err := runWrite(t, eng, "CREATE (:N {name:'c'})"); err != nil {
		t.Fatalf("seed c: %v", err)
	}

	// Resolve the real endpoint keys of the single seeded edge.
	srcKey, dstKey, found := findOneEdge(g)
	if !found {
		t.Fatal("precondition: a seeded edge should exist")
	}
	wantW, _ := g.EdgeWeight(srcKey, dstKey)
	wantLabels := g.EdgeLabels(srcKey, dstKey)
	wantProps := g.EdgeProperties(srcKey, dstKey)

	// A statement that DELETEs the relationship, then sets a property whose key
	// the validator rejects — forcing a failure AFTER the delete applied.
	g.SetValidator(&nthSetRejector{key: "boom", rejN: 1})
	err := runWrite(t, eng,
		"MATCH (a:N {name:'a'})-[r:LINK]->(b:N {name:'b'}) DELETE r WITH a MATCH (n:N) SET n.boom = 1")
	if err == nil {
		t.Fatal("expected the statement to error after the DELETE, got nil")
	}

	g.SetValidator(nil)
	// The edge must be restored at the same endpoints.
	if !g.AdjList().HasEdge(srcKey, dstKey) {
		t.Fatal("edge was not restored after DELETE-then-fail rollback")
	}
	if gotW, ok := g.EdgeWeight(srcKey, dstKey); !ok || gotW != wantW {
		t.Errorf("restored edge weight = %v (ok=%v), want %v", gotW, ok, wantW)
	}
	if got := g.EdgeLabels(srcKey, dstKey); !sameStringSet(got, wantLabels) {
		t.Errorf("restored edge labels = %v, want %v", got, wantLabels)
	}
	gotProps := g.EdgeProperties(srcKey, dstKey)
	if len(gotProps) != len(wantProps) {
		t.Errorf("restored edge has %d properties, want %d (%v vs %v)", len(gotProps), len(wantProps), gotProps, wantProps)
	}
	for k, want := range wantProps {
		got, ok := gotProps[k]
		if !ok {
			t.Errorf("restored edge missing property %q", k)
			continue
		}
		if got.Kind() != want.Kind() {
			t.Errorf("restored edge property %q kind = %v, want %v", k, got.Kind(), want.Kind())
		}
	}
}

// TestRunInTx_ParallelEdgeRemovalThenFailRestoresPerHandleMetadata is the
// acceptance-criteria test for #1327: the exotic multigraph removal-then-fail
// interleaving. Two parallel (a)-[:R…]->(b) instances carry DISTINCT per-handle
// types and properties. A RunInTx removes ONE instance on an early row, then
// fails on a later row. On rollback BOTH the surviving instance and the removed
// instance's per-handle type+properties must be fully restored — the removed
// instance must come back with its ORIGINAL stable handle so the handle-keyed
// read path resolves it to its own type, not to a surviving sibling.
//
// Before the #1327 fix the edge-removal undo re-added the instance with a plain
// AddEdge (handle 0); the per-handle store survived but no live adjacency slot
// carried the removed handle, so `type(r)` mis-mapped the re-added instance to
// the survivor — an Atomicity gap: the rollback left a state that neither fully
// contained nor fully excluded the removed instance.
func TestRunInTx_ParallelEdgeRemovalThenFailRestoresPerHandleMetadata(t *testing.T) {
	eng, g, w, _ := walMultigraphEngineWithGraph(t)
	t.Cleanup(func() { _ = w.Close() })

	// Seed two distinctly-typed parallel edges between the same ordered pair,
	// each with its own property value, plus a node c so the failing statement
	// has a later matched row to reach after the DELETE.
	if _, err := eng.RunInTx(context.Background(),
		`CREATE (a:N {key:'x'}),(b:N {key:'y'}),`+
			`(a)-[:R1 {p:1}]->(b),(a)-[:R2 {p:2}]->(b)`, nil); err != nil {
		t.Fatalf("seed parallels: %v", err)
	}
	if err := runWrite(t, eng, "CREATE (:N {key:'c'})"); err != nil {
		t.Fatalf("seed c: %v", err)
	}

	srcKey, dstKey, ok := findEdgeBetweenKeys(g, "x", "y")
	if !ok {
		t.Fatal("precondition: the x→y parallel edges should exist")
	}

	// Snapshot the pre-delete per-handle metadata for the pair: handle → type,
	// handle → p. This is the authoritative per-instance surface in multigraph
	// mode; the rollback must reproduce it exactly.
	wantByHandle := snapshotHandleMeta(g, srcKey, dstKey)
	if len(wantByHandle) != 2 {
		t.Fatalf("precondition: want 2 distinct handles, got %d (%v)", len(wantByHandle), wantByHandle)
	}
	wantCreateCount := g.EdgeCreateCount(srcKey, dstKey)

	// Remove the :R1 instance on an early row, then fail on a later row: the
	// validator rejects the `boom` write the second MATCH issues.
	g.SetValidator(&nthSetRejector{key: "boom", rejN: 1})
	err := runWrite(t, eng,
		"MATCH (:N {key:'x'})-[r:R1]->(:N {key:'y'}) DELETE r WITH 1 AS _x MATCH (n:N) SET n.boom = 1")
	if err == nil {
		t.Fatal("expected the statement to error after removing the R1 instance, got nil")
	}
	g.SetValidator(nil)

	// (1) Both parallel slots must be live again with their original handles
	// and per-handle metadata — the removed instance fully restored, the
	// survivor untouched.
	gotByHandle := snapshotHandleMeta(g, srcKey, dstKey)
	if len(gotByHandle) != len(wantByHandle) {
		t.Fatalf("after rollback: %d live handles, want %d (want=%v got=%v)",
			len(gotByHandle), len(wantByHandle), wantByHandle, gotByHandle)
	}
	for h, want := range wantByHandle {
		got, present := gotByHandle[h]
		if !present {
			t.Errorf("removed instance handle %d not restored on rollback", h)
			continue
		}
		if got.typ != want.typ {
			t.Errorf("handle %d type = %q, want %q", h, got.typ, want.typ)
		}
		if got.p != want.p {
			t.Errorf("handle %d property p = %d, want %d", h, got.p, want.p)
		}
	}

	// (2) The handle-keyed read path must surface BOTH distinct types again.
	// Pre-fix this returned the survivor's type twice.
	if gotTypes := readParallelTypes(t, eng); len(gotTypes) != 2 ||
		gotTypes[0] != "R1" || gotTypes[1] != "R2" {
		t.Errorf("type(r) over parallels = %v, want [R1 R2] (handle path mis-mapped)", gotTypes)
	}

	// (3) The per-pair union must be intact: both types present, edge live, and
	// the CREATE-multiplicity counter back at its pre-delete value.
	if !g.AdjList().HasEdge(srcKey, dstKey) {
		t.Fatal("pair has no live edge after rollback")
	}
	if got := g.EdgeCreateCount(srcKey, dstKey); got != wantCreateCount {
		t.Errorf("EdgeCreateCount = %d after rollback, want %d", got, wantCreateCount)
	}
	if unionLabels := g.EdgeLabels(srcKey, dstKey); !sameStringSet(unionLabels, []string{"R1", "R2"}) {
		t.Errorf("per-pair union labels = %v, want {R1, R2}", unionLabels)
	}
}

// handleMeta is the per-handle metadata snapshot the parallel-edge rollback
// test compares: the relationship type and the integer `p` property recorded
// under one stable handle.
type handleMeta struct {
	typ string
	p   int64
}

// snapshotHandleMeta returns, for the LIVE directed edges between srcKey and
// dstKey, a map from each slot's stable handle to its per-handle type and `p`
// property. It walks only handles that are live in the adjacency (via
// [lpg.Graph.WalkEdgeHandles]), so it captures exactly the parallel instances
// the read path can resolve — the property #1327 must preserve across a
// removal-then-fail rollback.
func snapshotHandleMeta(g *lpg.Graph[string, float64], srcKey, dstKey string) map[uint64]handleMeta {
	srcID, _ := g.AdjList().Mapper().Lookup(srcKey)
	dstID, _ := g.AdjList().Mapper().Lookup(dstKey)
	out := make(map[uint64]handleMeta)
	g.WalkEdgeHandles(func(tr lpg.EdgeHandleTriple) bool {
		if tr.Src != srcID || tr.Dst != dstID {
			return true
		}
		m := handleMeta{}
		if labels := g.EdgeLabelsByHandle(srcKey, dstKey, tr.Handle); len(labels) > 0 {
			m.typ = labels[0]
		}
		if props := g.EdgePropertiesByHandle(srcKey, dstKey, tr.Handle); props != nil {
			if pv, ok := props["p"]; ok {
				if i, ok := pv.Int64(); ok {
					m.p = i
				}
			}
		}
		out[tr.Handle] = m
		return true
	})
	return out
}

// readParallelTypes runs MATCH (x)-[r]->(y) RETURN type(r) and returns the
// sorted relationship types, so the test can assert the handle-keyed read path
// surfaces every restored parallel instance's own type.
func readParallelTypes(t *testing.T, eng *cypher.Engine) []string {
	t.Helper()
	res, err := eng.Run(context.Background(),
		`MATCH (:N {key:'x'})-[r]->(:N {key:'y'}) RETURN type(r) AS t`, nil)
	if err != nil {
		t.Fatalf("read parallel types: %v", err)
	}
	recs := drainRecords(t, res)
	types := make([]string, 0, len(recs))
	for _, row := range recs {
		if s, ok := row["t"].(expr.StringValue); ok {
			types = append(types, string(s))
		}
	}
	sort.Strings(types)
	return types
}

// findEdgeBetweenKeys resolves the synthetic node keys of the nodes whose
// `key` property equals srcVal and dstVal, and reports whether a directed edge
// connects them. Used to address the multigraph parallel edges by their real
// internal keys (the synthetic identifiers, not the Cypher variables).
func findEdgeBetweenKeys(g *lpg.Graph[string, float64], srcVal, dstVal string) (srcKey, dstKey string, found bool) {
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		if v, ok := g.GetNodeProperty(key, "key"); ok {
			if s, _ := v.String(); s == srcVal {
				srcKey = key
			} else if s == dstVal {
				dstKey = key
			}
		}
		return true
	})
	if srcKey == "" || dstKey == "" {
		return "", "", false
	}
	return srcKey, dstKey, g.AdjList().HasEdge(srcKey, dstKey)
}

// TestRunInTx_FailedStatementPreservesPreexistingState guards the idempotent-
// set undo path: a statement that re-asserts a label / property the target
// ALREADY carried (`SET n:Tag` on a node that already has :Tag — an idempotent
// no-op) and then fails on a later row must NOT strip that pre-existing label on
// rollback. The undo only reverts what the statement actually changed, so a
// no-op re-set records nothing. Without the hadLabel guard the rollback would
// wrongly detach the pre-existing :Tag (over-revert).
func TestRunInTx_FailedStatementPreservesPreexistingState(t *testing.T) {
	eng, g, w, _ := walEngineWithGraph(t)
	t.Cleanup(func() { _ = w.Close() })

	// Seed a node carrying label :Tag and property kept=1, plus a second node so
	// the failing statement has a later row to reach.
	if err := runWrite(t, eng, "CREATE (:Tag {name:'keep', kept:1})"); err != nil {
		t.Fatalf("seed tagged node: %v", err)
	}
	if err := runWrite(t, eng, "CREATE (:Tag {name:'other'})"); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	// A statement that re-applies the :Tag label (idempotent, since the node
	// already has it) and touches it, then fails: the validator rejects the
	// `bad` write on a later matched row. The pre-existing :Tag and kept must
	// survive; the statement's own additions (touched, bad) must not.
	g.SetValidator(&nthSetRejector{key: "bad", rejN: 1})
	err := runWrite(t, eng,
		"MATCH (n:Tag {name:'keep'}) SET n:Tag, n.touched = 1 WITH n MATCH (m:Tag) SET m.bad = 1")
	if err == nil {
		t.Fatal("expected the statement to error on the rejected SET, got nil")
	}

	g.SetValidator(nil)
	// Locate the seeded 'keep' node and assert its pre-existing state is intact
	// and the statement's own additions (touched, bad) are gone.
	var keepKey string
	g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if v, ok := g.GetNodeProperty(key, "name"); ok {
			if s, _ := v.String(); s == "keep" {
				keepKey = key
				return false
			}
		}
		return true
	})
	if keepKey == "" {
		t.Fatal("seeded 'keep' node not found after rollback")
	}
	if !g.HasNodeLabel(keepKey, "Tag") {
		t.Error("pre-existing label :Tag was stripped on rollback (over-revert)")
	}
	if _, ok := g.GetNodeProperty(keepKey, "kept"); !ok {
		t.Error("pre-existing property kept was stripped on rollback (over-revert)")
	}
	if _, ok := g.GetNodeProperty(keepKey, "touched"); ok {
		t.Error("statement-added property touched survived rollback")
	}
	if _, ok := g.GetNodeProperty(keepKey, "bad"); ok {
		t.Error("statement-added property bad survived rollback")
	}
}

// findOneEdge returns the synthetic keys of the first directed edge it finds in
// g, plus a found flag. Used by the DELETE-then-fail test to address the edge by
// its real internal keys.
func findOneEdge(g *lpg.Graph[string, float64]) (src, dst string, found bool) {
	g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		nbs, _ := g.AdjList().LoadEntry(id)
		if len(nbs) == 0 {
			return true
		}
		dstKey, ok := g.AdjList().Mapper().Resolve(nbs[0])
		if !ok {
			return true
		}
		src, dst, found = key, dstKey, true
		return false // stop at the first edge
	})
	return src, dst, found
}

// sameStringSet reports whether a and b contain the same elements (order
// independent). Used to compare unordered label slices.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
		if m[s] < 0 {
			return false
		}
	}
	return true
}
