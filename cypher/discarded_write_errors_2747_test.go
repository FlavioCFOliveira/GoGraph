package cypher_test

// discarded_write_errors_2747_test.go — regression battery for rmp #2747.
//
// walMutatorAdapter buffers every mutation into the WAL transaction with a call
// on a.tx whose error it discarded at eighteen sites, under a comment asserting
// only ErrTxFinished could occur. Two independent defects follow from that, and
// each has its own test here.
//
//  1. TestRelationshipDurability_NoWeightCodec_2747 — a LIVE ACID Durability
//     breach. [txn.Tx.AddEdgeWithHandle] refuses with ErrNoWeightCodec whenever
//     the store has no weight codec, for ANY weight. An engine built by
//     NewEngineWithStore over a txn.NewStoreWithCodec store — the wiring
//     store/db.go's own doc comment recommended — therefore had every
//     relationship refused, the refusal discarded, and the commit acknowledged.
//     Nodes, labels and properties recovered; the relationship did not.
//
//  2. TestOverlongLabelRefusedBeforeInMemoryWrite_2747 — the field bound is now
//     refused at the API instead of only at the encoder. rmp #2742 had to put
//     its guard at the encoder precisely BECAUSE these sites discarded, and said
//     so in txn.go. With the errors propagated the refusal moves ahead of the
//     in-memory write, which matters for two reasons the test asserts directly.
//
// Both tests carry a positive control, so neither can pass on a harness that
// observes nothing.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// uint16FieldCap2747 is the largest byte length a WAL uint16 length prefix can
// carry. A label of exactly this length must still work; one byte more must be
// refused. It mirrors uint16PrefixMax in store/txn's own #2742 battery, and is
// restated here because that constant is package-private to store/txn_test.
const uint16FieldCap2747 = 1<<16 - 1

// createRelQuery2747 is the one relationship-creating statement both arms of the
// durability test run, so the arms differ ONLY in the store's weight codec.
const createRelQuery2747 = `CREATE (a:Person {name: 'alice'})-[:KNOWS {since: 2020}]->(b:Person {name: 'bob'})`

// runToCompletion2747 runs a write statement and returns the first error it
// surfaces, whether from the call, the drain, or the commit on Close.
func runToCompletion2747(t *testing.T, eng *cypher.Engine, query string) error {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		return err
	}
	for res.Next() { // deliberate full drain
	}
	drainErr := res.Err()
	closeErr := res.Close()
	if drainErr != nil {
		return drainErr
	}
	return closeErr
}

// recoveredGraph2747 closes the WAL and replays dir, returning the graph that
// survived. Recovery is the only witness that decides whether a commit was
// honest: it reads what is durable, not what the writer believes it wrote.
func recoveredGraph2747(t *testing.T, w *wal.Writer, dir string) *lpg.Graph[string, float64] {
	t.Helper()
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	return res.Graph
}

// endpointKeys2747 returns the two internal node keys the CREATE interned, in
// creation order. The Cypher engine mints synthetic keys (__cx_N) for anonymous
// pattern nodes, so a test cannot name them literally; it must read them back
// from the mapper of the graph under inspection.
func endpointKeys2747(t *testing.T, g *lpg.Graph[string, float64]) []string {
	t.Helper()
	var keys []string
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		keys = append(keys, key)
		return true
	})
	return keys
}

// anyEdgeBetween2747 reports whether ANY directed edge exists among the keys.
func anyEdgeBetween2747(g *lpg.Graph[string, float64], keys []string) bool {
	for _, src := range keys {
		for _, dst := range keys {
			if src != dst && g.AdjList().HasEdge(src, dst) {
				return true
			}
		}
	}
	return false
}

// TestRelationshipDurability_NoWeightCodec_2747 is the failing-on-HEAD
// regression for the discarded ErrNoWeightCodec.
//
// Gate invariant:
//   - BEFORE the fix, the weightless arm returns nil from RunInTx (the refusal
//     is discarded at cypher/api.go's AddEdgeH site), the commit is
//     acknowledged, and the recovered graph holds both nodes with their labels
//     and properties but NO relationship — an acknowledged commit that lost
//     data, breaching ACID Durability.
//   - AFTER the fix, the refusal propagates: the statement fails with an error
//     wrapping txn.ErrNoWeightCodec, and nothing is acknowledged.
//
// The weighted arm is the positive control. It proves the harness can observe a
// relationship surviving recovery at all, so "no relationship recovered" in the
// weightless arm is a real absence and not a recovery that returned nothing.
func TestRelationshipDurability_NoWeightCodec_2747(t *testing.T) {
	t.Parallel()

	t.Run("control_weighted_store_relationship_survives", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		st := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		})
		eng := cypher.NewEngineWithStore(st)

		if err := runToCompletion2747(t, eng, createRelQuery2747); err != nil {
			t.Fatalf("control: a store WITH a weight codec must accept the CREATE: %v", err)
		}
		keys := endpointKeys2747(t, g)
		if !anyEdgeBetween2747(g, keys) {
			t.Fatalf("control: the CREATE did not produce an in-memory relationship (keys %q)", keys)
		}

		rg := recoveredGraph2747(t, w, dir)
		if got := len(endpointKeys2747(t, rg)); got != 2 {
			t.Fatalf("control: recovered %d nodes, want 2", got)
		}
		if !anyEdgeBetween2747(rg, endpointKeys2747(t, rg)) {
			t.Fatal("control: the relationship did NOT survive recovery, so this harness cannot witness one; " +
				"the weightless arm's absence assertion would be vacuous")
		}
	})

	t.Run("weightless_store_must_refuse_not_lose", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		// NewStoreWithCodec leaves the store WITHOUT a weight codec. This is the
		// wiring store/db.go's "Typical wiring" doc comment recommended verbatim
		// until rmp #2747.
		st := txn.NewStoreWithCodec[string, float64](g, w, txn.NewStringCodec())
		eng := cypher.NewEngineWithStore(st)

		runErr := runToCompletion2747(t, eng, createRelQuery2747)
		if runErr == nil {
			// Show what was actually lost, so the failure is self-explaining.
			rg := recoveredGraph2747(t, w, dir)
			t.Fatalf("ACID Durability breach: CREATE of a relationship was ACKNOWLEDGED on a store with "+
				"no weight codec; the WAL never received the edge, and recovery returned %d nodes with "+
				"relationship=%v",
				len(endpointKeys2747(t, rg)), anyEdgeBetween2747(rg, endpointKeys2747(t, rg)))
		}
		if !errors.Is(runErr, txn.ErrNoWeightCodec) {
			t.Fatalf("refusal is not typed: got %v, want it to wrap txn.ErrNoWeightCodec", runErr)
		}

		// Fail-stop, not fail-silent: having refused, the engine must not have
		// left a relationship durably recorded either.
		rg := recoveredGraph2747(t, w, dir)
		if anyEdgeBetween2747(rg, endpointKeys2747(t, rg)) {
			t.Fatal("a refused CREATE left a relationship in the recovered graph")
		}
	})
}

// TestOverlongLabelRefusedBeforeInMemoryWrite_2747 asserts the field bound is
// enforced at the API, ahead of the in-memory write, rather than only at the
// encoder when the commit is assembled.
//
// The oracle is the label registry. [lpg.Graph.SetNodeLabel] interns the label
// into the process-lifetime [lpg.LabelRegistry], and NOTHING un-interns it —
// not the undo log, not rollbackUnderBarrier. So an over-long label found in
// the registry after a refused statement is positive proof the in-memory write
// LANDED and was only reversed afterwards. Registry.Lookup is the non-interning
// read, so the assertion cannot create what it looks for.
//
// Gate invariant:
//   - BEFORE the fix, Exec returns nil, the label IS interned, a later statement
//     in the same transaction counts the node, and only Commit refuses.
//   - AFTER the fix, Exec refuses with an error wrapping txn.ErrFieldTooLong,
//     the label is NOT interned, and no statement ever saw the node.
func TestOverlongLabelRefusedBeforeInMemoryWrite_2747(t *testing.T) {
	t.Parallel()

	over := strings.Repeat("L", uint16FieldCap2747+1)
	atCap := strings.Repeat("C", uint16FieldCap2747)

	t.Run("explicit_tx", func(t *testing.T) {
		t.Parallel()
		eng, g, w, _ := walEngineWithGraph(t)
		t.Cleanup(func() { _ = w.Close() })

		tx, err := eng.BeginTx(context.Background())
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		defer func() { _ = tx.Rollback() }()

		res, execErr := tx.Exec("CREATE (n:`"+over+"`)", nil)
		if res != nil {
			for res.Next() { // deliberate full drain
			}
			if execErr == nil {
				execErr = res.Err()
			}
			_ = res.Close()
		}
		if execErr == nil {
			t.Fatal("the statement was ACCEPTED: a label one byte past the uint16 prefix must be " +
				"refused by the statement, not deferred to Commit")
		}
		if !errors.Is(execErr, txn.ErrFieldTooLong) {
			t.Fatalf("refusal is not typed: got %v, want it to wrap txn.ErrFieldTooLong", execErr)
		}

		// The load-bearing assertion: the in-memory write never happened.
		if _, interned := g.Registry().Lookup(over); interned {
			t.Fatalf("the refusal came AFTER the in-memory write: the %d-byte label is interned in the "+
				"label registry, which no rollback reverses", len(over))
		}

		// Atomicity: a refused statement dooms its transaction, so the partial
		// pattern node the CREATE had already interned before reaching the label
		// can never be acknowledged. This is [ExplicitTx.Exec]'s standing
		// contract for ANY failed statement (it sets failed and Commit returns
		// ErrTxPoisoned), asserted here because the refusal is new.
		if commitErr := tx.Commit(); !errors.Is(commitErr, cypher.ErrTxPoisoned) {
			t.Fatalf("a transaction whose statement was refused committed or failed wrongly: got %v, "+
				"want cypher.ErrTxPoisoned", commitErr)
		}
	})

	t.Run("autocommit", func(t *testing.T) {
		t.Parallel()
		eng, g, w, _ := walEngineWithGraph(t)
		t.Cleanup(func() { _ = w.Close() })

		runErr := runToCompletion2747(t, eng, "CREATE (n:`"+over+"`)")
		if runErr == nil {
			t.Fatal("the autocommit statement was ACKNOWLEDGED for a label the WAL cannot represent")
		}
		if !errors.Is(runErr, txn.ErrFieldTooLong) {
			t.Fatalf("refusal is not typed: got %v, want it to wrap txn.ErrFieldTooLong", runErr)
		}
		if _, interned := g.Registry().Lookup(over); interned {
			t.Fatalf("the refusal came AFTER the in-memory write: the %d-byte label is interned", len(over))
		}
	})

	// The over-restriction guard AND the non-vacuity control: a label of exactly
	// the prefix's capacity is representable, so it must still be accepted, land
	// in memory, be interned, and survive recovery byte-for-byte. Without this,
	// "the over-long label is absent" would pass on a build that refused every
	// label.
	t.Run("control_at_cap_label_is_accepted_and_recovered", func(t *testing.T) {
		t.Parallel()
		eng, g, w, dir := walEngineWithGraph(t)

		if err := runToCompletion2747(t, eng, "CREATE (n:`"+atCap+"`)"); err != nil {
			t.Fatalf("over-restricted: a %d-byte label (the exact uint16 capacity) was refused: %v",
				uint16FieldCap2747, err)
		}
		if _, interned := g.Registry().Lookup(atCap); !interned {
			t.Fatal("control: an accepted at-cap label was not interned, so the registry oracle used " +
				"by the refusal assertions above cannot observe an in-memory write at all")
		}

		rg := recoveredGraph2747(t, w, dir)
		keys := endpointKeys2747(t, rg)
		if len(keys) != 1 {
			t.Fatalf("control: recovered %d nodes, want 1", len(keys))
		}
		if !rg.HasNodeLabel(keys[0], atCap) {
			t.Fatalf("control: the at-cap label did not survive recovery (recovered labels have lengths %v)",
				labelLens2747(rg.NodeLabels(keys[0])))
		}
	})
}

func labelLens2747(labels []string) []int {
	out := make([]int, len(labels))
	for i, s := range labels {
		out[i] = len(s)
	}
	return out
}
