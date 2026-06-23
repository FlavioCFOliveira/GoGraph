package cypher_test

// byhandle_edge_prop_durability_test.go — durability + recovery coverage for
// the per-instance (by-handle) edge-property store maintained on SET / REMOVE /
// SET += / SET = on a bound relationship (#1686).
//
// The by-handle store is the per-parallel-edge property surface. Before #1686
// it was written only at CREATE; a SET on one parallel edge left it stale and
// could corrupt a sibling. #1686 makes SET/REMOVE dual-write it durably (the
// per-pair store stays authoritative for reads until #1684). These tests assert
// that, AFTER a store reopen, the by-handle property landed on exactly the SET
// instance, the sibling instance is untouched, and the per-pair coalesced view
// still matches the in-memory engine (no per-pair regression).
//
// Layer: short. The engines/graphs are local, so the suite is goleak-clean.

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// bhRecOpts / bhStoreOpts mirror the codec wiring used across the recovery
// tests (string keys, float64 weights).
func bhRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

func bhStoreOpts() txn.Options[string, float64] {
	return txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

// bhWriteCycle reopens dir, runs each query in its own committed autocommit
// transaction, then persists. When snap is true it writes a full snapshot and
// truncates the WAL (self-sufficient snapshot recovery on the next open);
// otherwise it only fsyncs the WAL (pure WAL-replay recovery).
func bhWriteCycle(t *testing.T, dir string, snap bool, queries ...string) {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, bhRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	store := txn.NewStoreWithOptions[string, float64](res.Graph, w, bhStoreOpts())
	eng := cypher.NewEngineWithStore(store)
	for _, q := range queries {
		r, err := eng.RunInTxAny(context.Background(), q, nil)
		if err != nil {
			w.Close()
			t.Fatalf("RunInTx(%q): %v", q, err)
		}
		for r.Next() { //nolint:revive // drain to run the write to completion
		}
		if rerr := r.Err(); rerr != nil {
			_ = r.Close()
			w.Close()
			t.Fatalf("result err (%q): %v", q, rerr)
		}
		if err := r.Close(); err != nil {
			w.Close()
			t.Fatalf("Close(%q): %v", q, err)
		}
	}
	if snap {
		cs := csr.BuildFromAdjList(res.Graph.AdjList())
		if err := snapshot.WriteSnapshotFullWithMapperCodec(filepath.Join(dir, "snapshot"), cs, res.Graph, txn.NewStringCodec()); err != nil {
			w.Close()
			t.Fatalf("WriteSnapshotFull: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		w.Close()
		t.Fatalf("Sync: %v", err)
	}
	if snap {
		if _, err := w.Truncate(); err != nil {
			w.Close()
			t.Fatalf("Truncate: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
}

// byHandleValuesForPair reopens dir and returns, for the ordered pair
// (:N{key:src}) -> (:N{key:dst}), one map of properties per parallel edge
// instance (keyed by stable handle). It reads the by-handle store DIRECTLY (the
// read path is not rerouted to it in #1686), enumerating live handles via
// [lpg.Graph.WalkEdgeHandles] and reading each via EdgePropertiesByHandleID.
func byHandleValuesForPair(t *testing.T, dir, src, dst string) map[uint64]map[string]lpg.PropertyValue {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, bhRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open(read): %v", err)
	}
	g := res.Graph
	srcID, ok := g.AdjList().Mapper().Lookup(keyNode(t, g, src))
	if !ok {
		t.Fatalf("src node %q not found after reopen", src)
	}
	dstID, ok := g.AdjList().Mapper().Lookup(keyNode(t, g, dst))
	if !ok {
		t.Fatalf("dst node %q not found after reopen", dst)
	}
	perInstance := make(map[uint64]map[string]lpg.PropertyValue)
	g.WalkEdgeHandles(func(tr lpg.EdgeHandleTriple) bool {
		if tr.Src == srcID && tr.Dst == dstID {
			perInstance[tr.Handle] = g.EdgePropertiesByHandleID(srcID, dstID, tr.Handle)
		}
		return true
	})
	return perInstance
}

// keyNode finds the synthetic node key whose `key` property equals val. CREATE
// assigns synthetic keys (__cx_<hex>); the user-facing identity is the `key`
// property, so the test resolves the synthetic key by scanning node properties.
func keyNode(t *testing.T, g *lpg.Graph[string, float64], val string) string {
	t.Helper()
	var found string
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, k string) bool {
		props := g.NodeProperties(k)
		if sv, ok := props["key"]; ok {
			if s, ok := sv.String(); ok && s == val {
				found = k
				return false
			}
		}
		return true
	})
	if found == "" {
		t.Fatalf("no node with key=%q", val)
	}
	return found
}

// perPairValueAfterReopen reopens dir with a read-only engine and returns the
// coalesced per-pair value of r.<key> read through Cypher (the authoritative
// read path in #1686), for the relationship of the given type between the pair.
func perPairValueAfterReopen(t *testing.T, dir, relType, key string) []string {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, bhRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open(perpair): %v", err)
	}
	q := "MATCH (:N {key:'x'})-[r:" + relType + "]->(:N {key:'y'}) RETURN r." + key + " AS v"
	r, err := cypher.NewEngine(res.Graph).RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("per-pair read: %v", err)
	}
	rows := collectRecords(t, r)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		switch sv := row["v"].(type) {
		case nil:
			out = append(out, "<null>")
		case expr.StringValue:
			out = append(out, string(sv))
		default:
			out = append(out, "<non-string>")
		}
	}
	sort.Strings(out)
	return out
}

// countByHandleWith returns how many parallel instances carry key=want, and how
// many carry the key at all, across the per-instance maps.
func countByHandleWith(perInstance map[uint64]map[string]lpg.PropertyValue, key, want string) (withWant, withKey int) {
	for _, props := range perInstance {
		v, ok := props[key]
		if !ok {
			continue
		}
		withKey++
		if s, ok := v.String(); ok && s == want {
			withWant++
		}
	}
	return withWant, withKey
}

func runByHandleSetReopen(t *testing.T, snap bool) {
	t.Helper()
	dir := t.TempDir()
	// Two endpoints, then two distinctly-typed parallel edges each carrying its
	// own CREATE-time property, each in its own reopen cycle (a
	// one-command-per-process consumer).
	bhWriteCycle(t, dir, snap, `CREATE (a:N {key:'x'})`)
	bhWriteCycle(t, dir, snap, `CREATE (b:N {key:'y'})`)
	bhWriteCycle(t, dir, snap, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:USES {w:1}]->(b)`)
	bhWriteCycle(t, dir, snap, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:CALLS {w:2}]->(b)`)

	// SET a NEW property on EXACTLY ONE instance (the USES edge).
	bhWriteCycle(t, dir, snap, `MATCH (:N {key:'x'})-[r:USES]->(:N {key:'y'}) SET r.tag = 'on-uses'`)

	// After reopen the by-handle store must carry tag='on-uses' on exactly one
	// instance, and no instance other than that one may carry tag at all.
	perInstance := byHandleValuesForPair(t, dir, "x", "y")
	if len(perInstance) != 2 {
		t.Fatalf("snap=%v: expected 2 parallel by-handle instances, got %d: %v", snap, len(perInstance), perInstance)
	}
	withWant, withKey := countByHandleWith(perInstance, "tag", "on-uses")
	if withWant != 1 || withKey != 1 {
		t.Fatalf("snap=%v: SET on one parallel edge must touch exactly one by-handle instance: tag='on-uses' on %d instances, tag present on %d instances; full=%v",
			snap, withWant, withKey, perInstance)
	}

	// The sibling (CALLS) instance must still carry its own CREATE-time prop and
	// NOT the tag — assert per-instance isolation explicitly via the CREATE prop.
	var usesHasW, callsHasW bool
	for _, props := range perInstance {
		_, hasTag := props["tag"]
		if w, ok := props["w"]; ok {
			if iv, ok := w.Int64(); ok {
				switch iv {
				case 1:
					usesHasW = true
					if !hasTag {
						t.Fatalf("snap=%v: USES instance (w=1) lost its tag after reopen: %v", snap, props)
					}
				case 2:
					callsHasW = true
					if hasTag {
						t.Fatalf("snap=%v: CALLS sibling (w=2) was corrupted with the USES tag: %v", snap, props)
					}
				}
			}
		}
	}
	if !usesHasW || !callsHasW {
		t.Fatalf("snap=%v: expected both CREATE-time w props to survive: usesHasW=%v callsHasW=%v; full=%v", snap, usesHasW, callsHasW, perInstance)
	}

	// The per-pair read path (authoritative in #1686) must still see tag on the
	// USES match (and the coalesced union is unchanged from today's behaviour).
	if got := perPairValueAfterReopen(t, dir, "USES", "tag"); len(got) != 1 || got[0] != "on-uses" {
		t.Fatalf("snap=%v: per-pair read of USES.tag after reopen = %v, want [on-uses]", snap, got)
	}
}

// TestByHandleEdgeProp_SET_WALReplay verifies that a SET on one of two parallel
// edges maintains the by-handle store on exactly that instance and that the
// per-instance state survives pure WAL-replay recovery.
func TestByHandleEdgeProp_SET_WALReplay(t *testing.T) {
	t.Parallel()
	runByHandleSetReopen(t, false)
}

// TestByHandleEdgeProp_SET_Snapshot verifies the same across the self-sufficient
// snapshot recovery path (snapshot + WAL truncate), then a further SET appended
// to the WAL tail (last-writer-wins).
func TestByHandleEdgeProp_SET_Snapshot(t *testing.T) {
	t.Parallel()
	runByHandleSetReopen(t, true)
}

// TestByHandleEdgeProp_SET_SnapshotThenTail checkpoints after the first SET,
// appends a SECOND SET on the same instance, reopens, and asserts last-writer-
// wins on the by-handle store across snapshot + WAL-tail.
func TestByHandleEdgeProp_SET_SnapshotThenTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bhWriteCycle(t, dir, true, `CREATE (a:N {key:'x'})`)
	bhWriteCycle(t, dir, true, `CREATE (b:N {key:'y'})`)
	bhWriteCycle(t, dir, true, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:USES {w:1}]->(b)`)
	bhWriteCycle(t, dir, true, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:CALLS {w:2}]->(b)`)
	// First SET is checkpointed into the snapshot (snap=true truncates the WAL).
	bhWriteCycle(t, dir, true, `MATCH (:N {key:'x'})-[r:USES]->(:N {key:'y'}) SET r.tag = 'v1'`)
	// Second SET rides the WAL tail only (snap=false: no checkpoint).
	bhWriteCycle(t, dir, false, `MATCH (:N {key:'x'})-[r:USES]->(:N {key:'y'}) SET r.tag = 'v2'`)

	perInstance := byHandleValuesForPair(t, dir, "x", "y")
	withV2, withKey := countByHandleWith(perInstance, "tag", "v2")
	if withV2 != 1 || withKey != 1 {
		t.Fatalf("last-writer-wins across snapshot+tail failed: tag='v2' on %d, tag present on %d; full=%v", withV2, withKey, perInstance)
	}
}

func runByHandleRemoveReopen(t *testing.T, viaSetNull, snap bool) {
	t.Helper()
	dir := t.TempDir()
	bhWriteCycle(t, dir, snap, `CREATE (a:N {key:'x'})`)
	bhWriteCycle(t, dir, snap, `CREATE (b:N {key:'y'})`)
	// Both parallel edges carry tag at CREATE; the by-handle store gets it via
	// the CREATE path.
	bhWriteCycle(t, dir, snap, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:USES {w:1, tag:'keep'}]->(b)`)
	bhWriteCycle(t, dir, snap, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:CALLS {w:2, tag:'keep'}]->(b)`)

	// Remove tag from EXACTLY the USES instance (via REMOVE or SET = null).
	stmt := `MATCH (:N {key:'x'})-[r:USES]->(:N {key:'y'}) REMOVE r.tag`
	if viaSetNull {
		stmt = `MATCH (:N {key:'x'})-[r:USES]->(:N {key:'y'}) SET r.tag = null`
	}
	bhWriteCycle(t, dir, snap, stmt)

	perInstance := byHandleValuesForPair(t, dir, "x", "y")
	if len(perInstance) != 2 {
		t.Fatalf("expected 2 parallel by-handle instances, got %d: %v", len(perInstance), perInstance)
	}
	// Exactly one instance still carries tag='keep' (the CALLS sibling); the
	// USES instance lost it. This exercises OpDelEdgePropertyByHandle durability.
	_, withKey := countByHandleWith(perInstance, "tag", "keep")
	if withKey != 1 {
		t.Fatalf("viaSetNull=%v snap=%v: REMOVE on one parallel edge must leave tag on exactly the sibling: tag present on %d instances; full=%v",
			viaSetNull, snap, withKey, perInstance)
	}
	// The instance that lost tag must be the one with w=1 (USES).
	for _, props := range perInstance {
		if w, ok := props["w"]; ok {
			if iv, ok := w.Int64(); ok && iv == 1 {
				if _, hasTag := props["tag"]; hasTag {
					t.Fatalf("viaSetNull=%v snap=%v: USES instance (w=1) still carries tag after removal: %v", viaSetNull, snap, props)
				}
			}
		}
	}
}

// TestByHandleEdgeProp_REMOVE_WALReplay exercises OpDelEdgePropertyByHandle over
// pure WAL-replay recovery.
func TestByHandleEdgeProp_REMOVE_WALReplay(t *testing.T) {
	t.Parallel()
	runByHandleRemoveReopen(t, false, false)
}

// TestByHandleEdgeProp_REMOVE_Snapshot exercises it over snapshot recovery.
func TestByHandleEdgeProp_REMOVE_Snapshot(t *testing.T) {
	t.Parallel()
	runByHandleRemoveReopen(t, false, true)
}

// TestByHandleEdgeProp_SetNull_WALReplay exercises SET r.tag = null (the REMOVE
// equivalent) over pure WAL-replay recovery.
func TestByHandleEdgeProp_SetNull_WALReplay(t *testing.T) {
	t.Parallel()
	runByHandleRemoveReopen(t, true, false)
}

// runByHandleMergeOnMatchReopen mirrors runByHandleSetReopen but mutates the
// matched edge via MERGE ... ON MATCH SET rather than a bare SET clause. The
// MERGE ON MATCH / ON CREATE action path mirrors its per-pair property writes
// onto the matched edge's by-handle store (#1684, the read-routing's foundation
// in the MERGE path); this asserts that mutation is durable — it survives a store
// reopen and lands on exactly the matched parallel instance, leaving the sibling
// untouched. The by-handle write reuses #1686's OpSetEdgePropertyByHandle WAL op,
// so this also exercises that op being emitted from the MERGE path.
func runByHandleMergeOnMatchReopen(t *testing.T, snap bool) {
	t.Helper()
	dir := t.TempDir()
	bhWriteCycle(t, dir, snap, `CREATE (a:N {key:'x'})`)
	bhWriteCycle(t, dir, snap, `CREATE (b:N {key:'y'})`)
	bhWriteCycle(t, dir, snap, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:USES {w:1}]->(b)`)
	bhWriteCycle(t, dir, snap, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:CALLS {w:2}]->(b)`)

	// MERGE matches the existing USES edge and mutates it via ON MATCH SET; the
	// by-handle mirror must land on that instance only.
	bhWriteCycle(t, dir, snap, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) MERGE (a)-[r:USES]->(b) ON MATCH SET r.tag = 'via-merge'`)

	perInstance := byHandleValuesForPair(t, dir, "x", "y")
	if len(perInstance) != 2 {
		t.Fatalf("snap=%v: expected 2 parallel by-handle instances, got %d: %v", snap, len(perInstance), perInstance)
	}
	withWant, withKey := countByHandleWith(perInstance, "tag", "via-merge")
	if withWant != 1 || withKey != 1 {
		t.Fatalf("snap=%v: MERGE ON MATCH SET must touch exactly one by-handle instance: tag='via-merge' on %d, present on %d; full=%v",
			snap, withWant, withKey, perInstance)
	}
	// The CALLS sibling must keep its CREATE-time w and never gain the tag.
	for _, props := range perInstance {
		if w, ok := props["w"]; ok {
			if iv, ok := w.Int64(); ok && iv == 2 {
				if _, hasTag := props["tag"]; hasTag {
					t.Fatalf("snap=%v: CALLS sibling corrupted with the MERGE tag: %v", snap, props)
				}
			}
		}
	}
	// Per-pair read (authoritative) must also see the tag on the USES match.
	if got := perPairValueAfterReopen(t, dir, "USES", "tag"); len(got) != 1 || got[0] != "via-merge" {
		t.Fatalf("snap=%v: per-pair read of USES.tag after reopen = %v, want [via-merge]", snap, got)
	}
}

// TestByHandleEdgeProp_MergeOnMatch_WALReplay verifies a MERGE ON MATCH SET on
// one of two parallel edges maintains the by-handle store on exactly that
// instance and survives pure WAL-replay recovery (#1684 MERGE-path dual-write).
func TestByHandleEdgeProp_MergeOnMatch_WALReplay(t *testing.T) {
	t.Parallel()
	runByHandleMergeOnMatchReopen(t, false)
}

// TestByHandleEdgeProp_MergeOnMatch_Snapshot verifies the same across the
// self-sufficient snapshot recovery path.
func TestByHandleEdgeProp_MergeOnMatch_Snapshot(t *testing.T) {
	t.Parallel()
	runByHandleMergeOnMatchReopen(t, true)
}
