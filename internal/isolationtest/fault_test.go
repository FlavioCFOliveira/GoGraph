package isolationtest_test

// fault_test.go — the harness's negative control (rmp #2340 AC5).
//
// A VALIDATION INSTRUMENT THAT HAS NEVER BEEN SHOWN TO FAIL IS NOT EVIDENCE.
// Everything in specs_test.go passes; on its own that is equally consistent with
// "GoGraph's isolation is sound" and with "this harness cannot see an isolation
// fault at all". The tests here settle it by injecting a known fault, showing
// the harness reports it, removing the fault, and showing the report goes away.
//
// The fault is injected in the SCENARIO rather than in the engine, and that is
// deliberate. Patching production code to break isolation would test a build
// nobody ships; splitting the transfer across two transactions injects the
// defect the invariant is actually about — "the transfer was not atomic" — into
// a build that is otherwise bit-identical to the one under test. The reader,
// the invariant, the enumeration and the timing all stay exactly as they are in
// the passing case, so a difference in outcome can only come from the atomicity
// of the transfer.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/isolationtest"
)

// conservationObserver is the bank-transfer invariant, machine-checked: every
// `total` a reader observes must be the conserved 100.
//
// It asserts on the STRUCTURED rows, which is the same data the transcript
// renders, so an invariant can never quietly disagree with the golden file.
func conservationObserver(t *testing.T) isolationtest.Observer {
	t.Helper()
	return func(o isolationtest.Observation) error {
		if len(o.Cols) == 0 || o.Cols[0] != "total" {
			return nil
		}
		for _, row := range o.Rows {
			if row[0] != "100" {
				return fmt.Errorf("torn total: read %s, want the conserved 100", row[0])
			}
		}
		return nil
	}
}

// atomicTransferSpec is bank-transfer with the transfer inside ONE transaction:
// the correct scenario, which must never tear.
func atomicTransferSpec() *isolationtest.Spec { return bankTransferSpec() }

// tornTransferSpec is the SAME scenario with the fault injected: the debit and
// the credit are committed as two separate transactions, so the graph passes
// through a state in which 10 exists nowhere.
//
// A reader that lands between the two commits must see 90. That is not a bug in
// GoGraph — snapshot isolation is being asked to protect an invariant that spans
// two transactions, which it never promised to do — it is a KNOWN, deliberately
// non-atomic transfer, and it is exactly the observation an isolation harness
// has to be able to make.
func tornTransferSpec() *isolationtest.Spec {
	s := bankTransferSpec()
	s.Name = "bank-transfer-torn"
	s.Doc = "FAULT-INJECTED variant of bank-transfer, used only as the harness's negative\n" +
		"control. The transfer is split across TWO transactions, so the total is 90 for\n" +
		"anyone who reads between them."
	s.Sessions[0].Setup = nil
	s.Sessions[0].Steps = []isolationtest.Step{
		{Name: "s1dx", Ctl: isolationtest.Begin},
		{Name: "s1cy", Query: "MATCH (a:Account {id:'X'}) SET a.bal = a.bal - 10 RETURN a.bal AS bal"},
		{Name: "s1c", Ctl: isolationtest.Commit},
		{Name: "s1dx2", Ctl: isolationtest.Begin},
		{Name: "s1cy2", Query: "MATCH (a:Account {id:'Y'}) SET a.bal = a.bal + 10 RETURN a.bal AS bal"},
		{Name: "s1c2", Ctl: isolationtest.Commit},
	}
	// The reader must take a FRESH snapshot per read, or it would hold one taken
	// before the first commit and never observe the intermediate state at all —
	// which would make this control prove nothing.
	s.Sessions[1].Setup = nil
	s.Sessions[1].Steps = []isolationtest.Step{
		{Name: "s2t", Query: "MATCH (a:Account) RETURN sum(a.bal) AS total"},
		{Name: "s2t2", Query: "MATCH (a:Account) RETURN sum(a.bal) AS total"},
	}
	return s
}

// TestHarnessDetectsTornTotal is the negative control proper: the SAME
// invariant, the SAME reader and the SAME enumeration, run against the correct
// scenario and against the fault-injected one. It must be silent on the first
// and must fire on the second.
//
// Both halves are required. Passing only the first would show the harness is
// quiet, not that it can speak; passing only the second would show it fires,
// not that it discriminates.
func TestHarnessDetectsTornTotal(t *testing.T) {
	t.Parallel()

	t.Run("atomic/no violation", func(t *testing.T) {
		t.Parallel()
		r := &isolationtest.Runner{NewEngine: memEngine, Observe: conservationObserver(t)}
		_ = runToString(t, atomicTransferSpec(), r)
		if v := r.Violations(); len(v) > 0 {
			t.Errorf("the correct scenario reported %d conservation violations; it must report none:\n%s",
				len(v), strings.Join(v, "\n"))
		}
	})

	t.Run("torn/violation reported", func(t *testing.T) {
		t.Parallel()
		r := &isolationtest.Runner{NewEngine: memEngine, Observe: conservationObserver(t)}
		transcript := runToString(t, tornTransferSpec(), r)
		v := r.Violations()
		if len(v) == 0 {
			t.Fatalf("THE HARNESS IS BLIND: a transfer split across two transactions tore the total in no "+
				"interleaving, so nothing here proves the passing specs mean anything.\ntranscript:\n%s",
				transcript)
		}
		// It must catch the fault where the fault actually is — in the
		// interleavings that read between the two commits — and NOT everywhere,
		// which would mean the observer is simply always failing.
		if len(v) >= 2*countReads(transcript) {
			t.Errorf("every read was reported as a violation (%d), which is what an always-failing "+
				"observer looks like rather than a detected fault", len(v))
		}
		for _, line := range v {
			if !strings.Contains(line, "torn total: read 90") {
				t.Errorf("violation is not the expected torn read: %s", line)
			}
			if !strings.Contains(line, "permutation ") {
				t.Errorf("violation does not name a re-runnable permutation: %s", line)
			}
		}
		t.Logf("harness detected %d torn reads across the enumeration; first: %s", len(v), v[0])
	})
}

// countReads counts how many total-reading steps a transcript contains, so the
// always-failing-observer check above has something real to compare against.
func countReads(transcript string) int {
	return strings.Count(transcript, "step s2t:") + strings.Count(transcript, "step s2t2:")
}

// TestViolationNamesAReplayablePermutation closes the loop between the two
// halves of the harness: a violation names a permutation, and that permutation
// replayed ALONE must reproduce the same violation.
//
// Without this, a reported permutation name would be a label rather than a
// handle, and the re-runnability claim in the package doc would be untested.
func TestViolationNamesAReplayablePermutation(t *testing.T) {
	t.Parallel()
	spec := tornTransferSpec()
	r := &isolationtest.Runner{NewEngine: memEngine, Observe: conservationObserver(t)}
	_ = runToString(t, spec, r)
	v := r.Violations()
	if len(v) == 0 {
		t.Fatal("no violation to replay; see TestHarnessDetectsTornTotal")
	}
	name := permNameOf(t, v[0])

	replay := &isolationtest.Runner{NewEngine: memEngine, Observe: conservationObserver(t), Only: name}
	_ = runToString(t, spec, replay)
	if got := replay.Violations(); len(got) == 0 {
		t.Errorf("permutation %q violated the invariant in the full run but not when replayed alone; "+
			"the permutation name is therefore not a usable handle", name)
	}
}

// permNameOf extracts the permutation name from a violation line of the form
// `permutation "a b c" step s: …`.
func permNameOf(t *testing.T, violation string) string {
	t.Helper()
	const head = `permutation "`
	i := strings.Index(violation, head)
	if i < 0 {
		t.Fatalf("violation does not carry a permutation name: %s", violation)
	}
	rest := violation[i+len(head):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("violation has an unterminated permutation name: %s", violation)
	}
	return rest[:j]
}
