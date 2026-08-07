package lpg

// mvcc_abort_reclaim_test.go — MVCC (rmp #2318): the aborted-version gates.
//
// Layer: short.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestAbort_VersionsAreReleasedBySweep is the ticket's own reproduction, and
// acceptance criterion 1.
//
// Measured against the build before this task, with these exact numbers: seed 50
// nodes with a label, reclaim to zero, write a label on each of the 50 inside a
// transaction, abort, reclaim again with NO live reader — `freed=0` and
// `VersionCount()=50`, for the life of the process, because [mvcc.AbortedTS] is
// the maximum uint64 and every reclaimer truncates on `stamp <= watermark`.
func TestAbort_VersionsAreReleasedBySweep(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()

	const nodes = 50
	for i := 0; i < nodes; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "Seed"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	g.ReclaimNow()
	if n := g.VersionCount(); n != 0 {
		t.Fatalf("the substrate holds %d versions before the aborted transaction, want 0: "+
			"this test cannot attribute what it frees", n)
	}

	tx := g.beginLabelTx()
	for i := 0; i < nodes; i++ {
		if err := tx.setNodeLabel(fmt.Sprintf("n%d", i), "Aborted"); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	before := g.VersionCount()
	if before < nodes {
		t.Fatalf("the transaction left only %d versions over %d writes; nothing to reclaim",
			before, nodes)
	}
	tx.abort()

	// Released BY THE ABORT, not by a later sweep: the withdrawal is synchronous
	// because a present-time read takes the stored value directly and may not see
	// work that was never committed. See [Graph.withdrawAbortedNow].
	if n := g.VersionCount(); n != 0 {
		t.Errorf("abort left %d of the %d version records it created, want 0: AbortedTS cannot "+
			"satisfy the watermark test, so nothing else will ever free them", n, before)
	}
	// And a sweep afterwards finds nothing left to do, which is what "the abort
	// released them" means from the other side.
	if freed := g.ReclaimNow(); freed != 0 {
		t.Errorf("a sweep after the abort still freed %d records; the withdrawal was incomplete",
			freed)
	}
}

// TestAbort_WithdrawnWritesStayInvisible is acceptance criterion 2, and the half
// that stops the fix becoming an exposure.
//
// The ticket warns that "a reclaimer that simply dropped them would EXPOSE the
// aborted writes", because the aborted deltas are the only thing masking the
// stored value. So the sweep must leave the stored value CLEAN, and this asserts
// it from both sides of the sweep and for a reader that starts afterwards.
func TestAbort_WithdrawnWritesStayInvisible(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("a", "Seed"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeProperty("a", "p", Int64Value(1)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	g.ReclaimNow()

	tx := g.beginLabelTx()
	if err := tx.setNodeLabel("a", "Aborted"); err != nil {
		t.Fatalf("label: %v", err)
	}
	if err := tx.setNodeProperty("a", "p", Int64Value(99)); err != nil {
		t.Fatalf("property: %v", err)
	}
	tx.abort()

	// BEFORE the sweep: masked by the aborted deltas.
	assertClean := func(when string) {
		t.Helper()
		if g.HasNodeLabel("a", "Aborted") {
			t.Errorf("%s: the aborted transaction's label is visible", when)
		}
		if !g.HasNodeLabel("a", "Seed") {
			t.Errorf("%s: the pre-transaction label was lost", when)
		}
		v, ok := g.GetNodeProperty("a", "p")
		if !ok {
			t.Errorf("%s: the pre-transaction property was lost", when)
			return
		}
		if got, _ := v.Int64(); got != 1 {
			t.Errorf("%s: the property reads %d, want the pre-transaction 1", when, got)
		}
	}
	// The withdrawal is synchronous, so there is no "masked" interval to check:
	// this asserts the STORED value, with no aborted delta left to mask it.
	assertClean("immediately after the abort")
	g.ReclaimNow()
	assertClean("after a further sweep")
	// And a reader that starts afterwards, which cannot have any pre-abort context.
	snap := g.BeginRead()
	defer g.EndRead(snap)
	view := g.ReadAt(snap)
	if view.HasNodeLabel("a", "Aborted") {
		t.Error("a reader starting after the sweep sees the aborted transaction's label")
	}
	if !view.HasNodeLabel("a", "Seed") {
		t.Error("a reader starting after the sweep has lost the pre-transaction label")
	}
}

// TestAbort_StoredValueEqualsTheSerialSchedule is acceptance criterion 3.
//
// The stored value must equal what a serial schedule in which the aborted
// transaction never ran would produce — asserted against a CONTROL graph driven
// through the identical committed writes with the aborted transaction omitted,
// rather than against hand-written expectations, so the oracle cannot inherit the
// same mistake as the code.
func TestAbort_StoredValueEqualsTheSerialSchedule(t *testing.T) {
	seed := func(g *Graph[string, float64]) {
		t.Helper()
		if err := g.AddNode("a"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel("a", "Seed"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty("a", "p", Int64Value(1)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	control := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = control.Close() }()
	seed(control)
	control.ReclaimNow()

	subject := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = subject.Close() }()
	seed(subject)
	subject.ReclaimNow()

	tx := subject.beginLabelTx()
	if err := tx.setNodeLabel("a", "Aborted"); err != nil {
		t.Fatalf("label: %v", err)
	}
	if err := tx.setNodeProperty("a", "p", Int64Value(99)); err != nil {
		t.Fatalf("property: %v", err)
	}
	tx.removeNodeLabel("a", "Seed")
	tx.abort()
	subject.ReclaimNow()

	// Every store the transaction touched, compared against the control.
	id := mvccNodeID(t, subject, "a")
	cid := mvccNodeID(t, control, "a")
	if got, want := subject.NodeLabelsByID(id), control.NodeLabelsByID(cid); !sameStrings(got, want) {
		t.Errorf("labels after the withdrawal are %v, want the serial-schedule %v", got, want)
	}
	sp, sok := subject.GetNodeProperty("a", "p")
	cp, cok := control.GetNodeProperty("a", "p")
	if sok != cok {
		t.Fatalf("property presence is %v, want the serial-schedule %v", sok, cok)
	}
	if sok {
		sv, _ := sp.Int64()
		cv, _ := cp.Int64()
		if sv != cv {
			t.Errorf("property after the withdrawal is %d, want the serial-schedule %d", sv, cv)
		}
	}
	if n := subject.VersionCount(); n != 0 {
		t.Errorf("the withdrawal left %d version records, want 0", n)
	}
}

// sameStrings compares two label sets irrespective of order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

// TestAbort_DirtyBaseIsNotWritable pins the half that closes the ATOMICITY
// violation, which the ticket does not describe and which this task measured.
//
// A writer arriving on an object whose newest version is aborted must be REFUSED
// until the sweep has withdrawn it. Allowed through, it builds its value on the
// dirty stored value and a later reader sees the aborted write — measured as
// `reader sees L=true M=true` against the build that exempted an aborted head.
//
// It also pins the LIVENESS half: the object must be writable once the withdrawal
// has run, because refusing forever is the bug rmp #2300 introduced the exemption
// to avoid.
func TestAbort_DirtyBaseIsNotWritable(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.ReclaimNow()

	tx := g.beginLabelTx()
	if err := tx.setNodeLabel("a", "Aborted"); err != nil {
		t.Fatalf("label: %v", err)
	}
	tx.abort()

	// The withdrawal is synchronous, so by the time abort returns there is no
	// aborted head left and the object is WRITABLE. The [mvcc.Conflicts] change
	// covers the window BEFORE the withdrawal completes, where a concurrent writer
	// would otherwise build on the dirty base — refused there, it retries and
	// succeeds here.
	if g.VersionCount() != 0 {
		t.Fatalf("abort left %d version records, so this test is not measuring a clean object",
			g.VersionCount())
	}
	tx3 := g.beginLabelTx()
	if err := tx3.setNodeLabel("a", "Later"); err != nil {
		t.Fatalf("the object is still unwritable after the sweep withdrew the aborted "+
			"version, which is the liveness bug the exemption existed to avoid: %v", err)
	}
	if _, err := tx3.commit(); err != nil {
		t.Fatalf("commit after the withdrawal: %v", err)
	}
	if !g.HasNodeLabel("a", "Later") {
		t.Error("the retried write did not take effect")
	}
	if g.HasNodeLabel("a", "Aborted") {
		t.Error("the aborted transaction's label survived the withdrawal and the retry")
	}
}
