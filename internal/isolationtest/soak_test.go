package isolationtest_test

// soak_test.go — the specs whose full enumeration is too large for the short
// layer (rmp #2340 AC6; see docs/test-layers.md).
//
// The enumeration is a multinomial, (Σnᵢ)! / Πnᵢ!, so a spec grows out of the
// short layer far faster than it looks. Two sessions of three steps is 20
// permutations; THREE sessions of 3, 4 and 3 steps is 4200. That is the whole
// reason this file exists, and the reason [isolationtest.CountPermutations]
// exists: a layer assignment here is computed, not guessed.

import (
	"math/big"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/isolationtest"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// readOnlyAnomalySpec is the Fekete/O'Neil read-only transaction anomaly — the
// scenario PostgreSQL ships as specs/read-only-anomaly.spec, which is the single
// spec rmp #2340 named because it is the same shape as GoGraph's examples/27.
//
// Two writers and one read-only observer. Under snapshot isolation the observer
// can see a state consistent with NO serial ordering of the two writers: s1's
// credit of Y applied, s2's debit of X not yet, even though s2 read a state in
// which the credit had not happened. It is not a defect — it is the documented
// behaviour of SI, and the reason serialisable snapshot isolation had to be
// invented — and pinning it here is what makes a future change to GoGraph's
// isolation level visible as a diff.
//
// The full enumeration is 4200 permutations, which is why this is soak.
func readOnlyAnomalySpec() *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "read-only-anomaly",
		Doc: "The read-only transaction anomaly under snapshot isolation (Fekete, O'Neil &\n" +
			"O'Neil). Two accounts start at 0. s1 credits Y by 20; s2 reads both and then\n" +
			"debits X by 11 (an overdraft charge it would not have applied had it seen the\n" +
			"credit). A read-only s3 can observe X=0, Y=20 — a state no serial ordering of s1\n" +
			"and s2 produces. GoGraph is snapshot-isolated, so this is EXPECTED; the\n" +
			"transcript is the before-picture for any future serialisable mode.",
		Setup: []isolationtest.Step{
			{Name: "mk", Query: "CREATE (:Acct {id:'X', bal:0}), (:Acct {id:'Y', bal:0})"},
		},
		Sessions: []*isolationtest.Session{
			{
				Name:  "s1",
				Setup: []isolationtest.Step{{Name: "s1b", Ctl: isolationtest.Begin}},
				Steps: []isolationtest.Step{
					{Name: "s1ry", Query: "MATCH (a:Acct {id:'Y'}) RETURN a.bal AS bal"},
					{Name: "s1wy", Query: "MATCH (a:Acct {id:'Y'}) SET a.bal = 20 RETURN a.bal AS bal"},
					{Name: "s1c", Ctl: isolationtest.Commit},
				},
			},
			{
				Name:  "s2",
				Setup: []isolationtest.Step{{Name: "s2b", Ctl: isolationtest.Begin}},
				Steps: []isolationtest.Step{
					{Name: "s2rx", Query: "MATCH (a:Acct {id:'X'}) RETURN a.bal AS bal"},
					{Name: "s2ry", Query: "MATCH (a:Acct {id:'Y'}) RETURN a.bal AS bal"},
					{Name: "s2wx", Query: "MATCH (a:Acct {id:'X'}) SET a.bal = -11 RETURN a.bal AS bal"},
					{Name: "s2c", Ctl: isolationtest.Commit},
				},
			},
			{
				// s3's BEGIN is a STEP, not session setup, and that is not a
				// stylistic choice — it is forced by a real difference from the
				// reference engine. PostgreSQL DEFERS a REPEATABLE READ
				// snapshot to the transaction's first statement, so its spec can
				// open s3 in session setup and still have s3's snapshot taken
				// wherever s3r lands. GoGraph takes the snapshot EAGERLY at
				// BeginReadTx, so an s3 opened in setup is pinned to the initial
				// state in every interleaving and the anomaly is unreachable —
				// measured: s3r returned X=0, Y=0 for the reference's own
				// permutation. Interleaving the BEGIN restores the degree of
				// freedom the deferred snapshot gives PostgreSQL for free.
				Name: "s3",
				Steps: []isolationtest.Step{
					{Name: "s3b", Ctl: isolationtest.BeginRead},
					{Name: "s3r", Query: "MATCH (a:Acct) RETURN a.id AS id, a.bal AS bal ORDER BY a.id"},
					{Name: "s3c", Ctl: isolationtest.Commit},
				},
			},
		},
	}
}

// TestReadOnlyAnomalyNamedPermutation runs the SINGLE interleaving from
// PostgreSQL's spec, at the SHORT layer.
//
// Naming the permutation is PostgreSQL's own escape hatch for a spec whose full
// enumeration is impractical, and it is used here for the same reason: the one
// ordering that exhibits the anomaly is worth asserting on every run, while the
// other 4199 are worth asserting nightly.
func TestReadOnlyAnomalyNamedPermutation(t *testing.T) {
	t.Parallel()
	s := readOnlyAnomalySpec()
	s.Name = "read-only-anomaly-named"
	// PostgreSQL's own permutation, with s3's BEGIN placed after s1's COMMIT —
	// which is where PostgreSQL's deferred snapshot effectively puts it.
	s.Permutations = [][]string{{"s2rx", "s2ry", "s1ry", "s1wy", "s1c", "s3b", "s3r", "s2wx", "s2c", "s3c"}}
	isolationtest.Check(t, s, memRunner())
}

// TestReadOnlyAnomalyExhaustive enumerates all 4200 interleavings.
//
// Layer: soak. The assertion below is on the COUNT rather than on a transcript,
// because a 4200-permutation golden file is a maintenance liability that nobody
// would read a diff of; what matters exhaustively is the invariant, which is
// checked by an Observer: no reader may ever see a balance other than the three
// the reachable commit states produce.
func TestReadOnlyAnomalyExhaustive(t *testing.T) {
	testlayers.RequireSoak(t)
	t.Parallel()
	s := readOnlyAnomalySpec()
	if want, got := big.NewInt(4200), isolationtest.CountPermutations(s); want.Cmp(got) != 0 {
		t.Fatalf("expected %s permutations, spec now yields %s — the scenario changed shape "+
			"and its layer assignment must be re-derived", want, got)
	}
	// The reachable balances are exactly these: 0 (neither writer visible), 20
	// (s1 visible on Y) and -11 (s2 visible on X). Anything else is a value no
	// commit state produces — i.e. a torn read.
	legal := map[string]bool{"0": true, "20": true, "-11": true}
	r := &isolationtest.Runner{
		NewEngine: memEngine,
		Observe: func(o isolationtest.Observation) error {
			// By COLUMN NAME, not by cell content. Keying off the text was
			// wrong and the exhaustive run caught it immediately: the id column
			// renders as `"X"` with quotes, so a value-based guard rejected
			// every one of the 4200 interleavings. An invariant that fires on
			// everything is indistinguishable from no invariant at all.
			bal := -1
			for i, c := range o.Cols {
				if c == "bal" {
					bal = i
				}
			}
			if bal < 0 {
				return nil
			}
			for _, row := range o.Rows {
				if !legal[row[bal]] {
					return errUnreachableBalance(row[bal])
				}
			}
			return nil
		},
	}
	// VALIDATE THE INSTRUMENT BEFORE TRUSTING ITS SILENCE. A clean exhaustive
	// run means "no violation" only if this observer can produce one; the first
	// version of it could not tell them apart — it keyed on cell text, the id
	// column renders quoted, and it therefore rejected all 4200 interleavings.
	// The mirror-image failure is silent and far worse, so the observer is asked
	// here, directly, to reject a balance no commit state produces.
	probe := r.Observe(isolationtest.Observation{
		Cols: []string{"id", "bal"},
		Rows: [][]string{{`"X"`, "9"}},
	})
	if probe == nil {
		t.Fatal("the conservation observer accepted a balance of 9, which no commit state produces; " +
			"a clean exhaustive run below would therefore prove nothing")
	}
	if err := r.Observe(isolationtest.Observation{
		Cols: []string{"id", "bal"},
		Rows: [][]string{{`"X"`, "0"}, {`"Y"`, "20"}},
	}); err != nil {
		t.Fatalf("the conservation observer rejected a legal state: %v", err)
	}

	var sink discardWriter
	if err := r.Run(t.Context(), s, &sink); err != nil {
		t.Fatalf("run read-only-anomaly: %v", err)
	}
	if v := r.Violations(); len(v) > 0 {
		t.Errorf("%d interleavings observed a balance no commit state produces:\n%s", len(v), v[0])
	}
}

type errUnreachableBalance string

func (e errUnreachableBalance) Error() string {
	return "observed balance " + string(e) + ", which no reachable commit state produces"
}
