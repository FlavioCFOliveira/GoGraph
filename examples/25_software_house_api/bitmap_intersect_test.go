package main

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// TestBitmapIntersectDemo_BothRegimes is the deterministic core of the
// reportBitmapIntersect startup telemetry, asserted without any timing.
//
// It pins BOTH regimes, which is the point of the harness: a selective
// conjunction the planner serves set-at-a-time, and a nested one where the gate
// declines. Pinning only the first would let the example keep passing while the
// gate silently stopped discriminating — and it is the discrimination, not the
// win alone, that the example exists to make observable.
func TestBitmapIntersectDemo_BothRegimes(t *testing.T) {
	// A component scale that makes the Code layer dominate its Repository roots and
	// gives the cross-cutting review marker a meaningful population.
	ds := openScaledSeeded(t, synthScale{components: 2000, seed: 1})

	demo, err := collectBitmapIntersectDemo(ds.graph)
	if err != nil {
		t.Fatalf("collectBitmapIntersectDemo: %v", err)
	}

	// ── The FIRE regime ──────────────────────────────────────────────────────
	// :Component and :NeedsReview overlap without either containing the other, so
	// the intersection is strictly smaller than both and must be served as one AND.
	if !demo.fire.intersected {
		t.Errorf("selective conjunction was NOT intersected\nplan:\n%s", demo.fire.plan)
	}
	if !strings.Contains(demo.fire.plan, "∩") {
		t.Errorf("fire plan does not name intersected labels:\n%s", demo.fire.plan)
	}
	smallest := demo.fire.leftCount
	if demo.fire.rightCount < smallest {
		smallest = demo.fire.rightCount
	}
	if demo.fire.rows >= smallest {
		t.Errorf("fire regime is not selective: rows=%d must be < the smaller label's %d "+
			"(%s=%d, %s=%d) — the fixture no longer exercises the gate's condition",
			demo.fire.rows, smallest,
			demo.fire.leftLabel, demo.fire.leftCount, demo.fire.rightLabel, demo.fire.rightCount)
	}
	if demo.fire.rows <= 0 {
		t.Errorf("fire regime returned no rows; the contrast would be vacuous")
	}

	// ── The VETO regime ──────────────────────────────────────────────────────
	// :Repository is a strict subset of :Code, so the intersection equals the
	// smaller label: no rows left to remove, and the gate must decline.
	if demo.veto.intersected {
		t.Errorf("nested conjunction WAS intersected; the gate should decline when it "+
			"removes no rows\nplan:\n%s", demo.veto.plan)
	}
	if demo.veto.rows != demo.veto.rightCount {
		t.Errorf("veto regime: rows=%d, want %d (every %s is also %s)",
			demo.veto.rows, demo.veto.rightCount, demo.veto.rightLabel, demo.veto.leftLabel)
	}
	if demo.veto.rightCount >= demo.veto.leftCount {
		t.Errorf("veto fixture lost its nesting: %s=%d must be < %s=%d",
			demo.veto.rightLabel, demo.veto.rightCount, demo.veto.leftLabel, demo.veto.leftCount)
	}

	// The logical rendering must carry the exact estimate and its provenance —
	// that is what makes the plan evidence rather than decoration.
	if !strings.Contains(demo.fire.plan, "est. rows=") || !strings.Contains(demo.fire.plan, "exact") {
		t.Errorf("fire plan carries no exact cardinality estimate:\n%s", demo.fire.plan)
	}
}

// TestBitmapIntersectDemo_ResultIdentity confirms both demo queries return the
// identical answer with the path ON (default) and OFF, so the contrast
// reportBitmapIntersect prints compares like with like. The intersection is a
// physical substitution, never a semantic change.
func TestBitmapIntersectDemo_ResultIdentity(t *testing.T) {
	ds := openScaledSeeded(t, synthScale{components: 500, seed: 1})
	view, err := buildReviewView(ds.graph)
	if err != nil {
		t.Fatalf("buildReviewView: %v", err)
	}

	on := cypher.NewEngineWithOptions(view, cypher.EngineOptions{})
	off := cypher.NewEngineWithOptions(view, cypher.EngineOptions{DisableBitmapIntersection: true})

	for _, q := range []string{biFireQuery, biVetoQuery} {
		gotOn, err := biScalar(context.Background(), on, q)
		if err != nil {
			t.Fatalf("on: %v", err)
		}
		gotOff, err := biScalar(context.Background(), off, q)
		if err != nil {
			t.Fatalf("off: %v", err)
		}
		if gotOn != gotOff {
			t.Errorf("answer differs with the intersection on (%d) and off (%d) for:\n%s",
				gotOn, gotOff, q)
		}
	}
}

// TestBitmapIntersectDemo_ViewDoesNotMutateServedGraph pins the containment the
// harness relies on: the cross-cutting marker exists only in the derived view, so
// the served graph, its API surface and every fact this example's other tests pin
// stay exactly as seeded.
func TestBitmapIntersectDemo_ViewDoesNotMutateServedGraph(t *testing.T) {
	ds := openScaledSeeded(t, synthScale{components: 200, seed: 1})

	eng := cypher.NewEngine(ds.graph)
	before, err := mlsCountLabel(context.Background(), eng, biReviewLabel)
	if err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 0 {
		t.Fatalf("the served graph already carries %s on %d nodes; the demo label must "+
			"exist only in the derived view", biReviewLabel, before)
	}

	if _, err := buildReviewView(ds.graph); err != nil {
		t.Fatalf("buildReviewView: %v", err)
	}

	after, err := mlsCountLabel(context.Background(), eng, biReviewLabel)
	if err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != 0 {
		t.Errorf("deriving the review view added %s to %d served nodes; it must copy, "+
			"not mutate", biReviewLabel, after)
	}
}
