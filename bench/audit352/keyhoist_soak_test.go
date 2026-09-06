//go:build soak || nightly

package audit352_test

// keyhoist_soak_test.go — the EXACT allocation evidence for rmp #2662.
//
// # What it measures
//
// `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 0 LIMIT 10` used to
// project a hidden `p` so the Sort above could evaluate `p.salary` against it, and
// the projection's node-variable fast path turned that column into a full
// expr.NodeValue — labels AND the whole property bag — once per row. #2662 projects
// the KEY into its own hidden column instead, so the sort resolves it by schema
// lookup and the entity is never materialised.
//
// The claim is therefore structural, and it is asserted structurally: the frame
// cypher.upgradeNodeIDToValue must be ABSENT from an exact, bracketed
// runtime.MemProfile of the hoisted arm and PRESENT on the pre-#2662 arm. A share
// threshold would not do — it would pass on a path that merely got cheaper.
//
// # Why soak
//
// Same reason as sortprofile_soak_test.go: MemProfileRate=1 takes a stack walk on
// EVERY allocation, over the shared 120 000-node fixture, on both arms of every
// cell. See docs/test-layers.md.
//
//	go test -tags=soak ./bench/audit352/ -run TestSortKeyHoist -v
//
// # The arm seam and the plan cache
//
// sortseam.KeyHoistDisabled is read at TRANSLATE time and a translated plan is
// cached per Engine, so an arm cannot be selected by flipping the control around a
// call on a shared engine. Each arm gets its OWN engine, built and warmed with the
// control already set; see [keyHoistArmEngine].

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/internal/sortseam"
)

// keyHoistQuery is rmp #2662's reproduction, verbatim.
const keyHoistQuery = `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 0 LIMIT 10`

// keyHoistControl is the sort-free control over the SAME scan. It is the floor the
// sort shape is being driven towards: whatever tops this profile is what the sort
// shape costs once the sort's own share is gone.
const keyHoistControl = `MATCH (p:Person) RETURN p.firstName`

// frameNodeMaterialise is the frame whose presence IS the defect. It is reached
// only from the projection's node-variable fast path, so its absence is a proof
// that the projection stopped carrying the entity — not a correlation.
const frameNodeMaterialise = "cypher.upgradeNodeIDToValue"

// keyHoistArmEngine returns an Engine whose plan cache was populated under the arm
// selected by disabled. The control is restored before returning; the engine keeps
// the plans it translated under it.
func keyHoistArmEngine(tb testing.TB, disabled bool, warm []string) *cypher.Engine {
	tb.Helper()
	restore := sortseam.SetKeyHoistDisabled(disabled)
	defer restore()
	eng := cypher.NewEngine(benchGraph)
	for _, q := range warm {
		if _, err := eng.Explain(q, nil); err != nil {
			tb.Fatalf("warm Explain(%q) disabled=%v: %v", q, disabled, err)
		}
	}
	return eng
}

// keyHoistCell runs one query once under an exact bracketed profile and returns the
// attribution. The warm-up is OUTSIDE the window so one-off compilation cannot
// appear as a path difference.
func keyHoistCell(tb testing.TB, eng *cypher.Engine, query string, wantRows int) attribution {
	tb.Helper()
	if got := drainCounting(tb, eng, query); got != wantRows {
		tb.Fatalf("warm-up shipped %d rows, want %d", got, wantRows)
	}
	at := exerciseAttributed(tb, 1, func() {
		if got := drainCounting(tb, eng, query); got != wantRows {
			tb.Fatalf("shipped %d rows, want %d", got, wantRows)
		}
	})
	at.assertDescribesWindow(tb, query)
	return at
}

// TestSortKeyHoistNoiseFloor measures the instrument against ITSELF before any
// delta is attributed: the SAME arm, the SAME engine, alternating cells, same
// methodology as the A/B below. A difference smaller than what this reports is not
// a finding.
//
// It is NOT t.Parallel: it writes a process-global profile rate.
func TestSortKeyHoistNoiseFloor(t *testing.T) {
	eng := keyHoistArmEngine(t, false, []string{keyHoistQuery})

	const rounds = 4
	var objs []int64
	for i := 0; i < rounds*2; i++ {
		at := keyHoistCell(t, eng, keyHoistQuery, 10)
		objs = append(objs, at.totalObjects)
	}
	lo, hi := objs[0], objs[0]
	var sum int64
	for _, v := range objs {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
		sum += v
	}
	mean := float64(sum) / float64(len(objs))
	spread := 100 * float64(hi-lo) / mean
	t.Logf("NOISE FLOOR (hoisted arm vs itself, %d cells): objects %v", len(objs), objs)
	t.Logf("NOISE FLOOR: min=%d max=%d mean=%.1f spread=%.4f%% of the mean", lo, hi, mean, spread)

	// The instrument is exact, not sampled, so the spread is expected to be at or
	// near zero. A double-digit spread would mean the window is picking up work
	// that is not the query, and every delta below would be unattributable.
	if spread > 5.0 {
		t.Errorf("noise floor %.4f%% exceeds 5%%: the window is not describing the query alone, "+
			"so no A/B delta measured with it can be attributed", spread)
	}
}

// TestSortKeyHoistRemovesNodeMaterialisation is rmp #2662's acceptance criterion,
// asserted structurally and measured interleaved.
//
// Arms alternate A/B/B/A across rounds so neither is systematically first, and the
// two arms of a round run back to back so the drift between them is minimal.
//
// It is NOT t.Parallel: it writes a process-global profile rate.
func TestSortKeyHoistRemovesNodeMaterialisation(t *testing.T) {
	warm := []string{keyHoistQuery, keyHoistControl}
	off := keyHoistArmEngine(t, true, warm) // pre-#2662: entity passthrough
	on := keyHoistArmEngine(t, false, warm) // #2662: hoisted key column

	type arm struct {
		name string
		eng  *cypher.Engine
	}
	armOff, armOn := arm{"pre-2662", off}, arm{"hoisted", on}

	const rounds = 3
	var offObjs, onObjs []int64
	for r := 0; r < rounds; r++ {
		order := []arm{armOff, armOn}
		if r%2 == 1 {
			order = []arm{armOn, armOff}
		}
		for _, a := range order {
			at := keyHoistCell(t, a.eng, keyHoistQuery, 10)
			matObjs, matBytes := at.cum(frameNodeMaterialise)
			t.Logf("round %d arm=%-9s total %8d objects (%.2f/row over %d scanned)  %s cum %d objects, %d bytes",
				r, a.name, at.totalObjects, float64(at.totalObjects)/float64(nodeCount), nodeCount,
				shortFn(frameNodeMaterialise), matObjs, matBytes)

			switch a.name {
			case "pre-2662":
				offObjs = append(offObjs, at.totalObjects)
				// CONTROL / NON-VACUITY. Without this a zero on the hoisted arm
				// could mean the frame name, not the optimisation, changed.
				if matObjs == 0 {
					t.Fatalf("round %d: the pre-#2662 arm did NOT materialise a node — %s is absent "+
						"from the control window, so the seam did not select the old path and this "+
						"round compares the new path against itself", r, frameNodeMaterialise)
				}
			case "hoisted":
				onObjs = append(onObjs, at.totalObjects)
				// THE CRITERION: absence of the frame, not a share threshold.
				if matObjs != 0 {
					t.Errorf("round %d: the hoisted arm still reaches %s (%d objects, %d bytes): "+
						"the projection is still materialising the entity",
						r, frameNodeMaterialise, matObjs, matBytes)
				}
			}
		}
	}

	// The control shape, for the report: the floor the sort shape is driven towards.
	ctrl := keyHoistCell(t, on, keyHoistControl, nodeCount)
	ctrlObjs := ctrl.totalObjects

	t.Logf("SUMMARY over %d interleaved rounds (build: %s):", rounds, buildModeNote)
	t.Logf("  pre-#2662 arm : %v  -> %.2f objects/row", offObjs, perRow(offObjs, nodeCount))
	t.Logf("  hoisted arm   : %v  -> %.2f objects/row", onObjs, perRow(onObjs, nodeCount))
	t.Logf("  sort-free ctrl: %d -> %.2f objects/row", ctrlObjs, float64(ctrlObjs)/float64(nodeCount))
	t.Logf("  reduction     : %.2f%% of the pre-#2662 total",
		100*(1-perRow(onObjs, nodeCount)/perRow(offObjs, nodeCount)))
}

// buildModeNote records the one build fact an allocation count is not invariant
// under. -race disables the append-of-make optimisation, so a figure taken under
// one build mode does not transfer to the other.
const buildModeNote = "run BOTH arms in the same binary; state -race on/off beside any figure quoted from it"

// perRow is the mean of objs divided by rows.
func perRow(objs []int64, rows int) float64 {
	if len(objs) == 0 {
		return 0
	}
	var sum int64
	for _, v := range objs {
		sum += v
	}
	return float64(sum) / float64(len(objs)) / float64(rows)
}
