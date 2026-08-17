package sim

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestCartesianNotification_BothDirections is the happy path: on a populated
// graph the advisory must be attached to both Cartesian spellings and to
// neither of the connected ones. It runs against the same fixture the
// pattern-shapes battery uses.
func TestCartesianNotification_BothDirections(t *testing.T) {
	a, _ := buildMotifFixture(t)
	if v := CheckCartesianNotification(0, a); len(v) > 0 {
		t.Fatalf("cartesian notification checker reported a violation on a conforming engine: %v", v)
	}
}

// TestCartesianNotification_EmptyGraph proves the checker is meaningful before
// any data exists: the advisory is a plan-time property of the query text, so
// every arm must hold on an empty graph too. This is what lets the scenario run
// it at tick 0 and straight after a crash.
func TestCartesianNotification_EmptyGraph(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	a := NewEngineAdapter(cypher.NewEngine(g))
	if v := CheckCartesianNotification(0, a); len(v) > 0 {
		t.Fatalf("cartesian notification checker reported a violation on an empty graph: %v", v)
	}
}

// TestCartesianNotification_ProbesCoverBothPolarities proves the probe table
// itself is discriminating: at least one shape must expect the advisory and at
// least one must forbid it. A table that drifted to a single polarity could be
// satisfied by an implementation that always warns, or never warns.
func TestCartesianNotification_ProbesCoverBothPolarities(t *testing.T) {
	var warn, quiet int
	for _, p := range cartesianNotificationProbes {
		if p.wantWarned {
			warn++
		} else {
			quiet++
		}
	}
	if warn == 0 || quiet == 0 {
		t.Fatalf("probe table is not discriminating: %d expect the advisory, %d forbid it", warn, quiet)
	}
}

// TestCartesianNotification_SensitivityToInvertedExpectation proves the checker
// FIRES in both directions. Each probe's expectation is inverted in turn — the
// engine is untouched — so a checker that could not tell the two apart would
// pass and fail this test.
func TestCartesianNotification_SensitivityToInvertedExpectation(t *testing.T) {
	a, _ := buildMotifFixture(t)
	for _, p := range cartesianNotificationProbes {
		p := p
		t.Run(p.label, func(t *testing.T) {
			inverted := p
			inverted.wantWarned = !p.wantWarned

			notes, err := queryNotifications(context.Background(), a, inverted.query)
			if err != nil {
				t.Fatalf("probe query failed: %v", err)
			}
			warned := false
			for _, n := range notes {
				if n.Code == cartesianProductNotificationCode {
					warned = true
					break
				}
			}
			if warned == inverted.wantWarned {
				t.Fatalf("the engine agreed with the INVERTED expectation for %q; the probe cannot "+
					"distinguish a warned query from an unwarned one", inverted.query)
			}
		})
	}
}

// facetlessResult is a [Result] that deliberately does NOT implement
// [notificationReporter], standing in for an adapter that stopped exposing the
// advisory surface.
type facetlessResult struct{}

func (facetlessResult) Next() bool               { return false }
func (facetlessResult) ScalarInt() (int64, bool) { return 0, false }
func (facetlessResult) IntAt(int) (int64, bool)  { return 0, false }
func (facetlessResult) StringAt(int) (string, bool) {
	return "", false
}
func (facetlessResult) RowCount() int { return 0 }
func (facetlessResult) Err() error    { return nil }
func (facetlessResult) Close() error  { return nil }

// TestCartesianNotification_RequiresTheReporterFacet proves the probe fails
// loudly when a result cannot surface notifications, rather than passing while
// checking nothing — the dead-arm failure mode this checker exists to avoid.
func TestCartesianNotification_RequiresTheReporterFacet(t *testing.T) {
	// The adapter the simulator actually uses must carry the facet.
	var _ notificationReporter = (*resultAdapter)(nil)

	notes, err := notificationsOf(facetlessResult{})
	if err == nil {
		t.Fatal("a result without the notification facet was accepted; every advisory probe would " +
			"then pass while checking nothing")
	}
	if notes != nil {
		t.Errorf("expected no notifications alongside the error, got %v", notes)
	}
	if !strings.Contains(err.Error(), "does not expose Notifications") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestCartesianNotification_CodeIsPinned guards the exact advisory code, which
// is the string a Bolt driver matches on. A silent rename would leave every
// downstream consumer's filter dead.
func TestCartesianNotification_CodeIsPinned(t *testing.T) {
	const want = "Neo.ClientNotification.Statement.CartesianProductWarning"
	if cartesianProductNotificationCode != want {
		t.Fatalf("advisory code = %q, want %q", cartesianProductNotificationCode, want)
	}
	a, _ := buildMotifFixture(t)
	notes, err := queryNotifications(context.Background(), a, "MATCH (a:Person),(b:Person) RETURN count(*)")
	if err != nil {
		t.Fatalf("cartesian query: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("got %d notifications, want exactly 1", len(notes))
	}
	if notes[0].Code != want {
		t.Errorf("code = %q, want %q", notes[0].Code, want)
	}
	if notes[0].Severity != "INFORMATION" || notes[0].Category != "PERFORMANCE" {
		t.Errorf("severity/category = %q/%q, want INFORMATION/PERFORMANCE", notes[0].Severity, notes[0].Category)
	}
	if !strings.Contains(notes[0].Description, "cartesian product") {
		t.Errorf("description does not mention the cartesian product: %q", notes[0].Description)
	}
}
