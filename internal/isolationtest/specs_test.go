package isolationtest_test

// specs_test.go — the shipped isolation scenarios (rmp #2340).
//
// Each spec here is a named, scripted concurrent scenario whose EVERY
// order-preserving interleaving is run and whose transcript is diffed against a
// golden file. What that buys over the existing tests is stated in the package
// doc: a randomised search can find an anomaly, but only exhaustive enumeration
// can certify that a scenario is free of one, and only a golden transcript makes
// a change in isolation behaviour impossible to merge unnoticed.
//
// # What GoGraph is expected to do, and why these three
//
// GoGraph's sole concurrency control is MVCC with SNAPSHOT ISOLATION and
// per-object write-write conflict detection. That fixes what each scenario must
// show, and the three were chosen because they sit on different sides of that
// line:
//
//   - lost update — SI must REFUSE it. Two transactions that both read a
//     counter and both write it collide on the same object, so the second
//     committer is rejected. A permutation where this scenario silently keeps
//     one increment is the classic P4 anomaly and would be a defect.
//
//   - write skew — SI ALLOWS it, and that is not a bug: it is the documented
//     price of snapshot isolation as opposed to serialisability (Berenson et
//     al., "A Critique of ANSI SQL Isolation Levels", SIGMOD 1995, §4.4.4 A5B).
//     The two transactions write DIFFERENT objects, so nothing collides. The
//     golden file pins that GoGraph behaves as SI and not as something weaker,
//     and the day GoGraph gains a serialisable mode this transcript is the
//     before-picture.
//
//   - bank transfer — the conservation invariant from examples/27. Every
//     interleaving must leave the total unchanged, and no reader may ever
//     observe a partial transfer. This is the deterministic, named counterpart
//     of the torn-total sightings that rmp #2333 could not reproduce and that
//     rmp #2336 still chases with a standing randomised search.

import (
	"math/big"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/isolationtest"
)

// memEngine builds the store-less engine: MVCC concurrency control with no
// durability cost mixed in, which is the wiring these scenarios are about.
func memEngine() (*isolationtest.Engine, error) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	return &isolationtest.Engine{
		Eng:   cypher.NewEngine(g),
		Close: func() error { return nil },
	}, nil
}

func memRunner() *isolationtest.Runner {
	return &isolationtest.Runner{NewEngine: memEngine}
}

// lostUpdateSpec is the P4 lost-update scenario: two transactions read the same
// counter and both write it back.
//
// Under snapshot isolation the second committer MUST be refused, because both
// wrote the same object. The transcript records which of the two loses in each
// interleaving.
func lostUpdateSpec() *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "lost-update",
		Doc: "P4 lost update. Two transactions read counter c and each writes back a value\n" +
			"derived from what it read. GoGraph's concurrency control is MVCC with per-object\n" +
			"write-write conflict detection, so one of the two MUST be refused in every\n" +
			"interleaving where both write before either commits. An interleaving in which\n" +
			"both commit and one increment vanishes is the anomaly.",
		Setup: []isolationtest.Step{
			{Name: "mk", Query: "CREATE (:Counter {name:'c', n:0})"},
		},
		Sessions: []*isolationtest.Session{
			{
				Name:  "s1",
				Setup: []isolationtest.Step{{Name: "s1b", Ctl: isolationtest.Begin}},
				Steps: []isolationtest.Step{
					{Name: "s1r", Query: "MATCH (c:Counter {name:'c'}) RETURN c.n AS n"},
					{Name: "s1w", Query: "MATCH (c:Counter {name:'c'}) SET c.n = 10 RETURN c.n AS n"},
					{Name: "s1c", Ctl: isolationtest.Commit},
				},
			},
			{
				Name:  "s2",
				Setup: []isolationtest.Step{{Name: "s2b", Ctl: isolationtest.Begin}},
				Steps: []isolationtest.Step{
					{Name: "s2r", Query: "MATCH (c:Counter {name:'c'}) RETURN c.n AS n"},
					{Name: "s2w", Query: "MATCH (c:Counter {name:'c'}) SET c.n = 20 RETURN c.n AS n"},
					{Name: "s2c", Ctl: isolationtest.Commit},
				},
			},
		},
		Teardown: []isolationtest.Step{
			{Name: "final", Query: "MATCH (c:Counter {name:'c'}) RETURN c.n AS n"},
		},
	}
}

// writeSkewSpec is A5B write skew: two transactions read an overlapping set and
// each writes a DIFFERENT member of it.
//
// Snapshot isolation permits this — nothing collides — and the golden file pins
// that GoGraph is SI rather than something weaker or stronger.
func writeSkewSpec() *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "write-skew",
		Doc: "A5B write skew (Berenson et al., SIGMOD 1995 §4.4.4). Two on-call doctors; the\n" +
			"rule is that at least one must stay on call. Each transaction reads the count of\n" +
			"on-call doctors, sees 2, and takes ITSELF off call — writing a different node, so\n" +
			"there is no write-write conflict to detect. Snapshot isolation ALLOWS this and\n" +
			"the invariant ends up violated. That is the documented price of SI, not a defect;\n" +
			"this transcript exists so the day GoGraph offers serialisability the change is\n" +
			"visible as a diff rather than as folklore.",
		Setup: []isolationtest.Step{
			{Name: "mk", Query: "CREATE (:Doctor {name:'alice', oncall:true}), (:Doctor {name:'bob', oncall:true})"},
		},
		Sessions: []*isolationtest.Session{
			{
				Name:  "s1",
				Setup: []isolationtest.Step{{Name: "s1b", Ctl: isolationtest.Begin}},
				Steps: []isolationtest.Step{
					{Name: "s1r", Query: "MATCH (d:Doctor) WHERE d.oncall = true RETURN count(d) AS oncall"},
					{Name: "s1w", Query: "MATCH (d:Doctor {name:'alice'}) SET d.oncall = false RETURN d.name AS who"},
					{Name: "s1c", Ctl: isolationtest.Commit},
				},
			},
			{
				Name:  "s2",
				Setup: []isolationtest.Step{{Name: "s2b", Ctl: isolationtest.Begin}},
				Steps: []isolationtest.Step{
					{Name: "s2r", Query: "MATCH (d:Doctor) WHERE d.oncall = true RETURN count(d) AS oncall"},
					{Name: "s2w", Query: "MATCH (d:Doctor {name:'bob'}) SET d.oncall = false RETURN d.name AS who"},
					{Name: "s2c", Ctl: isolationtest.Commit},
				},
			},
		},
		Teardown: []isolationtest.Step{
			{Name: "final", Query: "MATCH (d:Doctor) WHERE d.oncall = true RETURN count(d) AS oncall"},
		},
	}
}

// bankTransferSpec is the conservation scenario from examples/27, made
// deterministic and named.
//
// A transfer moves 10 from X to Y inside one transaction; a concurrent reader
// takes the total. NO interleaving may show a total other than 100, because a
// snapshot reader must observe all of a transaction or none of it. This is the
// exact shape of the torn totals rmp #2333 could not reproduce.
func bankTransferSpec() *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "bank-transfer",
		Doc: "Conservation under concurrent transfer — the deterministic counterpart of\n" +
			"examples/27_concurrent_txn, and of the torn-total sightings rmp #2333 could not\n" +
			"reproduce. s1 moves 10 from X to Y inside ONE transaction; s2 reads the total in\n" +
			"a transaction of its own. Snapshot isolation requires that s2 observe ALL of s1\n" +
			"or NONE of it, so EVERY interleaving must report a total of 100. A transcript\n" +
			"line showing 90 or 110 is a torn read and an isolation defect.",
		Setup: []isolationtest.Step{
			{Name: "mk", Query: "CREATE (:Account {id:'X', bal:50}), (:Account {id:'Y', bal:50})"},
		},
		Sessions: []*isolationtest.Session{
			{
				Name:  "s1",
				Setup: []isolationtest.Step{{Name: "s1b", Ctl: isolationtest.Begin}},
				Steps: []isolationtest.Step{
					{Name: "s1dx", Query: "MATCH (a:Account {id:'X'}) SET a.bal = a.bal - 10 RETURN a.bal AS bal"},
					{Name: "s1cy", Query: "MATCH (a:Account {id:'Y'}) SET a.bal = a.bal + 10 RETURN a.bal AS bal"},
					{Name: "s1c", Ctl: isolationtest.Commit},
				},
			},
			{
				Name:  "s2",
				Setup: []isolationtest.Step{{Name: "s2b", Ctl: isolationtest.BeginRead}},
				Steps: []isolationtest.Step{
					{Name: "s2t", Query: "MATCH (a:Account) RETURN sum(a.bal) AS total"},
					{Name: "s2t2", Query: "MATCH (a:Account) RETURN sum(a.bal) AS total"},
					{Name: "s2c", Ctl: isolationtest.Commit},
				},
			},
		},
		Teardown: []isolationtest.Step{
			{Name: "final", Query: "MATCH (a:Account) RETURN sum(a.bal) AS total"},
		},
	}
}

// TestSpecs runs every shipped spec over EVERY interleaving and diffs the
// transcript against its golden file.
//
// Layer: short. Each spec is two sessions of three steps, so the enumeration is
// C(6,3) = 20 permutations each — verified by the assertion below rather than
// assumed, because the multinomial grows fast enough that a spec which looks
// small can be six figures and quietly blow the package's 60 s budget.
func TestSpecs(t *testing.T) {
	t.Parallel()
	specs := []*isolationtest.Spec{lostUpdateSpec(), writeSkewSpec(), bankTransferSpec()}
	for _, s := range specs {
		t.Run(s.Name, func(t *testing.T) {
			t.Parallel()
			if n := isolationtest.CountPermutations(s); n.Cmp(big.NewInt(64)) > 0 {
				t.Fatalf("spec %s enumerates %s permutations, which is too many for the short layer; "+
					"either name explicit permutations or move it to soak (docs/test-layers.md)", s.Name, n)
			}
			isolationtest.Check(t, s, memRunner())
		})
	}
}

// TestPermutationIsReRunnableByName is acceptance criterion 3: a permutation
// named in a failure diff can be replayed on its own and reproduces exactly.
//
// It is asserted rather than asserted-about: the full transcript is produced,
// one permutation's section is cut out of it, the same permutation is then run
// ALONE through Runner.Only, and the two must agree byte for byte. If per-
// permutation state ever leaked — a shared engine, a counter that survives —
// this is the test that would catch it.
func TestPermutationIsReRunnableByName(t *testing.T) {
	t.Parallel()
	s := bankTransferSpec()
	perms := isolationtest.Permutations(s)
	if len(perms) == 0 {
		t.Fatal("no permutations")
	}
	for _, idx := range []int{0, len(perms) / 2, len(perms) - 1} {
		name := perms[idx].Name()
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			full := runToString(t, s, memRunner())
			alone := runToString(t, s, &isolationtest.Runner{NewEngine: memEngine, Only: name})
			wantSection := sectionOf(t, full, name)
			gotSection := sectionOf(t, alone, name)
			if wantSection != gotSection {
				t.Errorf("permutation %q does not reproduce when run alone\n--- in full run ---\n%s\n--- alone ---\n%s",
					name, wantSection, gotSection)
			}
		})
	}
}

// TestOnlyRejectsUnknownPermutation pins the failure mode of the re-run path: a
// name that does not exist must be an error, never a silent zero-permutation
// pass. A harness that reports success for running nothing is the shape of
// instrument the measurement discipline exists to forbid.
func TestOnlyRejectsUnknownPermutation(t *testing.T) {
	t.Parallel()
	r := &isolationtest.Runner{NewEngine: memEngine, Only: "no such steps at all"}
	var sink discardWriter
	err := r.Run(t.Context(), bankTransferSpec(), &sink)
	if err == nil {
		t.Fatal("Runner.Only with an unknown permutation returned nil; it must be an error")
	}
	if sink.n != 0 {
		t.Errorf("wrote %d bytes for an unknown permutation; expected none", sink.n)
	}
}
