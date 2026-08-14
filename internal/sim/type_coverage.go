package sim

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TypedWriter is the type-coverage actor: it emits [tmplCreateTyped] CREATEs
// whose parameters span every round-tripping Cypher property kind — string,
// integer, float, boolean, list, a plain ISO-8601 string, and all six genuine
// temporal types — with values drawn deterministically from the seed. The
// oracle records the full property set so the type-coverage checker can verify
// each kind survives commit and crash/recovery.
//
// # Concurrency contract
//
// TypedWriter is NOT safe for concurrent use; it is invoked from the single
// simulation goroutine.
type TypedWriter struct{ counter int64 }

// Name returns the actor's identifier.
func (*TypedWriter) Name() string { return "TypedWriter" }

// typedDateDay returns the day-of-month a Typed node's `d` property carries.
// The stride of 7 over a 28-day month makes the date order DIFFER from the id
// order (ids 0,4 share day 1; 1,5 share day 8; …), which is what makes
// [CheckTypedTemporalOrder] load-bearing: an ORDER BY that silently fell back
// to insertion or id order would produce a different sequence.
func typedDateDay(id int64) int { return 1 + int((id*7)%28) }

// typedTimeOffsets are the zone offsets (seconds east of UTC) the `tm`
// property cycles through, so the TIME round-trip covers a positive, a
// negative, and the UTC zone rendering rather than only "Z".
var typedTimeOffsets = [3]int{0, 3600, -3600}

// NextOp returns the next Typed-node CREATE with a unique id and a value of every
// supported kind, all seed-derived so the op stream is a pure function of the
// seed. A pointer receiver carries the monotone id counter across calls.
//
// The six temporal properties are bound as genuine temporal expr values, so the
// engine stores them in its kind-tagged form and they read back as temporals;
// `ts` stays a plain ISO-8601 STRING and is the control that must read back as
// a string (rmp #2457).
func (w *TypedWriter) NextOp(seed *Seed, _ *GraphOracle) Op {
	id := w.counter
	w.counter++
	// Plain ISO-8601 STRING (NOT a temporal): the deliberate control.
	ts := fmt.Sprintf("2026-01-%02dT%02d:%02d:00Z", 1+int(id%28), int(id%24), int(seed.IntN(60)))
	return Op{
		Kind:   OpCreate,
		Cypher: tmplCreateTyped,
		Params: map[string]any{
			"id":  id,
			"s":   fmt.Sprintf("str-%d", id),
			"i":   int64(seed.IntN(1_000_000)),
			"f":   float64(seed.IntN(1_000_000)) / 1000.0,
			"b":   seed.IntN(2) == 0,
			"lst": []any{id, int64(seed.IntN(100)), int64(seed.IntN(100))},
			"ts":  ts,
			"d":   expr.NewDate(2026, 1, typedDateDay(id)),
			"ldt": expr.NewLocalDateTime(2026, 1+int(id%12), 1+int(id%28), int(id%24), int(id%60), int(id%60), 0),
			"dt":  expr.NewDateTime(2026, 1+int(id%12), 1+int(id%28), int(id%24), int(id%60), 0, 0, time.UTC),
			"lt":  expr.NewLocalTime(int(id%24), int(id%60), int(id%60), 0),
			"tm":  expr.NewTime(int(id%24), int(id%60), 0, 0, typedTimeOffsets[id%3]),
			"du":  expr.NewDuration(id%13, id%29, int64(seed.IntN(3600)), 0),
		},
	}
}

// typedExpectKind is the expr KIND every [typedPropKeys] property must read
// back as. It is the assertion that makes the type-coverage arm non-vacuous
// for temporals (rmp #2457): a temporal that degraded to an untagged
// PropString reads back as [expr.KindString] carrying the SAME text, so only
// the kind distinguishes a working round-trip from a broken one. `ts` is
// deliberately KindString — it is the plain-ISO control that proves the
// assertion discriminates instead of accepting anything.
var typedExpectKind = map[string]expr.Kind{
	"id":     expr.KindInteger,
	"s":      expr.KindString,
	"i":      expr.KindInteger,
	"f":      expr.KindFloat,
	"b":      expr.KindBool,
	"lst":    expr.KindList,
	"ts":     expr.KindString,
	"d":      expr.KindDate,
	"ldt":    expr.KindLocalDateTime,
	"dt":     expr.KindDateTime,
	"lt":     expr.KindLocalTime,
	"tm":     expr.KindTime,
	"du":     expr.KindDuration,
	"absent": expr.KindNull,
}

// typeCoverageWorkload is a 100% TypedWriter mix: every op creates a Typed node
// carrying a value of each supported kind.
func typeCoverageWorkload(_ *Seed) *Workload {
	return &Workload{Actors: []Actor{&TypedWriter{}}, Weights: []float64{1.0}}
}

// CheckTypedProperties reads every modelled Typed node back through the real
// engine read path and asserts, for every property, BOTH that its value
// round-trips to the modelled value (compared via the canonical expr.Value
// String() rendering) AND that it reads back with the expected expr KIND
// ([typedExpectKind]) — that a never-set property reads NULL, and that the node
// exists at all. It runs on a quiescent graph (the deterministic loop, including
// immediately after crash/recovery), so a divergence means a property failed to
// round-trip, changed type, or did not survive recovery.
//
// The kind assertion is what makes the temporal arm honest (rmp #2457). Before
// it, temporals were written as ISO-8601 STRINGS and compared as strings, so the
// arm could not distinguish a working temporal round-trip from a broken one:
// both sides said the same text. A temporal that degrades to an untagged
// PropString now fails on kind even though its text is unchanged.
func CheckTypedProperties(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	var vs []Violation
	cols := make([]string, len(typedPropKeys))
	for i, k := range typedPropKeys {
		cols[i] = "n." + k
	}
	proj := strings.Join(cols, ", ")
	ctx := context.Background()

	for _, id := range oracle.TypedIDs() {
		props, ok := oracle.TypedNode(id)
		if !ok {
			continue
		}
		q := fmt.Sprintf("MATCH (n:Typed {id:%d}) RETURN %s", id, proj)
		got, err := engine.projectRowValues(ctx, q, len(typedPropKeys))
		if err != nil {
			vs = append(vs, Violation{
				Kind: ViolationGraphIntegrity, Tick: tick, Op: "typed property read",
				Message: fmt.Sprintf("Typed{id:%d}: read failed: %v", id, err),
			})
			continue
		}
		if got == nil {
			vs = append(vs, Violation{
				Kind: ViolationACIDDurability, Tick: tick, Op: "typed node existence",
				Message: fmt.Sprintf("committed Typed{id:%d} absent in engine (did not survive recovery)", id),
			})
			continue
		}
		vs = append(vs, checkTypedRow(tick, id, props, got)...)
	}
	return vs
}

// checkTypedRow adjudicates one Typed node's projected row against the oracle's
// modelled property map: the expr KIND first (a type degradation is reported as
// such even when the text still matches), then the canonical value.
func checkTypedRow(tick, id int64, props map[string]any, got []expr.Value) []Violation {
	var vs []Violation
	for i, k := range typedPropKeys {
		gotV := got[i]
		if gotV == nil {
			vs = append(vs, Violation{
				Kind: ViolationGraphIntegrity, Tick: tick, Op: "typed property value",
				Message: fmt.Sprintf("Typed{id:%d}.%s: engine returned no value", id, k),
			})
			continue
		}
		if want, ok := typedExpectKind[k]; ok && gotV.Kind() != want {
			vs = append(vs, Violation{
				Kind: ViolationOracleDeviation, Tick: tick, Op: "typed property kind",
				Message: fmt.Sprintf("Typed{id:%d}.%s has kind %v (value %s), want kind %v"+
					" — the value DEGRADED to another type (a temporal read back as a plain"+
					" string means its storage tag was lost)", id, k, gotV.Kind(), gotV.String(), want),
			})
			continue
		}
		want := "null" // "absent" is never set, and any modelled-nil reads NULL
		if k != "absent" {
			want = canonicalValueString(props[k])
		}
		if gotV.String() != want {
			vs = append(vs, Violation{
				Kind: ViolationOracleDeviation, Tick: tick, Op: "typed property value",
				Message: fmt.Sprintf("Typed{id:%d}.%s = %s, want %s (kind did not round-trip)", id, k, gotV.String(), want),
			})
		}
	}
	return vs
}

// CheckTypedTemporalOrder asserts that ORDER BY over a TEMPORAL property agrees
// with an ordering the oracle computes itself from the temporals it modelled.
// The engine is asked for the Typed ids ordered by (n.d, n.id); the oracle sorts
// its own modelled ([expr.DateValue], id) pairs and the two id sequences must be
// identical.
//
// This is a genuine oracle, not a self-check: the dates are laid out with a
// stride ([typedDateDay]) that makes date order differ from id order, so an
// engine that ignored the temporal ordering — or compared the tagged storage
// strings byte-wise — would produce a different sequence. Fewer than two
// modelled nodes cannot order anything, so the check reports nothing.
func CheckTypedTemporalOrder(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	ids := oracle.TypedIDs()
	if len(ids) < 2 {
		return nil
	}
	type dated struct {
		d  expr.DateValue
		id int64
	}
	model := make([]dated, 0, len(ids))
	for _, id := range ids {
		props, ok := oracle.TypedNode(id)
		if !ok {
			continue
		}
		dv, ok := props["d"].(expr.DateValue)
		if !ok {
			return []Violation{{
				Kind: ViolationGraphIntegrity, Tick: tick, Op: "typed temporal order",
				Message: fmt.Sprintf("oracle models Typed{id:%d}.d as %T, want expr.DateValue", id, props["d"]),
			}}
		}
		model = append(model, dated{d: dv, id: id})
	}
	sort.Slice(model, func(i, j int) bool {
		a, b := model[i], model[j]
		if a.d.Year != b.d.Year {
			return a.d.Year < b.d.Year
		}
		if a.d.Month != b.d.Month {
			return a.d.Month < b.d.Month
		}
		if a.d.Day != b.d.Day {
			return a.d.Day < b.d.Day
		}
		return a.id < b.id
	})
	want := make([]string, len(model))
	for i, m := range model {
		want[i] = strconv.FormatInt(m.id, 10)
	}

	rows, err := engine.queryRowStrings(context.Background(),
		"MATCH (n:Typed) RETURN n.id ORDER BY n.d, n.id", 1)
	if err != nil {
		return []Violation{{
			Kind: ViolationGraphIntegrity, Tick: tick, Op: "typed temporal order",
			Message: fmt.Sprintf("ORDER BY temporal read failed: %v", err),
		}}
	}
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r[0]
	}
	if !equalStrings(got, want) {
		return []Violation{{
			Kind: ViolationOracleDeviation, Tick: tick, Op: "typed temporal order",
			Message: fmt.Sprintf("ORDER BY n.d, n.id produced ids %v, oracle-computed temporal order is %v", got, want),
		}}
	}
	return nil
}

// typeCoverageScenario verifies the property type system under the DST: a
// workload creates Typed nodes carrying a value of every round-tripping kind
// (string/int/float/bool/list/ISO-string + all six genuine temporal types + a
// NULL-reading absent key), and [CheckTypedProperties] confirms each kind
// round-trips WITH ITS KIND INTACT and — with crash/recovery injected —
// survives recovery. It is bit-reproducible.
//
// Checkpointing is enabled (rmp #2457) so the temporals are proven across BOTH
// durable paths: a crash before the next checkpoint recovers them by replaying
// the WAL, and a crash after one recovers them from the published snapshot (the
// checkpoint truncates the WAL prefix it folded, so the snapshot is the only
// source for the folded ops). A tag lost by either serialiser would surface as
// a kind violation at the post-recovery check.
func typeCoverageScenario() Scenario {
	return Scenario{
		Name:        ScenarioTypeCoverage,
		Description: "property type system: string/int/float/bool/list/6 temporal kinds round-trip + survive crash/recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x7A9E5,
		MaxTicks:    400,
		Workload:    typeCoverageWorkload,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 70.0, StabilityWindow: 25},
		Checkpoint:  CheckpointConfig{Enabled: true, Every: 45},
		run:         runTypeCoverage,
	}
}

// typeCoverageCheckEvery is the tick cadence for the periodic typed-property
// check inside [runTypeCoverage].
const typeCoverageCheckEvery = 60

// runTypeCoverage drives the type-coverage safety loop: it creates Typed nodes,
// checks property round-trip AND kind periodically and immediately after every
// crash/recovery (the DST-unique value — the kinds are validated against a graph
// that survived real recovery), runs the temporal ORDER BY oracle on the same
// cadence, publishes real checkpoints so recovery alternates between the WAL and
// the snapshot path, and ends with a terminal check. It is deterministic.
func runTypeCoverage(ctx context.Context, seed uint64) (*SimReport, error) {
	sm, report, err := runTypeCoverageSim(ctx, seed)
	if sm != nil {
		defer func() { _ = sm.Close() }()
	}
	return report, err
}

// runTypeCoverageSim is [runTypeCoverage] with the simulator handed back to the
// caller instead of closed, so a test can assert on what the run actually
// exercised — that crashes AND checkpoints really fired, and that the typed
// nodes really exist. The caller owns closing the returned simulator, which is
// non-nil whenever construction succeeded (even when a violation is reported).
func runTypeCoverageSim(ctx context.Context, seed uint64) (*Simulator, *SimReport, error) {
	sc := typeCoverageScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("sim: type-coverage new: %w", err)
	}

	var lastTick int64
	var lastOp Op
	for i := 0; i < cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return sm, nil, err
		}
		tick := sm.clock.Tick()

		crashesBefore := sm.crashCount
		if report, err := sm.maybeCrash(ctx, tick); err != nil {
			return sm, nil, err
		} else if report != nil {
			return sm, report, nil
		}
		if sm.crashCount > crashesBefore {
			// Validate every typed property — value AND kind — against the
			// crash-recovered graph, plus the temporal ordering, so a tag lost by
			// the WAL or the snapshot serialiser fails the run right here.
			if v := CheckTypedProperties(tick, sm.oracle, sm.engine); len(v) > 0 {
				rep := sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery typed check>"}, v)
				return sm, rep, nil
			}
			if v := CheckTypedTemporalOrder(tick, sm.oracle, sm.engine); len(v) > 0 {
				rep := sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery temporal order>"}, v)
				return sm, rep, nil
			}
		}
		// A real checkpoint folds the committed prefix into a snapshot and
		// truncates the WAL, so the NEXT crash recovers the temporals through the
		// snapshot path rather than by WAL replay.
		if err := sm.maybeCheckpoint(tick); err != nil {
			return sm, nil, err
		}

		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed := sm.execute(ctx, op)
		sm.applyToOracle(op, committed)
		lastTick, lastOp = tick, op

		if tick%int64(sm.cfg.CheckEvery) == 0 {
			if v := sm.checker.Check(tick, sm.oracle, sm.engine); len(v) > 0 {
				rep := sm.report(tick, op, v)
				return sm, rep, nil
			}
		}
		if tick%typeCoverageCheckEvery == 0 {
			if v := CheckTypedProperties(tick, sm.oracle, sm.engine); len(v) > 0 {
				rep := sm.report(tick, op, v)
				return sm, rep, nil
			}
			if v := CheckTypedTemporalOrder(tick, sm.oracle, sm.engine); len(v) > 0 {
				rep := sm.report(tick, op, v)
				return sm, rep, nil
			}
		}
	}
	// Terminal typed + temporal-ordering check.
	if v := CheckTypedProperties(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		rep := sm.report(lastTick, lastOp, v)
		return sm, rep, nil
	}
	if v := CheckTypedTemporalOrder(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		rep := sm.report(lastTick, lastOp, v)
		return sm, rep, nil
	}
	return sm, nil, nil
}
