package main

// catalogue_test.go — the regression gate for the deterministic MVCC scenario
// catalogue (rmp #2933). Layer: short.
//
// # Where the golden files live, and why
//
// The goldens are in internal/isolationtest/testdata, beside the harness's own.
// That is decided by the harness, not chosen here: isolationtest.Check hands the
// relative path testdata/<spec>.golden to goldens.Assert, which resolves it
// against the source directory of ITS caller — golden.go in
// internal/isolationtest — so every spec checked through Check reads and writes
// its golden there, whichever package runs it. The four rows that already had
// goldens there (WW01 lost-update, SK01 write-skew, SK06
// read-only-anomaly-named, SK09 bank-transfer) are mapped in README.md and not
// duplicated. Every catalogue golden is named <row id>-<scenario>, so the
// catalogue's files are told apart from the harness's own by prefix.

import (
	"bytes"
	"context"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/isolationtest"
)

// TestMain runs every test of this example under goleak: the catalogue starts one
// goroutine per session per permutation and one engine (with its vacuum) per
// permutation, so a missed close would leak thousands of goroutines.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// goldenDir is where isolationtest.Check keeps every golden (see the file
// comment), relative to this package.
const goldenDir = "../../internal/isolationtest/testdata"

// catalogueGolden matches the catalogue's golden names: a catalogue row ID
// prefix (ww01-, sk12-, ...). The harness's own goldens carry no such prefix.
var catalogueGolden = regexp.MustCompile(`^(ww|sk)\d{2}-.*\.golden$`)

// maxPermutations is the short-layer ceiling on one spec's interleavings. Every
// spec is checked against it rather than assumed to fit, because the multinomial
// grows fast enough that a spec which looks small can be six figures.
const maxPermutations = 128

// TestCatalogue runs every scenario over every interleaving it declares and
// diffs its transcript against its golden. A scenario that carries a
// property check must also report no violation, and no step of any scenario may
// block: GoGraph's DML takes no locks (F7), so a <waiting ...> line is a defect.
func TestCatalogue(t *testing.T) {
	t.Parallel()
	for _, sc := range catalogue() {
		w := &world{}
		spec := sc.build(w)
		t.Run(sc.ID+"/"+spec.Name, func(t *testing.T) {
			t.Parallel()
			if n := isolationtest.CountPermutations(spec); n.Cmp(big.NewInt(maxPermutations)) > 0 {
				t.Fatalf("spec %s enumerates %s permutations, above the short-layer ceiling %d",
					spec.Name, n, maxPermutations)
			}
			if len(isolationtest.Permutations(spec)) == 0 {
				t.Fatalf("spec %s runs no permutation: an adjacency filter or a named list removed all of them", spec.Name)
			}
			var check isolationtest.Observer
			if sc.check != nil {
				check = sc.check()
			}
			r := &isolationtest.Runner{NewEngine: w.engine, Observe: check}
			isolationtest.Check(t, spec, r)
			for _, v := range r.Violations() {
				t.Errorf("property violated: %s", v)
			}
		})
	}
}

// TestCatalogueNeverBlocks asserts the F7 property over the whole catalogue from
// the golden transcripts themselves, and that every catalogue golden belongs to a
// scenario — a golden whose spec was renamed or removed would otherwise sit there
// asserting nothing.
func TestCatalogueNeverBlocks(t *testing.T) {
	t.Parallel()
	want := map[string]bool{}
	for _, sc := range catalogue() {
		name := sc.specName()
		want[name+".golden"] = true
		// #nosec G304 -- a fixed fixture directory and a spec name from the catalogue.
		b, err := os.ReadFile(filepath.Join(goldenDir, name+".golden"))
		if err != nil {
			t.Errorf("scenario %s %s has no golden: %v", sc.ID, name, err)
			continue
		}
		if n := blockedSteps(string(b)); n != 0 {
			t.Errorf("golden %s records %d blocked steps", name, n)
		}
	}
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if catalogueGolden.MatchString(e.Name()) && !want[e.Name()] {
			t.Errorf("%s/%s belongs to no catalogue scenario", goldenDir, e.Name())
		}
	}
}

// TestCatalogueSpecNamesAreUnique guards the golden-file mapping: two scenarios
// with one spec name would share, and overwrite, one golden.
func TestCatalogueSpecNamesAreUnique(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, sc := range catalogue() {
		n := sc.specName()
		if prev, dup := seen[n]; dup {
			t.Errorf("spec name %s is used by %s and %s", n, prev, sc.ID)
		}
		seen[n] = sc.ID
		if !strings.HasPrefix(n, strings.ToLower(sc.ID)+"-") {
			t.Errorf("spec %s does not start with its row ID %s", n, sc.ID)
		}
		if sc.Driver == driverLPG && !strings.HasSuffix(n, "-lpg") {
			t.Errorf("lpg-driver spec %s lacks the -lpg suffix", n)
		}
	}
}

// TestSK10AccessPaths proves SK10 compares what it claims to compare: the seek
// arm of each spec is planned as an index seek, and the scan arm is not. Without
// this a planner change could turn both arms into scans and SK10's seek = scan
// check would compare a plan against itself.
func TestSK10AccessPaths(t *testing.T) {
	t.Parallel()
	for _, build := range []func(*world) *isolationtest.Spec{sk10Hash, sk10Btree} {
		w := &world{}
		spec := build(w)
		t.Run(spec.Name, func(t *testing.T) {
			env, err := w.engine()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = env.Close() }()
			for _, st := range spec.Setup {
				res, err := env.Eng.RunInTxAny(context.Background(), st.Query, nil)
				if err != nil {
					t.Fatalf("setup %s: %v", st.Name, err)
				}
				_ = res.Close()
			}
			// The statement the spec actually runs, not a reconstruction of it: the
			// planner sees both arms in one plan, and that plan is what must hold
			// one index access (the seek arm) and one label scan (the scan arm).
			read := spec.Sessions[0].Steps[0].Query
			plan, err := env.Eng.Explain(read, nil)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(plan, "NodeByIndex"); n != 1 {
				t.Errorf("want exactly one index access (the seek arm) in the plan of %q, got %d:\n%s", read, n, plan)
			}
			if n := strings.Count(plan, "NodeByLabelScan"); n != 1 {
				t.Errorf("want exactly one label scan (the scan arm) in the plan of %q, got %d:\n%s", read, n, plan)
			}
		})
	}
}

// TestReadSkewNegativeControl validates the INSTRUMENT: the read-skew row must be
// able to report the anomaly it exists for.
//
// The seam is world.perKeySnapshot, which makes every lpg-driver read take a
// fresh snapshot instead of reading at its transaction's — each key read at its
// own instant, the defect class read skew (A5A) names. With the seam the row's
// property check must report violations, and name the interleavings in which the
// transfer committed between the reads; without it, none. The seam lives only in
// this example's code and is set only here.
func TestReadSkewNegativeControl(t *testing.T) {
	t.Parallel()
	run := func(seam bool) []string {
		w := &world{perKeySnapshot: seam}
		spec := sk12ReadSkewLPG(w)
		r := &isolationtest.Runner{NewEngine: w.engine, Observe: readSkewFree()}
		var buf bytes.Buffer
		if err := r.Run(context.Background(), spec, &buf); err != nil {
			t.Fatalf("run (seam=%v): %v", seam, err)
		}
		return r.Violations()
	}
	broken := run(true)
	if len(broken) == 0 {
		t.Fatal("with the per-key-snapshot seam the read-skew row reported no anomaly: " +
			"the check cannot detect the defect it exists for")
	}
	for _, v := range broken {
		t.Logf("seam on, as expected: %s", v)
	}
	// The interleaving that defines read skew: x read, the whole transfer
	// committed, y read.
	const canonical = "s1x s2dx s2cy s2c s1y s1x2 s1c"
	if !slices.ContainsFunc(broken, func(v string) bool { return strings.Contains(v, canonical) }) {
		t.Errorf("the canonical read-skew interleaving %q was not reported; got %v", canonical, broken)
	}
	if healthy := run(false); len(healthy) != 0 {
		t.Errorf("without the seam the read-skew row reported %d violations: %v", len(healthy), healthy)
	}
}
