package exec

// dbhits_cell_test.go — the four renderings of a db-hits figure (rmp #2760).
//
// [DbHitsCell] and [DbHitsTotalCell] are the single place every PROFILE surface
// turns the (figure, known) pair into text — the indented tree here, the columnar
// table in cypher/explain, and both Engine methods in cypher. A defect in either
// reaches all of them at once, and neither is reachable through a real query in
// every state (no plan the engine builds today has a complete db-hits total), so
// they are pinned directly.
//
// The total's four cases are Neo4j's, transcribed from renderSummary.scala at tag
// 5.26.16 over InternalPlanDescription.TotalHits — not recalled:
//
//	case TotalHits(0, false) => "0"
//	case TotalHits(0, true)  => "?"
//	case TotalHits(x, false) => x.toString
//	case TotalHits(x, true)  => s"$x + ?"
//
// Layer: short.

import "testing"

// TestDbHitsCell_DistinguishesAZeroFromNoFigure is the whole point of the change
// in one assertion: a counted zero and an uncounted figure must not render alike.
func TestDbHitsCell_DistinguishesAZeroFromNoFigure(t *testing.T) {
	t.Parallel()

	if got := DbHitsCell(0, true); got != "0" {
		t.Errorf("DbHitsCell(0, known) = %q, want \"0\": an operator that provably read "+
			"nothing reports a measurement, and collapsing it into the unknown state "+
			"would lose exactly as much information as the zero it replaced", got)
	}
	if got := DbHitsCell(0, false); got != DbHitsUnknown {
		t.Errorf("DbHitsCell(0, unknown) = %q, want %q", got, DbHitsUnknown)
	}
	if DbHitsCell(0, true) == DbHitsCell(0, false) {
		t.Error("a counted zero and an uncounted figure render identically; the column " +
			"is back to the state rmp #2720 §1 measured and rmp #2760 removed")
	}
	if got := DbHitsCell(2000, true); got != "2000" {
		t.Errorf("DbHitsCell(2000, known) = %q, want \"2000\"", got)
	}
	// A figure carried alongside known=false is meaningless and must never surface,
	// however it got there.
	if got := DbHitsCell(2000, false); got != DbHitsUnknown {
		t.Errorf("DbHitsCell(2000, unknown) = %q, want %q — the int64 has to hold "+
			"something, and the renderer must not treat that something as a count",
			got, DbHitsUnknown)
	}
}

// TestDbHitsTotalCell_MatchesTheFourCases pins the total's rendering, including
// the "x + ?" form that is the reason the state is carried to the summary line at
// all: it neither hides the figure the engine does have nor lets a floor be read
// as the whole query's cost.
func TestDbHitsTotalCell_MatchesTheFourCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		total     int64
		uncertain bool
		want      string
		why       string
	}{
		{0, false, "0", "nothing was read, and every operator said so"},
		{0, true, DbHitsUnknown, "nothing countable was read and something was not counted"},
		{40, false, "40", "a complete total"},
		{40, true, "40 + " + DbHitsUnknown, "at least 40, plus an unknown amount"},
	}
	for _, c := range cases {
		if got := DbHitsTotalCell(c.total, c.uncertain); got != c.want {
			t.Errorf("DbHitsTotalCell(%d, %v) = %q, want %q — %s",
				c.total, c.uncertain, got, c.want, c.why)
		}
	}

	// The two zero cases and the two forty cases must each be distinguishable, or
	// the flag is carried to the summary line for nothing.
	if DbHitsTotalCell(0, false) == DbHitsTotalCell(0, true) {
		t.Error("a complete zero total and an entirely uncounted one render identically")
	}
	if DbHitsTotalCell(40, false) == DbHitsTotalCell(40, true) {
		t.Error("a complete total of 40 and a floor of 40 render identically, so a " +
			"reader cannot tell a query's cost from a lower bound on it")
	}
}
