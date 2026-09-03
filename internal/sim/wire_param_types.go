package sim

// wire_param_types.go — Bolt-wire parameter type matrix (rmp #2462).
//
// The concurrent-mode wire actors bound only String and Integer parameters, so
// every other PackStream kind a real driver sends — Float, Boolean, Null, List,
// Map — crossed the wire in no DST scenario at all. That is the path on which a
// literal/parameter divergence actually reaches a user: the Bolt server hands
// the decoded parameter map straight to the engine
// (bolt/server/session.go: `params := map[string]any(m.Parameters)`, then
// `csess.RunAny(...)`), so a binding defect shows up for driver clients before
// it shows up anywhere else.
//
// This probe binds every kind over the REAL wire and verifies it by read-back,
// including a genuine Map parameter used as the property map of a CREATE — the
// RunAny-with-a-real-map path.
//
// # What round-trips
//
// Verified empirically against the wire (see [wireParamEchoProbe]):
//
//	String   → packstream String   ✔ round-trips
//	Integer  → packstream Integer  ✔ round-trips
//	Float    → packstream Float    ✔ round-trips
//	Boolean  → packstream Boolean  ✔ round-trips
//	Null     → packstream Null     ✔ round-trips
//	Map      → packstream Map      ✔ round-trips
//	List     → packstream List     ✔ round-trips (since rmp #2513)
//
// # The list arm (rmp #2462 found it, rmp #2513 fixed it)
//
// A LIST always bound correctly in both directions of evaluation — indexing,
// size, and equality against a literal list gave the right answers, so the
// engine really did receive a list — but the RECORD encoder had no
// expr.ListValue arm, so a list-valued column fell through to the `default` arm
// of bolt/server/session.go's exprValueToPackstream and was emitted as its
// String() rendering. That was the RECORD encoder, not parameter binding: a
// LITERAL list return (`RETURN [1,2,3]`) was stringified identically.
//
// #2513 added the arm, and [wireParamListProbe] now asserts BOTH halves live:
// the input semantics through defect-immune scalar projections, and the output
// encoding as a genuine packstream List whose elements keep their own types.
// The broader end-to-end matrix — nesting, entities, temporals, and the
// list-producing functions — lives in wire_list_encoding_test.go.

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// wireParamLabelPrefix prefixes the node label the parameter-matrix probe
// writes under. It is distinct from every workload label so the probe can clean
// up by label without touching the population.
//
// The full label is this prefix plus a PER-PROBE slot (see [wireParamSlotPool]).
//
// # Why the label is not fixed (rmp #2728)
//
// It was, and that was a defect. Several callers drive ONE shared [SimServer]
// concurrently — bench/contention's dst-concurrent-bolt runs `level` goroutines
// each calling [RunConcurrent], and every call probes before it spawns any
// connection — so with a fixed label and a fixed id, concurrent probes left many
// live nodes matching the same `MATCH (n:WireParam ...)`. Each probe's
// `SET n.s = $nul` and `DETACH DELETE n` then fanned out over every OTHER
// probe's node.
//
// Measured on this fixture, 64 concurrent probes over 1984 operations against
// one shared server: 2,028,689 graph/lpg delNodePropertyInfo calls, 1022.5 per
// operation, against 6.0 per operation at one probe at a time — a 170x work
// amplification produced by the FIXTURE, not by the engine, of which 97.9%
// removed nothing. The probe also reported 13,808 spurious divergences, because
// its own `count(*) == 1` and `count(*) == 0` assertions are false when another
// probe's node is live. Private per-probe fixtures remove the sharing at its
// root; see [wireParamFixture].
const wireParamLabelPrefix = "WireParam"

// wireParamSlots hands each in-flight probe a private fixture slot, so two
// probes that overlap in time never share a label or an id (rmp #2728).
//
// Slots are RECYCLED rather than handed out monotonically, so the number of
// distinct labels the engine ever registers is bounded by the peak number of
// SIMULTANEOUS probes rather than by the number of probes run. A monotone
// counter would register one label per probe and grow the engine's label
// registry without bound over a long run, which the module's bounded-resources
// rule forbids.
var wireParamSlots wireParamSlotPool

// wireParamSlotPool is a free list of fixture slot numbers. It is safe for
// concurrent use by any number of goroutines.
//
// A slot is returned to the free list only when the probe that held it VERIFIED
// its own cleanup. A probe that could not delete its node retires the slot
// instead: recycling it would hand the leftover node to the next probe and
// reintroduce exactly the cross-talk the pool exists to remove. Retirement is
// bounded by the number of failed cleanups, which on a healthy server is zero.
type wireParamSlotPool struct {
	mu   sync.Mutex
	free []int
	next int
}

// acquire returns a slot number no other in-flight probe holds.
func (p *wireParamSlotPool) acquire() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := len(p.free); n > 0 {
		slot := p.free[n-1]
		p.free = p.free[:n-1]
		return slot
	}
	slot := p.next
	p.next++
	return slot
}

// release returns slot to the free list. The caller must have verified that no
// node survives under the slot's label.
func (p *wireParamSlotPool) release(slot int) {
	p.mu.Lock()
	p.free = append(p.free, slot)
	p.mu.Unlock()
}

// wireParamFixture is one probe's private fixture: the label its node is created
// under and the id that node carries. Both carry the slot number, so a node that
// ever escapes cleanup names the probe that leaked it.
type wireParamFixture struct {
	slot  int
	label string
	id    string
}

// newWireParamFixture reserves a slot and derives the fixture from it. The
// caller must release the slot through [wireParamSlotPool.release] once, and
// only once, it has verified that nothing survives under the label.
func newWireParamFixture() wireParamFixture {
	slot := wireParamSlots.acquire()
	return wireParamFixture{
		slot:  slot,
		label: fmt.Sprintf("%s_%d", wireParamLabelPrefix, slot),
		id:    fmt.Sprintf("wp-%d", slot),
	}
}

// wireParamListStringRendering is the DEFECTIVE encoding a list column had
// before rmp #2513: its String() rendering instead of a PackStream List. It is
// retained only so [wireParamListEncodingProbe] can name the regression it
// guards against by its exact shape — arriving as this string again means the
// expr.ListValue arm has been lost.
const wireParamListStringRendering = "[1, 2, 3]"

// probeWireParamTypes binds every PackStream parameter kind over a fresh Bolt
// connection to srv and verifies each by read-back. It returns one description
// per divergence; an empty slice means the whole matrix round-tripped as
// specified in the file comment.
//
// The probe is population-NEUTRAL: it creates exactly one node and deletes it
// again before returning, so the caller's eventual-consistency oracle
// (acknowledged creates vs engine node count) is unaffected. It runs on the
// caller's goroutine before any connection spawns.
//
// It is also ISOLATED: its fixture is private to this call (see
// [wireParamFixture]), so any number of probes may run concurrently against one
// shared srv without touching each other's nodes. Both properties are needed —
// neutrality alone let concurrent probes fan out across each other (rmp #2728).
func probeWireParamTypes(ctx context.Context, srv *SimServer) []string {
	c, err := srv.Dial()
	if err != nil {
		return []string{fmt.Sprintf("wire param probe: dial: %v", err)}
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(ctx); err != nil {
		return []string{fmt.Sprintf("wire param probe: connect: %v", err)}
	}

	var fails []string
	q := func(label, query string, params map[string]any) ([]*proto.Record, bool) {
		recs, err := wireQuery(c, query, params)
		if err != nil {
			fails = append(fails, fmt.Sprintf("%s: %v (query: %s)", label, err, query))
			return nil, false
		}
		return recs, true
	}

	fx := newWireParamFixture()
	fails = append(fails, wireParamEchoProbe(q)...)
	fails = append(fails, wireParamListProbe(q)...)
	fails = append(fails, wireParamMapCreateProbe(q, fx)...)
	cleanupFails, cleaned := wireParamCleanup(q, fx)
	fails = append(fails, cleanupFails...)
	if cleaned {
		wireParamSlots.release(fx.slot)
	}
	return fails
}

// wireQueryFn is the bound query helper the probe stages share: it runs one
// statement over the wire, reporting failure through the shared accumulator and
// returning ok=false when the statement did not complete.
type wireQueryFn func(label, query string, params map[string]any) ([]*proto.Record, bool)

// wireParamEchoProbe binds one parameter of every scalar kind plus a Map and
// echoes them straight back, asserting each arrives as its native PackStream
// type. This is the pure binding round-trip: no storage, no evaluation.
func wireParamEchoProbe(q wireQueryFn) []string {
	params := map[string]any{
		"s":   "hello",
		"i":   int64(42),
		"f":   2.5,
		"b":   true,
		"nul": nil,
		"m":   map[string]any{"k": int64(9), "s": "v"},
		"l":   []any{int64(1), "two", nil},
	}
	recs, ok := q("wire param echo",
		"RETURN $s AS s, $i AS i, $f AS f, $b AS b, $nul AS nul, $m AS m, $l AS l", params)
	if !ok {
		return nil
	}
	if len(recs) != 1 {
		return []string{fmt.Sprintf("wire param echo: got %d rows, want 1", len(recs))}
	}
	want := []any{
		"hello",
		int64(42),
		2.5,
		true,
		nil,
		map[string]any{"k": int64(9), "s": "v"},
		[]any{int64(1), "two", nil},
	}
	return compareWireRow("wire param echo", recs[0].Data, want,
		[]string{"String", "Integer", "Float", "Boolean", "Null", "Map", "List"})
}

// wireParamListProbe verifies a List parameter in two parts. Its INPUT
// semantics are checked through scalar projections — indexing, size, and
// equality against a literal list — which prove the engine received a list
// without depending on how a list is encoded back. Its OUTPUT encoding is then
// asserted to be a genuine PackStream List (rmp #2513); see the file comment.
func wireParamListProbe(q wireQueryFn) []string {
	var fails []string
	list := []any{int64(1), int64(2), int64(3)}

	recs, ok := q("wire param list semantics",
		"RETURN $l[0] AS first, size($l) AS n, $l = [1,2,3] AS eq", map[string]any{"l": list})
	if ok {
		if len(recs) != 1 {
			fails = append(fails, fmt.Sprintf("wire param list semantics: got %d rows, want 1", len(recs)))
		} else {
			fails = append(fails, compareWireRow("wire param list semantics", recs[0].Data,
				[]any{int64(1), int64(3), true},
				[]string{"List index", "List size", "List equality vs literal"})...)
		}
	}

	fails = append(fails, wireParamListEncodingProbe(q, list)...)
	return fails
}

// wireParamListEncodingProbe asserts the wire encoding of a list-valued column
// is a genuine PackStream List whose elements keep their own types (rmp #2513).
// The concrete Go type IS the encoding: a String carrying the same text is the
// defect this probe exists to catch, so it is named explicitly in the failure.
func wireParamListEncodingProbe(q wireQueryFn, list []any) []string {
	recs, ok := q("wire param list encoding", "RETURN $l AS l", map[string]any{"l": list})
	if !ok {
		return nil
	}
	if len(recs) != 1 || len(recs[0].Data) != 1 {
		return []string{"wire param list encoding: expected exactly one column in one row"}
	}
	if s, isString := recs[0].Data[0].(string); isString {
		return []string{fmt.Sprintf(
			"wire param list encoding: a list column arrived as the packstream String %q (the pre-#2513 "+
				"rendering was %q) — the expr.ListValue arm in bolt/server/session.go "+
				"exprValueToPackstream has been LOST",
			s, wireParamListStringRendering)}
	}
	return compareWireRow("wire param list encoding", recs[0].Data, []any{list}, []string{"List"})
}

// wireParamMapCreateProbe drives a genuine Map parameter through the autocommit
// RunAny path as the property map of a CREATE, then reads every property back
// over the wire and asserts its native type. It also binds a Float and a
// Boolean parameter as pattern-predicate seek keys (literal/parameter parity on
// the access path) and a Null parameter through SET, which must remove the
// property rather than store a null.
func wireParamMapCreateProbe(q wireQueryFn, fx wireParamFixture) []string {
	var fails []string
	props := map[string]any{
		"id": fx.id,
		"f":  2.5,
		"b":  true,
		"s":  "x",
		"i":  int64(7),
		"l":  []any{int64(1), int64(2), int64(3)},
	}
	if _, ok := q("wire param map CREATE",
		"CREATE (n:"+fx.label+" $props)", map[string]any{"props": props}); !ok {
		return fails
	}

	// Read every scalar property back with its native type.
	recs, ok := q("wire param property read-back",
		"MATCH (n:"+fx.label+" {id:$id}) RETURN n.f AS f, n.b AS b, n.s AS s, n.i AS i",
		map[string]any{"id": fx.id})
	if ok {
		if len(recs) != 1 {
			fails = append(fails, fmt.Sprintf("wire param property read-back: got %d rows, want 1", len(recs)))
		} else {
			fails = append(fails, compareWireRow("wire param property read-back", recs[0].Data,
				[]any{2.5, true, "x", int64(7)},
				[]string{"Float property", "Boolean property", "String property", "Integer property"})...)
		}
	}

	// A stored list must compare equal to the same list rebound as a parameter,
	// AND come back over the wire as a PackStream List (rmp #2513) — the
	// equality check alone would pass even with a stringifying encoder.
	recs, ok = q("wire param stored list equality",
		"MATCH (n:"+fx.label+" {id:$id}) RETURN n.l = $l AS eq",
		map[string]any{"id": fx.id, "l": []any{int64(1), int64(2), int64(3)}})
	if ok {
		fails = append(fails, expectSingleWireValue("wire param stored list equality", recs, true)...)
	}
	recs, ok = q("wire param stored list read-back",
		"MATCH (n:"+fx.label+" {id:$id}) RETURN n.l AS l", map[string]any{"id": fx.id})
	if ok {
		fails = append(fails, expectSingleWireValue("wire param stored list read-back", recs,
			[]any{int64(1), int64(2), int64(3)})...)
	}

	// Float and Boolean parameters as pattern-predicate seek keys: the shape a
	// literal/parameter divergence would break first.
	recs, ok = q("wire param float seek",
		"MATCH (n:"+fx.label+" {f:$f}) RETURN count(*) AS c", map[string]any{"f": 2.5})
	if ok {
		fails = append(fails, expectSingleWireValue("wire param float seek", recs, int64(1))...)
	}
	recs, ok = q("wire param bool seek",
		"MATCH (n:"+fx.label+" {b:$b}) RETURN count(*) AS c", map[string]any{"b": true})
	if ok {
		fails = append(fails, expectSingleWireValue("wire param bool seek", recs, int64(1))...)
	}

	// A Null parameter through SET removes the property.
	recs, ok = q("wire param null SET",
		"MATCH (n:"+fx.label+" {id:$id}) SET n.s = $nul RETURN n.s IS NULL AS gone",
		map[string]any{"id": fx.id, "nul": nil})
	if ok {
		fails = append(fails, expectSingleWireValue("wire param null SET", recs, true)...)
	}
	return fails
}

// wireParamCleanup removes the probe's node and asserts none survives under its
// private label, so the caller's node-count oracle sees a net-zero effect from
// the whole probe.
//
// It reports cleaned=true only when the check actually ran and saw an empty
// label. The caller keys slot recycling on that: an unverified cleanup retires
// the slot rather than handing a possibly-live node to the next probe. A failed
// statement is reported through q's accumulator, so returning no descriptions
// here never loses the diagnosis.
func wireParamCleanup(q wireQueryFn, fx wireParamFixture) (fails []string, cleaned bool) {
	if _, ok := q("wire param cleanup", "MATCH (n:"+fx.label+") DETACH DELETE n", nil); !ok {
		return nil, false
	}
	recs, ok := q("wire param cleanup check", "MATCH (n:"+fx.label+") RETURN count(*) AS c", nil)
	if !ok {
		return nil, false
	}
	fails = expectSingleWireValue("wire param cleanup check", recs, int64(0))
	return fails, len(fails) == 0
}

// expectSingleWireValue asserts recs holds exactly one row whose single column
// equals want.
func expectSingleWireValue(label string, recs []*proto.Record, want any) []string {
	if len(recs) != 1 || len(recs[0].Data) != 1 {
		return []string{fmt.Sprintf("%s: expected exactly one column in one row, got %d row(s)", label, len(recs))}
	}
	return compareWireRow(label, recs[0].Data, []any{want}, []string{"value"})
}

// compareWireRow compares a RECORD's fields against the expected packstream
// values, reporting one description per divergence. Both the VALUE and its
// concrete Go type are compared, because the type IS the wire encoding: an
// Integer arriving as a String is a defect even when the text matches.
func compareWireRow(label string, got, want []any, kinds []string) []string {
	if len(got) != len(want) {
		return []string{fmt.Sprintf("%s: got %d columns, want %d", label, len(got), len(want))}
	}
	var fails []string
	for i := range want {
		if reflect.DeepEqual(got[i], want[i]) {
			continue
		}
		fails = append(fails, fmt.Sprintf(
			"%s: %s column %d: got %T %#v, want %T %#v",
			label, kinds[i], i, got[i], got[i], want[i], want[i]))
	}
	return fails
}

// wireQuery runs one statement over the wire and returns its records, turning a
// Bolt FAILURE into an error so the caller need not classify terminals.
func wireQuery(c *WireClient, query string, params map[string]any) ([]*proto.Record, error) {
	resp, err := c.Run(query, params)
	if err != nil {
		return nil, fmt.Errorf("RUN: %w", err)
	}
	if f, isFail := resp.(*proto.Failure); isFail {
		return nil, fmt.Errorf("RUN refused: %s %s", f.Code, f.Message)
	}
	recs, term, err := c.PullAll()
	if err != nil {
		return nil, fmt.Errorf("PULL: %w", err)
	}
	if f, isFail := term.(*proto.Failure); isFail {
		return nil, fmt.Errorf("PULL refused: %s %s", f.Code, f.Message)
	}
	return recs, nil
}
