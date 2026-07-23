package cypher

// count_rapid_test.go — task #2085. Two correctness/determinism properties for
// the relationship count-store, complementing the concurrency and crash-parity
// suites that already exist (count_maintenance_test.go, count_durability_test.go):
//
//   - TestCountStore_Rapid: a pgregory.net/rapid property test that drives random
//     CREATE / DELETE / SET-label / REMOVE-label sequences and, after every
//     operation, asserts the store equals a fresh ground-truth O(V+E) recount —
//     E and N exact always, D/T exact on non-dirty cells (dirty cells skipped),
//     and sum(E) == the live edge count.
//   - TestCountStore_DeterministicSameSeed: the determinism mandate — two runs of
//     the SAME fixed-seed pseudo-random workload must produce a byte-identical
//     canonical count-store state. No time or global rand participates.
//
// Layer: short. Race-clean.

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// countAlphabet is the small label/type vocabulary both properties draw from. A
// tiny alphabet maximises cell reuse (and dirty-flag interplay) so the recount
// oracle is exercised against a dense, churning store rather than disjoint cells.
var (
	countLabels = []string{"A", "B", "C", "D"}
	countTypes  = []string{"R", "S", "T"}
)

// newMultigraphEngine builds a directed multigraph engine (the openCypher
// storage model) with the given per-relabel OUT recount budget.
func newMultigraphEngine(budget int) (*Engine, *lpg.Graph[string, float64]) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	return NewEngineWithOptions(g, EngineOptions{MaxLabelRecountEdges: budget}), g
}

// recountLabelsByName returns the exact live-node count per label name by an
// independent O(V) walk — the ground-truth oracle for the N(label) statistic the
// label index serves (it is not held in the count-store).
func recountLabelsByName(g *lpg.Graph[string, float64]) map[string]int64 {
	m := make(map[string]int64)
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		if g.IsTombstoned(id) {
			return true
		}
		for _, name := range g.NodeLabelsByID(id) {
			m[name]++
		}
		return true
	})
	return m
}

// sumEdgeCells sums every E cell — the store's view of the total live typed-edge
// count, which must equal the graph's live edge count.
func sumEdgeCells(snap *count.Snapshot) int64 {
	var sum int64
	for _, v := range snap.E {
		sum += v
	}
	return sum
}

// TestCountStore_Rapid drives random write sequences and checks the store against
// a fresh recount after every operation.
func TestCountStore_Rapid(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		budget := rapid.SampledFrom([]int{0, 2, 8}).Draw(rt, "budget")
		eng, g := newMultigraphEngine(budget)
		src := resolverFor(eng)
		ctx := context.Background()

		runQ := func(q string) {
			r, err := eng.RunInTx(ctx, q, nil)
			if err != nil {
				rt.Fatalf("RunInTx(%q): %v", q, err)
			}
			for r.Next() {
			}
			if err := r.Err(); err != nil {
				_ = r.Close()
				rt.Fatalf("RunInTx(%q) drain: %v", q, err)
			}
			if err := r.Close(); err != nil {
				rt.Fatalf("RunInTx(%q) close: %v", q, err)
			}
		}

		nOps := rapid.IntRange(1, 30).Draw(rt, "nOps")
		for i := 0; i < nOps; i++ {
			switch rapid.IntRange(0, 4).Draw(rt, "op") {
			case 0, 1: // CREATE a typed edge between two fresh labelled nodes
				la := rapid.SampledFrom(countLabels).Draw(rt, "la")
				lb := rapid.SampledFrom(countLabels).Draw(rt, "lb")
				ty := rapid.SampledFrom(countTypes).Draw(rt, "createType")
				runQ("CREATE (:" + la + ")-[:" + ty + "]->(:" + lb + ")")
			case 2: // add a label to some node of a known label
				from := rapid.SampledFrom(countLabels).Draw(rt, "from")
				to := rapid.SampledFrom(countLabels).Draw(rt, "to")
				runQ("MATCH (n:" + from + ") WITH n LIMIT 1 SET n:" + to)
			case 3: // remove a label
				l := rapid.SampledFrom(countLabels).Draw(rt, "removeLabel")
				runQ("MATCH (n:" + l + ") WITH n LIMIT 1 REMOVE n:" + l)
			case 4: // delete one relationship of a random type
				ty := rapid.SampledFrom(countTypes).Draw(rt, "deleteType")
				runQ("MATCH ()-[r:" + ty + "]->() WITH r LIMIT 1 DELETE r")
			}

			// E/D/T: exact on every non-dirty cell, bidirectionally.
			if diffs := diffCounts(eng.countStore, g); len(diffs) > 0 {
				rt.Fatalf("count-store diverged from recount after op %d:\n%s", i, strings.Join(diffs, "\n"))
			}
			// sum(E) == live edge count.
			snap := eng.countStore.Snapshot()
			if got, want := sumEdgeCells(&snap), int64(g.AdjList().Size()); got != want {
				rt.Fatalf("sum(E)=%d != live edge count=%d after op %d", got, want, i)
			}
			// N(label) exact for every label in the alphabet, always.
			oracleN := recountLabelsByName(g)
			for _, name := range countLabels {
				got, _ := src.ResolveLabelCount(name)
				if want := oracleN[name]; got != want {
					rt.Fatalf("N(%s)=%d, want %d (label index vs recount) after op %d", name, got, want, i)
				}
			}
		}
	})
}

// countWorkloadPool is the number of uniquely-keyed nodes the deterministic
// determinism workload operates over.
const countWorkloadPool = 12

// runSeededCountWorkload runs a fixed pseudo-random write workload driven solely
// by a seeded RNG (no time, no global rand) and returns the resulting count-store
// snapshot. Two calls with the same seed must return equal state.
//
// Every mutation targets its node(s) by a UNIQUE integer key property (k), so the
// affected node is unambiguous and the whole graph — hence the count-store — is a
// deterministic function of the seeded choices alone. This deliberately avoids
// id()-ordered targeting: primary ids are drawn from a process-global counter, so
// their values are not stable across engine instances (or under concurrent id
// consumption by other tests), which would make an id()-ordered LIMIT pick
// different nodes across runs and conflate query row-order with the count-store's
// own determinism. The count-store keys its cells on the registry's per-run
// interned label/type ids, whose assignment order is fixed by the seeded query
// sequence, so two same-seed runs also agree on those ids.
func runSeededCountWorkload(t *testing.T, seed int64) count.Snapshot {
	t.Helper()
	eng, _ := newMultigraphEngine(8)
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // determinism is the point; not security-sensitive.

	// A fixed pool of uniquely-keyed nodes, each with a deterministic initial
	// label. Later mutations address these nodes by their unique k.
	for k := 0; k < countWorkloadPool; k++ {
		mustRun(t, eng, fmt.Sprintf("CREATE (:%s {k:%d})", countLabels[k%len(countLabels)], k))
	}

	for step := 0; step < 300; step++ {
		i := rng.Intn(countWorkloadPool)
		j := rng.Intn(countWorkloadPool)
		switch rng.Intn(5) {
		case 0, 1: // create a typed edge between two pooled nodes
			ty := countTypes[rng.Intn(len(countTypes))]
			mustRun(t, eng, fmt.Sprintf("MATCH (a {k:%d}),(b {k:%d}) CREATE (a)-[:%s]->(b)", i, j, ty))
		case 2: // add a label to a pooled node
			to := countLabels[rng.Intn(len(countLabels))]
			mustRun(t, eng, fmt.Sprintf("MATCH (n {k:%d}) SET n:%s", i, to))
		case 3: // remove a label from a pooled node
			l := countLabels[rng.Intn(len(countLabels))]
			mustRun(t, eng, fmt.Sprintf("MATCH (n {k:%d}) REMOVE n:%s", i, l))
		case 4: // delete every edge of a type between two pooled nodes
			ty := countTypes[rng.Intn(len(countTypes))]
			mustRun(t, eng, fmt.Sprintf("MATCH (a {k:%d})-[r:%s]->(b {k:%d}) DELETE r", i, ty, j))
		}
	}
	return eng.countStore.Snapshot()
}

// canonCountState serialises a snapshot into a deterministic, order-independent
// byte form: every map is emitted with sorted keys and every dirty-label set is
// sorted, so equal logical state produces byte-identical output regardless of map
// iteration order. Two same-seed runs whose canonical bytes differ prove a
// determinism violation.
func canonCountState(s *count.Snapshot) []byte {
	var b bytes.Buffer

	eKeys := make([]uint32, 0, len(s.E))
	for k := range s.E {
		eKeys = append(eKeys, k)
	}
	sort.Slice(eKeys, func(i, j int) bool { return eKeys[i] < eKeys[j] })
	for _, k := range eKeys {
		fmt.Fprintf(&b, "E %d=%d\n", k, s.E[k])
	}

	writeD := func(name string, m map[uint64]int64) {
		keys := make([]uint64, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, k := range keys {
			fmt.Fprintf(&b, "%s %d=%d\n", name, k, m[k])
		}
	}
	writeD("DOut", s.DOut)
	writeD("DIn", s.DIn)

	tKeys := make([][3]uint32, 0, len(s.T))
	for k := range s.T {
		tKeys = append(tKeys, k)
	}
	sort.Slice(tKeys, func(i, j int) bool {
		a, c := tKeys[i], tKeys[j]
		if a[0] != c[0] {
			return a[0] < c[0]
		}
		if a[1] != c[1] {
			return a[1] < c[1]
		}
		return a[2] < c[2]
	})
	for _, k := range tKeys {
		fmt.Fprintf(&b, "T %d,%d,%d=%d\n", k[0], k[1], k[2], s.T[k])
	}

	writeDirty := func(name string, ids []uint32) {
		sorted := append([]uint32(nil), ids...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		fmt.Fprintf(&b, "%s %v\n", name, sorted)
	}
	writeDirty("dDirtyOut", s.DirtyDOut)
	writeDirty("dDirtyIn", s.DirtyDIn)
	writeDirty("tDirtyA", s.DirtyTA)
	writeDirty("tDirtyB", s.DirtyTB)

	return b.Bytes()
}

// TestCountStore_DeterministicSameSeed runs the identical seeded workload twice
// and asserts the two count-store states are byte-identical (the determinism
// mandate).
func TestCountStore_DeterministicSameSeed(t *testing.T) {
	t.Parallel()

	const seed = 0x5eed_1234
	sa := runSeededCountWorkload(t, seed)
	sb := runSeededCountWorkload(t, seed)
	a := canonCountState(&sa)
	b := canonCountState(&sb)
	if !bytes.Equal(a, b) {
		t.Fatalf("same-seed count-store state differs between runs:\n--- run A ---\n%s\n--- run B ---\n%s", a, b)
	}
	// Guard against a degenerate all-empty state passing vacuously: the workload
	// must have produced live cells.
	if len(a) == 0 {
		t.Fatal("canonical state is empty; the workload produced no count cells")
	}
}
