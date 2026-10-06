package main

// catalogue.go — the deterministic MVCC scenario catalogue (rmp #2933).
//
// Every scenario is an isolationtest spec run over EVERY order-preserving
// interleaving of its steps (or, where the catalogue says so, over named
// interleavings) and pinned by a golden transcript under testdata/. The rows,
// their sources and their expected snapshot-isolation outcomes come from
// docs/mvcc-scenario-catalogue.md §1.1 to §1.8 and §5 (§1.3 and §1.4 in
// catalogue_mgix.go, §1.5 to §1.7 in catalogue_ddhz.go, §1.8, the §5 rows
// FP01-FP03 and the frontier rows SE02, SE04 and SE03's fsync arm in
// catalogue_rigg.go); README.md maps each row to its
// PostgreSQL or InnoDB source and states where GoGraph's outcome differs and why.
//
// # Two drivers over one graph (H3)
//
// Each scenario runs through the Cypher engine. Where the scenario's shape exists
// in the lpg API — property reads and writes, label removal and property delete
// inside an explicit transaction — a second spec drives the SAME graph through
// lpg.Graph directly, with Probe and Hook steps. The engine under test is shared;
// what differs is the surface, so a defect that lives in one surface and not the
// other shows up as a disagreement between the two transcripts.
//
// A row whose shape needs ROLLBACK runs through the Cypher driver only: the
// Cypher engine owns the undo log that ExplicitTx.Rollback replays.
//
// # The vacuum drain (H1)
//
// An aborted version head conflicts until the background vacuum withdraws it
// (catalogue F5), so a write that follows a peer's ROLLBACK races a goroutine.
// Every such row places a drain step — a Hook that calls lpg.Graph.ReclaimNow —
// in the rolling-back session immediately after its ROLLBACK, and runs only the
// interleavings in which no other session's step falls between the two
// ([adjacentOnly]). The drain is therefore at a fixed point in every run, and the
// golden records the engine's answer rather than the vacuum's timing.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/internal/isolationtest"
)

// Driver names, as they appear in the telemetry and in the spec names.
const (
	driverCypher = "cypher"
	driverLPG    = "lpg"
)

// world is the per-permutation environment a scenario's Go steps reach.
//
// The isolationtest runner builds a FRESH engine for every permutation through
// [world.engine]; the Hook and Probe steps of a spec read the graph of the
// permutation in force through w.g. The runner is sequential across
// permutations and hands every step to its session goroutine over a channel, so
// the write in engine() happens before every read of w.g a step makes.
//
// A world is NOT safe for concurrent use by two runners.
type world struct {
	g *lpg.Graph[string, float64]
	// eng is the Cypher engine of the permutation in force, for the Go steps that
	// drive it directly: a sessionless read, a statement cancelled mid-flight, a
	// client-side retry loop (catalogue_ddhz.go).
	eng *cypher.Engine
	// parallelScanThreshold, when non-zero, is the engine's
	// cypher.EngineOptions.ParallelScanThreshold: RO03 lowers it so a small
	// fixture takes the morsel-parallel count path.
	parallelScanThreshold int
	// held is the statement a cancelMidStatement step has in flight, parked in
	// the cat.hold procedure; nil when there is none.
	held *heldStatement
	// resets run when a permutation's engine is built, and closers when it is
	// closed: the per-permutation state of the Go steps beyond the lpg sessions.
	resets  []func()
	closers []func()
	// sessions are the lpg-driver session states of the spec, reset for every
	// permutation so no transaction or snapshot survives into the next one.
	sessions []*lpgSession
	// perKeySnapshot is the negative-control seam: when set, every lpg-driver
	// read takes a FRESH snapshot instead of reading at its transaction's
	// snapshot, which is the defect class "each key is read at its own instant".
	// Set only by the negative-control test; see TestReadSkewNegativeControl.
	perKeySnapshot bool
}

// engine is the isolationtest.EngineFactory: a new graph and a Cypher engine
// over it, and every lpg-driver session state reset.
func (w *world) engine() (*isolationtest.Engine, error) {
	g := newGraph()
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{ParallelScanThreshold: w.parallelScanThreshold})
	if err := w.registerHold(eng); err != nil {
		return nil, errors.Join(err, eng.Close())
	}
	w.g, w.eng = g, eng
	for _, s := range w.sessions {
		s.reset()
	}
	for _, r := range w.resets {
		r()
	}
	return &isolationtest.Engine{
		Eng: eng,
		Close: func() error {
			// A statement parked in cat.hold, or a transaction left open by a
			// permutation, would pin a goroutine or a horizon slot; end them before
			// the graph so nothing is leaked.
			w.releaseHeld()
			for _, s := range w.sessions {
				s.abandon()
			}
			for _, c := range w.closers {
				c()
			}
			return eng.Close()
		},
	}, nil
}

// drain is the H1 step: settle every version no reader can reach, aborted heads
// included, at the point in the interleaving where the step sits.
func (w *world) drain(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<vacuum drain>", Hook: func(context.Context) error {
		w.g.ReclaimNow()
		return nil
	}}
}

// ---------------------------------------------------------------------------
// Cypher step constructors.

func q(name, query string) isolationtest.Step { return isolationtest.Step{Name: name, Query: query} }

func begin(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Ctl: isolationtest.Begin}
}

func beginRead(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Ctl: isolationtest.BeginRead}
}

func commit(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Ctl: isolationtest.Commit}
}

func rollback(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Ctl: isolationtest.Rollback}
}

// steps is a readability helper for a session's step list.
func steps(s ...isolationtest.Step) []isolationtest.Step { return s }

// ---------------------------------------------------------------------------
// The lpg driver.

// lpgSession is one scripted actor driving the graph through the lpg API. Its
// steps run on the isolationtest session goroutine, one at a time, so its state
// needs no lock of its own.
type lpgSession struct {
	w    *world
	sess *lpg.Session[string, float64]
	tx   lpg.WriteTx
	// snap is the read snapshot a beginRead step holds, nil when there is none.
	snap *lpg.Snapshot
}

func (w *world) lpgSession() *lpgSession {
	s := &lpgSession{w: w}
	w.sessions = append(w.sessions, s)
	return s
}

func (s *lpgSession) reset() { s.sess, s.tx, s.snap = nil, lpg.WriteTx{}, nil }

// abandon ends whatever the session still holds. A write transaction ended here
// publishes, which is harmless: the graph is discarded right after.
func (s *lpgSession) abandon() {
	if s.tx.Valid() {
		s.sess.EndVersionedTx(s.tx)
	}
	if s.snap != nil {
		s.w.g.EndRead(s.snap)
	}
	s.reset()
}

// begin opens a multi-statement write transaction through a lpg.Session.
func (s *lpgSession) begin(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "lpg: Session.BeginVersionedTx", Hook: func(context.Context) error {
		if s.tx.Valid() {
			return errors.New("lpg session already has an open transaction")
		}
		s.sess = s.w.g.NewSession()
		tx, err := s.sess.BeginVersionedTx()
		if err != nil {
			return err
		}
		s.tx = tx
		return nil
	}}
}

// commit ends the open write transaction. A transaction whose conflict was
// RECORDED rather than returned (a void primitive, F3) is refused here with that
// conflict, before Session.EndVersionedTx aborts its commit record — which is
// the order cypher's ExplicitTx.Commit observes too.
func (s *lpgSession) commit(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "lpg: Session.EndVersionedTx", Hook: func(context.Context) error {
		if !s.tx.Valid() {
			return errors.New("lpg session has no open transaction")
		}
		tx := s.tx
		s.tx = lpg.WriteTx{}
		cerr := tx.Err()
		s.sess.EndVersionedTx(tx)
		return cerr
	}}
}

// write runs fn as one statement: inside the open write transaction when there
// is one, otherwise as its own autocommit transaction.
func (s *lpgSession) write(ctx context.Context, fn func(lpg.WriteView[string, float64]) error) error {
	g := s.w.g
	if s.tx.Valid() {
		return g.ApplyInVersionedTx(ctx, s.tx, func(tx lpg.WriteTx) error { return fn(g.Writer(tx)) })
	}
	return g.ApplyVersionedCtx(ctx, func(tx lpg.WriteTx) error { return fn(g.Writer(tx)) })
}

// set writes key.prop = v.
func (s *lpgSession) set(name, key, prop string, v int64) isolationtest.Step {
	label := fmt.Sprintf("lpg: SetNodeProperty(%s, %s, %d)", key, prop, v)
	return isolationtest.Step{Name: name, Label: label, Hook: func(ctx context.Context) error {
		return s.write(ctx, func(wv lpg.WriteView[string, float64]) error {
			return wv.SetNodeProperty(key, prop, lpg.Int64Value(v))
		})
	}}
}

// add writes key.prop = key.prop + delta, reading through the transaction's own
// view — the read-modify-write a correct embedder performs.
func (s *lpgSession) add(name, key, prop string, delta int64) isolationtest.Step {
	label := fmt.Sprintf("lpg: SetNodeProperty(%s, %s, %s + %d)", key, prop, prop, delta)
	return isolationtest.Step{Name: name, Label: label, Hook: func(ctx context.Context) error {
		g := s.w.g
		return s.write(ctx, func(wv lpg.WriteView[string, float64]) error {
			cur, ok := g.WriterViewOf(wv.Tx()).GetNodeProperty(key, prop)
			if !ok {
				return fmt.Errorf("%s.%s is absent", key, prop)
			}
			n, _ := cur.Int64()
			return wv.SetNodeProperty(key, prop, lpg.Int64Value(n+delta))
		})
	}}
}

// removeLabel removes a node label: a void primitive, whose conflict is recorded
// on the transaction rather than returned.
func (s *lpgSession) removeLabel(name, key, lbl string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: fmt.Sprintf("lpg: RemoveNodeLabel(%s, %s)", key, lbl),
		Hook: func(ctx context.Context) error {
			return s.write(ctx, func(wv lpg.WriteView[string, float64]) error { return wv.RemoveNodeLabel(key, lbl) })
		}}
}

// delProp deletes a node property: a void primitive.
func (s *lpgSession) delProp(name, key, prop string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: fmt.Sprintf("lpg: DelNodeProperty(%s, %s)", key, prop),
		Hook: func(ctx context.Context) error {
			return s.write(ctx, func(wv lpg.WriteView[string, float64]) error { return wv.DelNodeProperty(key, prop) })
		}}
}

// read returns key.prop for each "key.prop" named, at the session's instant: the
// read snapshot it holds, its write transaction's own view or — outside both — a
// snapshot taken for this one read.
//
// Under the perKeySnapshot seam every key is read at a FRESH snapshot instead.
func (s *lpgSession) read(name string, refs ...string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "lpg: read " + strings.Join(refs, ", "),
		Probe: func(context.Context) ([]string, [][]string, error) {
			g := s.w.g
			row := make([]string, len(refs))
			for i, ref := range refs {
				key, prop, _ := strings.Cut(ref, ".")
				var (
					v  lpg.PropertyValue
					ok bool
				)
				switch {
				case s.w.perKeySnapshot:
					snap := g.BeginRead()
					v, ok = g.GetNodePropertyAsOf(key, prop, snap)
					g.EndRead(snap)
				case s.snap != nil:
					v, ok = g.GetNodePropertyAsOf(key, prop, s.snap)
				case s.tx.Valid():
					v, ok = g.WriterViewOf(s.tx).GetNodeProperty(key, prop)
				default:
					snap := g.BeginRead()
					v, ok = g.GetNodePropertyAsOf(key, prop, snap)
					g.EndRead(snap)
				}
				row[i] = renderProperty(v, ok)
			}
			return refs, [][]string{row}, nil
		}}
}

// renderProperty renders a property the way the Cypher driver renders a value,
// so the two drivers' transcripts read alike.
func renderProperty(v lpg.PropertyValue, ok bool) string {
	if !ok {
		return "null"
	}
	if n, isInt := v.Int64(); isInt {
		return strconv.FormatInt(n, 10)
	}
	if s, isStr := v.String(); isStr {
		return s
	}
	if b, isBool := v.Bool(); isBool {
		return strconv.FormatBool(b)
	}
	return fmt.Sprintf("<kind %d>", v.Kind())
}

// lpgNode is one node an lpg-driver fixture creates.
type lpgNode struct {
	key    string
	labels []string
	props  map[string]int64
}

// lpgFixture is a spec Setup step that builds nodes through the lpg API, keyed by
// name. Each node also carries a `name` property equal to its key, so a Cypher
// step can find it with {name: '<key>'}: the two drivers address the same node.
func (w *world) lpgFixture(nodes ...lpgNode) isolationtest.Step {
	return isolationtest.Step{Name: "mk", Label: "<lpg fixture>", Hook: func(context.Context) error {
		return w.g.ApplyVersioned(func(tx lpg.WriteTx) error {
			wv := w.g.Writer(tx)
			for _, n := range nodes {
				if err := wv.AddNode(n.key); err != nil {
					return err
				}
				if err := wv.SetNodeProperty(n.key, "name", lpg.StringValue(n.key)); err != nil {
					return err
				}
				for _, l := range n.labels {
					if err := wv.SetNodeLabel(n.key, l); err != nil {
						return err
					}
				}
				keys := make([]string, 0, len(n.props))
				for k := range n.props {
					keys = append(keys, k)
				}
				slices.Sort(keys)
				for _, k := range keys {
					if err := wv.SetNodeProperty(n.key, k, lpg.Int64Value(n.props[k])); err != nil {
						return err
					}
				}
			}
			return nil
		})
	}}
}

// ---------------------------------------------------------------------------
// Interleaving selection.

// adjacentOnly restricts s to the enumerated interleavings in which, for every
// pair, the second step runs IMMEDIATELY after the first — no other session's
// step between them. It is how a vacuum drain is held at a fixed point after a
// ROLLBACK (H1): the interleavings it removes are exactly the ones whose outcome
// would depend on when the background vacuum woke.
func adjacentOnly(s *isolationtest.Spec, pairs ...[2]string) *isolationtest.Spec {
	var kept [][]string
	for _, p := range isolationtest.Permutations(s) {
		ok := true
		for _, pair := range pairs {
			i := slices.Index(p.Steps, pair[0])
			if i < 0 || i+1 >= len(p.Steps) || p.Steps[i+1] != pair[1] {
				ok = false
				break
			}
		}
		if ok {
			kept = append(kept, p.Steps)
		}
	}
	s.Permutations = kept
	return s
}

// perms splits space-separated permutations into the form Spec.Permutations
// takes.
func perms(ps ...string) [][]string {
	out := make([][]string, len(ps))
	for i, p := range ps {
		out[i] = strings.Fields(p)
	}
	return out
}

// ---------------------------------------------------------------------------
// The catalogue.

// scenario is one catalogue row driven through one driver.
type scenario struct {
	// ID is the catalogue row (docs/mvcc-scenario-catalogue.md).
	ID string
	// Driver is driverCypher or driverLPG.
	Driver string
	// build returns the spec, bound to w.
	build func(w *world) *isolationtest.Spec
	// check, when non-nil, is a property every completed step must satisfy. It
	// is built per run because a property spanning steps keeps state.
	check func() isolationtest.Observer
}

// catalogue returns every implemented row, in catalogue order.
func catalogue() []scenario {
	return []scenario{
		{ID: "WW02", Driver: driverCypher, build: ww02Cypher},
		{ID: "WW02", Driver: driverLPG, build: ww02LPG},
		{ID: "WW03", Driver: driverCypher, build: ww03Cypher},
		{ID: "WW03", Driver: driverLPG, build: ww03LPG},
		{ID: "WW04", Driver: driverCypher, build: ww04Commit},
		{ID: "WW04", Driver: driverCypher, build: ww04UpdateRollsBack},
		{ID: "WW04", Driver: driverCypher, build: ww04DeleteRollsBack},
		{ID: "WW04", Driver: driverCypher, build: ww04UpdateRollsBackExplicit},
		{ID: "WW04", Driver: driverCypher, build: ww04DeleteRollsBackExplicit},
		{ID: "WW05", Driver: driverCypher, build: ww05Commit},
		{ID: "WW05", Driver: driverCypher, build: ww05RollsBack},
		{ID: "WW06", Driver: driverCypher, build: ww06},
		{ID: "WW07", Driver: driverCypher, build: ww07},
		{ID: "WW08", Driver: driverCypher, build: ww08Cypher},
		{ID: "WW08", Driver: driverLPG, build: ww08LPG},
		{ID: "WW09", Driver: driverCypher, build: ww09Cypher},
		{ID: "WW09", Driver: driverLPG, build: ww09LPG},
		{ID: "WW10", Driver: driverCypher, build: ww10Cypher},
		{ID: "WW10", Driver: driverLPG, build: ww10LPG},
		{ID: "WW11", Driver: driverCypher, build: func(w *world) *isolationtest.Spec { return ww11Ring(w, 3) }},
		{ID: "WW11", Driver: driverCypher, build: func(w *world) *isolationtest.Spec { return ww11Ring(w, 8) }},
		{ID: "WW12", Driver: driverCypher, build: ww12},
		{ID: "WW12", Driver: driverCypher, build: ww12Explicit},
		{ID: "WW13", Driver: driverCypher, build: ww13Label},
		{ID: "WW13", Driver: driverLPG, build: ww13LabelLPG},
		{ID: "WW13", Driver: driverCypher, build: ww13PropertyDelete},
		{ID: "WW13", Driver: driverLPG, build: ww13PropertyDeleteLPG},
		{ID: "WW13", Driver: driverCypher, build: ww13EdgeProperty},
		{ID: "WW14", Driver: driverCypher, build: ww14},
		{ID: "WW15", Driver: driverCypher, build: ww15, check: ww15Check},
		{ID: "SK02", Driver: driverCypher, build: sk02},
		{ID: "SK03", Driver: driverCypher, build: sk03},
		{ID: "SK04", Driver: driverCypher, build: sk04},
		{ID: "SK05", Driver: driverCypher, build: sk05Cypher},
		{ID: "SK05", Driver: driverLPG, build: sk05LPG},
		{ID: "SK07", Driver: driverCypher, build: sk07},
		{ID: "SK08", Driver: driverCypher, build: sk08},
		{ID: "SK10", Driver: driverCypher, build: sk10Hash, check: seekEqualsScan},
		{ID: "SK10", Driver: driverCypher, build: sk10Btree, check: seekEqualsScan},
		{ID: "SK11", Driver: driverCypher, build: sk11Increment},
		{ID: "SK11", Driver: driverCypher, build: sk11ValuePreserving},
		{ID: "SK12", Driver: driverCypher, build: sk12, check: repeatableCount},
		{ID: "SK12", Driver: driverLPG, build: sk12ReadSkewLPG, check: readSkewFree},
		{ID: "SK13", Driver: driverCypher, build: sk13Cypher},
		{ID: "SK13", Driver: driverLPG, build: sk13LPG},
		{ID: "SK14", Driver: driverCypher, build: sk14, check: fastPathEqualsScan},
		{ID: "MG01", Driver: driverCypher, build: mg01},
		{ID: "MG02", Driver: driverCypher, build: mg02Commit},
		{ID: "MG02", Driver: driverCypher, build: mg02RollsBack},
		{ID: "MG02", Driver: driverCypher, build: mg02Autocommit},
		{ID: "MG03", Driver: driverCypher, build: mg03},
		{ID: "MG04", Driver: driverCypher, build: mg04},
		{ID: "MG05", Driver: driverCypher, build: mg05NoConstraint},
		{ID: "MG05", Driver: driverCypher, build: mg05Unique},
		{ID: "MG06", Driver: driverCypher, build: mg06},
		{ID: "MG07", Driver: driverCypher, build: mg07},
		{ID: "MG08", Driver: driverCypher, build: mg08},
		{ID: "MG09", Driver: driverCypher, build: mg09SameTx},
		{ID: "MG09", Driver: driverCypher, build: mg09AfterCommit},
		{ID: "MG09", Driver: driverCypher, build: mg09PeerRollsBack},
		{ID: "MG10", Driver: driverCypher, build: mg10},
		{ID: "MG12", Driver: driverCypher, build: mg12InTx},
		{ID: "MG12", Driver: driverCypher, build: mg12PeerRemoval},
		{ID: "IX01", Driver: driverCypher, build: ix01, check: seekEqualsScan},
		{ID: "IX02", Driver: driverCypher, build: ix02, check: seekEqualsScan},
		{ID: "IX03", Driver: driverCypher, build: ix03, check: seekEqualsScan},
		{ID: "IX04", Driver: driverCypher, build: ix04Hash, check: seekEqualsScan},
		{ID: "IX04", Driver: driverCypher, build: ix04Btree, check: seekEqualsScan},
		{ID: "IX05", Driver: driverCypher, build: ix05, check: seekEqualsScan},
		{ID: "IX06", Driver: driverCypher, build: ix06, check: seekEqualsScan},
		{ID: "IX07", Driver: driverCypher, build: ix07, check: seekEqualsScan},
		{ID: "IX08", Driver: driverCypher, build: ix08, check: seekEqualsScan},
		{ID: "IX09", Driver: driverCypher, build: ix09, check: seekEqualsScan},
		{ID: "IX10", Driver: driverCypher, build: ix10, check: seekEqualsScan},
		{ID: "DD01", Driver: driverCypher, build: dd01Commit, check: seekEqualsScan},
		{ID: "DD01", Driver: driverCypher, build: dd01Rollback, check: seekEqualsScan},
		{ID: "DD02", Driver: driverCypher, build: dd02, check: seekEqualsScan},
		{ID: "DD03", Driver: driverCypher, build: dd03, check: seekEqualsScan},
		{ID: "DD04", Driver: driverCypher, build: dd04, check: seekEqualsScan},
		{ID: "DD05", Driver: driverCypher, build: dd05Commit, check: seekEqualsScan},
		{ID: "DD05", Driver: driverCypher, build: dd05Rollback, check: seekEqualsScan},
		{ID: "DD06", Driver: driverCypher, build: dd06},
		{ID: "DD06", Driver: driverCypher, build: dd06DDL},
		{ID: "DD07", Driver: driverCypher, build: dd07InWriteTx},
		{ID: "DD07", Driver: driverCypher, build: dd07InReadTx},
		{ID: "DD08", Driver: driverCypher, build: dd08},
		{ID: "DD09", Driver: driverCypher, build: dd09, check: seekEqualsScan},
		{ID: "AB01", Driver: driverCypher, build: ab01Commit},
		{ID: "AB01", Driver: driverCypher, build: ab01Rollback},
		{ID: "AB02", Driver: driverCypher, build: ab02, check: seekEqualsScan},
		{ID: "AB03", Driver: driverCypher, build: ab03Cypher},
		{ID: "AB03", Driver: driverLPG, build: ab03LPG},
		{ID: "AB04", Driver: driverCypher, build: ab04, check: acknowledgedEqualsFinal},
		{ID: "AB05", Driver: driverCypher, build: ab05, check: seekEqualsScan},
		{ID: "AB06", Driver: driverCypher, build: ab06Commit},
		{ID: "AB06", Driver: driverCypher, build: ab06Rollback},
		{ID: "AB06", Driver: driverLPG, build: ab06LPG},
		{ID: "HZ01", Driver: driverCypher, build: hz01Cypher, check: retainedThenReclaimed},
		{ID: "HZ01", Driver: driverLPG, build: hz01LPG, check: retainedThenReclaimed},
		{ID: "HZ02", Driver: driverCypher, build: hz02Cypher},
		{ID: "HZ02", Driver: driverLPG, build: hz02LPG},
		{ID: "HZ03", Driver: driverCypher, build: hz03Cypher},
		{ID: "HZ03", Driver: driverLPG, build: hz03LPG},
		{ID: "HZ04", Driver: driverCypher, build: hz04},
		{ID: "RO01", Driver: driverCypher, build: ro01Cypher, check: stableRead},
		{ID: "RO01", Driver: driverLPG, build: ro01LPG, check: stableRead},
		{ID: "RO02", Driver: driverCypher, build: ro02Cypher},
		{ID: "RO02", Driver: driverLPG, build: ro02LPG},
		{ID: "RO03", Driver: driverCypher, build: ro03, check: fastPathEqualsScan},
		{ID: "SE01", Driver: driverCypher, build: se01},
		{ID: "SE02", Driver: driverCypher, build: se02Cypher, check: parkedIn("s1wait", se02Held)},
		{ID: "SE02", Driver: driverLPG, build: se02LPG, check: parkedIn("s1wait", "s0w s0a s1w s1lag0 s1lag s1fr s1aw s1wait s0p s1rel s1seen")},
		{ID: "SE03", Driver: driverCypher, build: se03},
		{ID: "SE03", Driver: driverCypher, build: se03Fsync},
		{ID: "SE04", Driver: driverCypher, build: se04Cypher, check: parkedIn("s1wait", "s0w s0a s1w s1sl s1aw s1wait s0p s1rel")},
		{ID: "SE04", Driver: driverLPG, build: se04LPG, check: parkedIn("s1wait", "s0w s0a s1w s1c s1sl s1aw s1wait s0p s1rel")},
		{ID: "RI01", Driver: driverCypher, build: ri01Cypher, check: noDanglingEdge},
		{ID: "RI01", Driver: driverLPG, build: ri01LPG, check: noDanglingEdge},
		{ID: "RI02", Driver: driverCypher, build: ri02IntoCypher, check: noDanglingEdge},
		{ID: "RI02", Driver: driverLPG, build: ri02IntoLPG, check: noDanglingEdge},
		{ID: "RI02", Driver: driverCypher, build: ri02OutOfCypher, check: noDanglingEdge},
		{ID: "RI02", Driver: driverLPG, build: ri02OutOfLPG, check: noDanglingEdge},
		{ID: "RI03", Driver: driverCypher, build: ri03SameTarget, check: noDanglingEdge},
		{ID: "RI03", Driver: driverLPG, build: ri03SameTargetLPG, check: noDanglingEdge},
		{ID: "RI03", Driver: driverCypher, build: ri03SameSource, check: noDanglingEdge},
		{ID: "RI03", Driver: driverLPG, build: ri03SameSourceLPG, check: noDanglingEdge},
		{ID: "RI04", Driver: driverCypher, build: ri04Cypher, check: noDanglingEdge},
		{ID: "RI04", Driver: driverLPG, build: ri04LPG, check: noDanglingEdge},
		{ID: "RI05", Driver: driverCypher, build: ri05Cypher, check: noDanglingEdge},
		{ID: "RI05", Driver: driverLPG, build: ri05LPG, check: noDanglingEdge},
		{ID: "RI06", Driver: driverCypher, build: ri06Same, check: noDanglingEdge},
		{ID: "RI06", Driver: driverLPG, build: ri06SameLPG, check: noDanglingEdge},
		{ID: "RI06", Driver: driverCypher, build: ri06Parallel, check: noDanglingEdge},
		{ID: "RI06", Driver: driverLPG, build: ri06ParallelLPG, check: noDanglingEdge},
		{ID: "GG01", Driver: driverCypher, build: gg01, check: allOf(noDanglingEdge, sameRows([2]string{"s0r1", "s0r2"}))},
		{ID: "GG02", Driver: driverCypher, build: gg02Cypher, check: noDanglingEdge},
		{ID: "GG02", Driver: driverLPG, build: gg02LPG, check: noDanglingEdge},
		{ID: "GG03", Driver: driverCypher, build: gg03, check: allOf(noDanglingEdge, seekEqualsScan,
			sameRows([2]string{"s0d1", "s0d2"}, [2]string{"s0r1", "s0r2"}))},
		{ID: "GG04", Driver: driverCypher, build: gg04Cypher, check: allOf(noDanglingEdge, seekEqualsScan,
			sameRows([2]string{"s0c1", "s0c2"}))},
		{ID: "GG04", Driver: driverLPG, build: gg04LPG, check: allOf(noDanglingEdge, sameRows([2]string{"s0c1", "s0c2"}))},
		{ID: "GG05", Driver: driverCypher, build: gg05, check: noDanglingEdge},
		{ID: "GG06", Driver: driverCypher, build: gg06Disjoint, check: noDanglingEdge},
		{ID: "GG06", Driver: driverCypher, build: gg06Same, check: noDanglingEdge},
		{ID: "GG06", Driver: driverLPG, build: gg06DisjointLPG, check: noDanglingEdge},
		{ID: "GG06", Driver: driverLPG, build: gg06SameLPG, check: noDanglingEdge},
		{ID: "GG06", Driver: driverCypher, build: gg06SelfLoop, check: noDanglingEdge},
		{ID: "GG06", Driver: driverLPG, build: gg06SelfLoopLPG, check: noDanglingEdge},
		{ID: "FP01", Driver: driverCypher, build: fp01Cypher},
		{ID: "FP01", Driver: driverLPG, build: fp01LPG},
		{ID: "FP02", Driver: driverLPG, build: fp02, check: fp02Released},
		{ID: "FP03", Driver: driverCypher, build: fp03Cypher},
		{ID: "FP03", Driver: driverLPG, build: fp03LPG},
	}
}

// ---------------------------------------------------------------------------
// §1.1 Write-write conflicts.

func ww02Cypher(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ww02-stale-snapshot-write",
		Doc: "WW02. s2 takes its snapshot at BEGIN; s1 writes the same node and commits after it.\n" +
			"s2's write must be refused whenever s1's version is in flight or committed after\n" +
			"s2's snapshot (first-updater-wins / first-committer-wins, F2). PostgreSQL READ\n" +
			"COMMITTED would re-fetch and succeed; snapshot isolation cannot.",
		Setup: steps(q("mk", "CREATE (:Item {name:'n', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:Item {name:'n'}) SET n.v = 1 RETURN n.v AS v"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (n:Item {name:'n'}) SET n.v = 2 RETURN n.v AS v"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'n'}) RETURN n.v AS v")),
	}
}

func ww02LPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "ww02-stale-snapshot-write-lpg",
		Doc: "WW02 through the lpg API over the same graph: the same two transactions as the\n" +
			"Cypher arm, opened with Session.BeginVersionedTx and written with\n" +
			"ApplyInVersionedTx. The verdicts must match the Cypher arm.",
		Setup: steps(w.lpgFixture(lpgNode{key: "n", labels: []string{"Item"}, props: map[string]int64{"v": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(s1.set("s1w", "n", "v", 1), s1.commit("s1c"))},
			{Name: "s2", Setup: steps(s2.begin("s2b")), Steps: steps(s2.set("s2w", "n", "v", 2), s2.commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'n'}) RETURN n.v AS v")),
	}
}

func ww03Cypher(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ww03-blind-increment",
		Doc: "WW03. Two transactions each run the blind increment SET c.n = c.n + 1 with no\n" +
			"prior read. The second writer is refused (F2), so the final value is 1 after\n" +
			"one commit; only a retry reaches 2. InnoDB READ COMMITTED / REPEATABLE READ and\n" +
			"PostgreSQL READ COMMITTED give 2 without an error.",
		Setup: steps(q("mk", "CREATE (:Counter {name:'c', n:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1i", "MATCH (c:Counter {name:'c'}) SET c.n = c.n + 1 RETURN c.n AS n"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2i", "MATCH (c:Counter {name:'c'}) SET c.n = c.n + 1 RETURN c.n AS n"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (c:Counter {name:'c'}) RETURN c.n AS n")),
	}
}

func ww03LPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "ww03-blind-increment-lpg",
		Doc:  "WW03 through the lpg API: each increment reads through WriterViewOf(tx).",
		Setup: steps(w.lpgFixture(lpgNode{key: "c", labels: []string{"Counter"},
			props: map[string]int64{"n": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(s1.add("s1i", "c", "n", 1), s1.commit("s1c"))},
			{Name: "s2", Setup: steps(s2.begin("s2b")), Steps: steps(s2.add("s2i", "c", "n", 1), s2.commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (c:Counter {name:'c'}) RETURN c.n AS n")),
	}
}

const ww04Final = "MATCH (n:Item) RETURN n.name AS name, n.x AS x ORDER BY name"

func ww04Commit(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ww04-update-delete-commit",
		Doc: "WW04, commit arm. s1 updates node n, s2 DETACH DELETEs it; both commit. Whichever\n" +
			"writes second is refused, while the first is open and after it commits (the head\n" +
			"is not visible to the later snapshot). Both orders are enumerated.",
		Setup: steps(q("mk", "CREATE (:Item {name:'n', x:0}), (:Item {name:'m', x:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1u", "MATCH (n:Item {name:'n'}) SET n.x = 1 RETURN n.x AS x"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2d", "MATCH (n:Item {name:'n'}) DETACH DELETE n RETURN count(*) AS deleted"),
				commit("s2c"))},
		},
		Final: steps(q("final", ww04Final)),
	}
}

// rollbackArmDoc describes the autocommit arms of the write-after-rollback rows.
const rollbackArmDoc = "The second writer is an autocommit statement, so its snapshot is\n" +
	"taken when it runs. The -explicit arm runs the same row with an explicit\n" +
	"transaction whose snapshot predates the ROLLBACK."

// explicitArmDoc describes the explicit-transaction arms of the write-after-rollback
// rows, which rmp #2973 unblocked.
const explicitArmDoc = "The second writer is an EXPLICIT transaction that BEGINs before\n" +
	"either session's first step, so its snapshot predates the ROLLBACK. A ROLLBACK\n" +
	"aborts its transaction (rmp #2973): once it is drained, the older snapshot's write\n" +
	"must succeed, exactly as the autocommit arm's does. Before #2973 the ROLLBACK\n" +
	"published a commit record and this write was refused."

func ww04UpdateRollsBack(w *world) *isolationtest.Spec {
	return adjacentOnly(&isolationtest.Spec{
		Name: "ww04-update-rollback-then-delete",
		Doc: "WW04, rollback arm (update rolls back). s1 updates n and ROLLS BACK; the vacuum is\n" +
			"drained immediately after (H1); s2 deletes n. A delete issued while s1 is open is\n" +
			"refused; one issued after the rollback and drain succeeds; one committed before s1's\n" +
			"write makes s1's write refused. Only interleavings with the drain adjacent to the\n" +
			"rollback run.\n" + rollbackArmDoc,
		Setup: steps(q("mk", "CREATE (:Item {name:'n', x:0}), (:Item {name:'m', x:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1u", "MATCH (n:Item {name:'n'}) SET n.x = 1 RETURN n.x AS x"),
				rollback("s1rb"), w.drain("s1dr"))},
			{Name: "s2", Steps: steps(
				q("s2d", "MATCH (n:Item {name:'n'}) DETACH DELETE n RETURN count(*) AS deleted"))},
		},
		Final: steps(q("final", ww04Final)),
	}, [2]string{"s1rb", "s1dr"})
}

func ww04DeleteRollsBack(w *world) *isolationtest.Spec {
	return adjacentOnly(&isolationtest.Spec{
		Name: "ww04-delete-rollback-then-update",
		Doc: "WW04, rollback arm (delete rolls back). s2 deletes n and ROLLS BACK, the vacuum is\n" +
			"drained (H1); s1 updates n. An update issued after the rollback and drain succeeds\n" +
			"and n survives — a rolled-back delete must neither lose the node nor leave it\n" +
			"unwritable.\n" + rollbackArmDoc,
		Setup: steps(q("mk", "CREATE (:Item {name:'n', x:0}), (:Item {name:'m', x:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(
				q("s1u", "MATCH (n:Item {name:'n'}) SET n.x = 1 RETURN n.x AS x"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2d", "MATCH (n:Item {name:'n'}) DETACH DELETE n RETURN count(*) AS deleted"),
				rollback("s2rb"), w.drain("s2dr"))},
		},
		Final: steps(q("final", ww04Final)),
	}, [2]string{"s2rb", "s2dr"})
}

func ww04UpdateRollsBackExplicit(w *world) *isolationtest.Spec {
	return adjacentOnly(&isolationtest.Spec{
		Name: "ww04-update-rollback-then-delete-explicit",
		Doc: "WW04, rollback arm (update rolls back), explicit second writer. s1 updates n and\n" +
			"ROLLS BACK; the vacuum is drained immediately after (H1); s2, open since before\n" +
			"s1's write, deletes n and commits. Only interleavings with the drain adjacent to\n" +
			"the rollback run.\n" + explicitArmDoc,
		Setup: steps(q("mk", "CREATE (:Item {name:'n', x:0}), (:Item {name:'m', x:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1u", "MATCH (n:Item {name:'n'}) SET n.x = 1 RETURN n.x AS x"),
				rollback("s1rb"), w.drain("s1dr"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2d", "MATCH (n:Item {name:'n'}) DETACH DELETE n RETURN count(*) AS deleted"),
				commit("s2c"))},
		},
		Final: steps(q("final", ww04Final)),
	}, [2]string{"s1rb", "s1dr"})
}

func ww04DeleteRollsBackExplicit(w *world) *isolationtest.Spec {
	return adjacentOnly(&isolationtest.Spec{
		Name: "ww04-delete-rollback-then-update-explicit",
		Doc: "WW04, rollback arm (delete rolls back), explicit second writer. s2 deletes n and\n" +
			"ROLLS BACK, the vacuum is drained (H1); s1, open since before s2's delete, updates\n" +
			"n and commits. A rolled-back delete must neither lose the node nor leave it\n" +
			"unwritable to an older snapshot.\n" + explicitArmDoc,
		Setup: steps(q("mk", "CREATE (:Item {name:'n', x:0}), (:Item {name:'m', x:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1u", "MATCH (n:Item {name:'n'}) SET n.x = 1 RETURN n.x AS x"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2d", "MATCH (n:Item {name:'n'}) DETACH DELETE n RETURN count(*) AS deleted"),
				rollback("s2rb"), w.drain("s2dr"))},
		},
		Final: steps(q("final", ww04Final)),
	}, [2]string{"s2rb", "s2dr"})
}

func ww05Commit(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ww05-update-delete-chain-commit",
		Doc: "WW05, commit arm. s1 updates n then deletes it in ONE transaction and commits; s2,\n" +
			"whose snapshot predates s1's commit, writes n.y. s2 is refused whenever s1's chain\n" +
			"is in flight or committed after s2's snapshot.",
		Setup: steps(q("mk", "CREATE (:Item {name:'n', x:0, y:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1u", "MATCH (n:Item {name:'n'}) SET n.x = 1 RETURN n.x AS x"),
				q("s1d", "MATCH (n:Item {name:'n'}) DETACH DELETE n RETURN count(*) AS deleted"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (n:Item {name:'n'}) SET n.y = 1 RETURN n.y AS y"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item) RETURN n.name AS name, n.x AS x, n.y AS y ORDER BY name")),
	}
}

func ww05RollsBack(w *world) *isolationtest.Spec {
	return adjacentOnly(&isolationtest.Spec{
		Name: "ww05-update-delete-chain-rollback",
		Doc: "WW05, rollback arm. s1 updates n then deletes it in ONE transaction and ROLLS\n" +
			"BACK; the vacuum is drained immediately after (H1). s2, whose snapshot predates\n" +
			"s1's chain, writes n.y. s2 is refused while the chain is in flight and allowed\n" +
			"after the rollback and drain; n survives with x unchanged. PostgreSQL: the waiter\n" +
			"proceeds once the DELETE aborts — the same verdict, without the wait.\n" +
			explicitArmDoc,
		Setup: steps(q("mk", "CREATE (:Item {name:'n', x:0, y:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1u", "MATCH (n:Item {name:'n'}) SET n.x = 1 RETURN n.x AS x"),
				q("s1d", "MATCH (n:Item {name:'n'}) DETACH DELETE n RETURN count(*) AS deleted"),
				rollback("s1rb"), w.drain("s1dr"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (n:Item {name:'n'}) SET n.y = 1 RETURN n.y AS y"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item) RETURN n.name AS name, n.x AS x, n.y AS y ORDER BY name")),
	}, [2]string{"s1rb", "s1dr"})
}

func ww06(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ww06-predicate-moved-out",
		Doc: "WW06. s1 moves n out of the predicate (status 'a' -> 'b') and commits; s2, with an\n" +
			"older snapshot, runs MATCH (n {status:'a'}) SET n.v = 1. s2 still matches n at its\n" +
			"snapshot, so its write is REFUSED rather than silently skipped. PostgreSQL READ\n" +
			"COMMITTED re-evaluates the predicate and skips the row; InnoDB's semi-consistent\n" +
			"read skips it too.",
		Setup: steps(q("mk", "CREATE (:Item {name:'n', status:'a', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1m", "MATCH (n:Item {name:'n'}) SET n.status = 'b' RETURN n.status AS status"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (n:Item {status:'a'}) SET n.v = 1 RETURN n.name AS name, n.v AS v"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item) RETURN n.name AS name, n.status AS status, n.v AS v")),
	}
}

func ww07(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ww07-non-matching-not-blocked",
		Doc: "WW07. s1 holds an uncommitted write to node a; s2 writes only the nodes matching\n" +
			"{k:'b'}, which a is not. s2 must neither wait nor conflict in any interleaving (F7):\n" +
			"no step may report <waiting ...> and both transactions commit.",
		Setup: steps(q("mk", "CREATE (:Item {name:'a', k:'a', v:0}), (:Item {name:'b', k:'b', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:Item {name:'a'}) SET n.k = 1 RETURN n.k AS k"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (n:Item {k:'b'}) SET n.v = 1 RETURN n.name AS name"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item) RETURN n.name AS name, n.k AS k, n.v AS v ORDER BY name")),
	}
}

func ww08Cypher(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ww08-different-properties-one-node",
		Doc: "WW08. s1 writes n.a, s2 writes n.b — different properties of ONE node. The node is\n" +
			"the unit of conflict (F4), so the second writer is refused, the same verdict as\n" +
			"PostgreSQL's and InnoDB's row granularity. A narrowing of the conflict unit to\n" +
			"per-property would show here as a diff.",
		Setup: steps(q("mk", "CREATE (:Item {name:'n', a:0, b:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:Item {name:'n'}) SET n.a = 1 RETURN n.a AS a"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (n:Item {name:'n'}) SET n.b = 2 RETURN n.b AS b"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'n'}) RETURN n.a AS a, n.b AS b")),
	}
}

func ww08LPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "ww08-different-properties-one-node-lpg",
		Doc:  "WW08 through the lpg API: SetNodeProperty on two different keys of one node.",
		Setup: steps(w.lpgFixture(lpgNode{key: "n", labels: []string{"Item"},
			props: map[string]int64{"a": 0, "b": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(s1.set("s1w", "n", "a", 1), s1.commit("s1c"))},
			{Name: "s2", Setup: steps(s2.begin("s2b")), Steps: steps(s2.set("s2w", "n", "b", 2), s2.commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'n'}) RETURN n.a AS a, n.b AS b")),
	}
}

func ww09Cypher(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ww09-disjoint-nodes",
		Doc: "WW09. Each session updates only its own node, twice, then commits. Disjoint writes\n" +
			"never conflict: zero refusals in every interleaving (F2, F11).",
		Setup: steps(q("mk", "CREATE (:Item {name:'a', v:0}), (:Item {name:'b', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w1", "MATCH (n:Item {name:'a'}) SET n.v = n.v + 1 RETURN n.v AS v"),
				q("s1w2", "MATCH (n:Item {name:'a'}) SET n.v = n.v + 1 RETURN n.v AS v"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w1", "MATCH (n:Item {name:'b'}) SET n.v = n.v + 1 RETURN n.v AS v"),
				q("s2w2", "MATCH (n:Item {name:'b'}) SET n.v = n.v + 1 RETURN n.v AS v"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item) RETURN n.name AS name, n.v AS v ORDER BY name")),
	}
}

func ww09LPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "ww09-disjoint-nodes-lpg",
		Doc:  "WW09 through the lpg API: two sessions, each incrementing only its own node twice.",
		Setup: steps(w.lpgFixture(
			lpgNode{key: "a", labels: []string{"Item"}, props: map[string]int64{"v": 0}},
			lpgNode{key: "b", labels: []string{"Item"}, props: map[string]int64{"v": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(
				s1.add("s1w1", "a", "v", 1), s1.add("s1w2", "a", "v", 1), s1.commit("s1c"))},
			{Name: "s2", Setup: steps(s2.begin("s2b")), Steps: steps(
				s2.add("s2w1", "b", "v", 1), s2.add("s2w2", "b", "v", 1), s2.commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item) RETURN n.name AS name, n.v AS v ORDER BY name")),
	}
}

func ww10Cypher(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ww10-crossing-writes",
		Doc: "WW10. The deadlock shape: s1 writes A then B, s2 writes B then A. GoGraph takes no\n" +
			"DML locks (F7), so no step may ever report <waiting ...>; a write whose node holds\n" +
			"a peer's version is refused instead. BOTH transactions can be refused in one\n" +
			"interleaving, where PostgreSQL and InnoDB pick a single deadlock victim. A refused\n" +
			"statement poisons its transaction, so its COMMIT reports the poisoning (G10).",
		Setup: steps(q("mk", "CREATE (:Item {name:'A', v:0}), (:Item {name:'B', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1a", "MATCH (n:Item {name:'A'}) SET n.v = 1 RETURN n.v AS v"),
				q("s1b2", "MATCH (n:Item {name:'B'}) SET n.v = 1 RETURN n.v AS v"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2b2", "MATCH (n:Item {name:'B'}) SET n.v = 2 RETURN n.v AS v"),
				q("s2a", "MATCH (n:Item {name:'A'}) SET n.v = 2 RETURN n.v AS v"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item) RETURN n.name AS name, n.v AS v ORDER BY name")),
	}
}

func ww10LPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "ww10-crossing-writes-lpg",
		Doc: "WW10 through the lpg API. A refused write dooms the lpg transaction, so its commit\n" +
			"returns the conflict and publishes nothing — where the Cypher arm reports the\n" +
			"poisoned transaction instead. The verdict (nothing of a refused transaction\n" +
			"survives) is the same; the error each surface reports at COMMIT differs.",
		Setup: steps(w.lpgFixture(
			lpgNode{key: "A", labels: []string{"Item"}, props: map[string]int64{"v": 0}},
			lpgNode{key: "B", labels: []string{"Item"}, props: map[string]int64{"v": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(
				s1.set("s1a", "A", "v", 1), s1.set("s1b2", "B", "v", 1), s1.commit("s1c"))},
			{Name: "s2", Setup: steps(s2.begin("s2b")), Steps: steps(
				s2.set("s2b2", "B", "v", 2), s2.set("s2a", "A", "v", 2), s2.commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item) RETURN n.name AS name, n.v AS v ORDER BY name")),
	}
}

// ww11Ring is the N-way crossing cycle: session i writes its own ring node and
// then its successor's, rolls back, and drains the vacuum; the last session
// finally rewrites the whole ring in one autocommit statement. Explicit
// interleavings, as the catalogue requires: the full enumeration of eight
// sessions is astronomically large.
func ww11Ring(w *world, n int) *isolationtest.Spec {
	sessions := make([]*isolationtest.Session, n)
	var mk strings.Builder
	mk.WriteString("CREATE ")
	for i := 0; i < n; i++ {
		if i > 0 {
			mk.WriteString(", ")
		}
		fmt.Fprintf(&mk, "(:Ring {name:'r%d', v:'-'})", i)
	}
	name := func(i int, step string) string { return fmt.Sprintf("s%d%s", i, step) }
	for i := 0; i < n; i++ {
		next := (i + 1) % n
		st := steps(
			begin(name(i, "b")),
			q(name(i, "own"), fmt.Sprintf("MATCH (n:Ring {name:'r%d'}) SET n.v = 's%d' RETURN n.v AS v", i, i)),
			q(name(i, "next"), fmt.Sprintf("MATCH (n:Ring {name:'r%d'}) SET n.v = 's%d' RETURN n.v AS v", next, i)),
			rollback(name(i, "rb")),
			w.drain(name(i, "dr")))
		if i == n-1 {
			st = append(st, q("retry", "MATCH (n:Ring) SET n.v = 'retry' RETURN count(n) AS written"))
		}
		sessions[i] = &isolationtest.Session{Name: fmt.Sprintf("s%d", i), Steps: st}
	}
	// Every transaction BEGINs as a step, placed so that no transaction writes a
	// node after a peer that touched it rolled back while it was open: such a
	// write is refused by the rollback defect README.md records, which this row
	// does not pin.
	var roundRobin, sequential, staggered []string
	for _, part := range []string{"b", "own", "next"} {
		for i := 0; i < n; i++ {
			roundRobin = append(roundRobin, name(i, part))
		}
	}
	for i := 0; i < n; i++ {
		roundRobin = append(roundRobin, name(i, "rb"), name(i, "dr"))
		sequential = append(sequential, name(i, "b"), name(i, "own"), name(i, "next"), name(i, "rb"), name(i, "dr"))
	}
	roundRobin = append(roundRobin, "retry")
	sequential = append(sequential, "retry")
	// Staggered: each session writes its own node, then the even-numbered ones
	// reach for their successor before the odd-numbered ones do.
	for i := 0; i < n; i++ {
		staggered = append(staggered, name(i, "b"), name(i, "own"))
	}
	for _, parity := range []int{0, 1} {
		for i := parity; i < n; i += 2 {
			staggered = append(staggered, name(i, "next"))
		}
	}
	for i := n - 1; i >= 0; i-- {
		staggered = append(staggered, name(i, "rb"), name(i, "dr"))
	}
	staggered = append(staggered, "retry")
	p := [][]string{roundRobin, sequential}
	named := "round-robin, sequential."
	if n <= 4 {
		p = append(p, staggered)
		named = "round-robin, sequential, staggered."
	}
	return &isolationtest.Spec{
		Name: fmt.Sprintf("ww11-ring-%d", n),
		Doc: fmt.Sprintf("WW11. A ring of %d transactions, each writing its own node and then its\n", n) +
			"successor's — the N-way deadlock cycle. No step may ever wait (F7); each write\n" +
			"succeeds or is refused. Every transaction then rolls back and the vacuum is drained\n" +
			"(H1), after which one autocommit statement rewrites the whole ring and must\n" +
			"succeed: the cycle leaves nothing behind that blocks progress. Named interleavings:\n" +
			named,
		Setup:        steps(q("mk", mk.String())),
		Sessions:     sessions,
		Permutations: p,
		Final:        steps(q("final", "MATCH (n:Ring) RETURN n.name AS name, n.v AS v ORDER BY name")),
	}
}

func ww12(w *world) *isolationtest.Spec {
	return adjacentOnly(&isolationtest.Spec{
		Name: "ww12-write-after-peer-rollback",
		Doc: "WW12. s1 writes n and ROLLS BACK; the vacuum is drained right after (H1); s2 writes\n" +
			"n. A write after the rollback and drain succeeds in every interleaving — an aborted\n" +
			"write must not leave the node permanently unwritable (#2318 class). A write while\n" +
			"s1 is open is refused; one committed before s1's write makes s1's refused.\n" +
			"The no-drain arm is NOT pinned: its outcome would depend on the vacuum's timing\n" +
			"(F5), so a golden would record a race.\n" + rollbackArmDoc,
		Setup: steps(q("mk", "CREATE (:Item {name:'n', x:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:Item {name:'n'}) SET n.x = 1 RETURN n.x AS x"),
				rollback("s1rb"), w.drain("s1dr"))},
			{Name: "s2", Steps: steps(
				q("s2w", "MATCH (n:Item {name:'n'}) SET n.x = 2 RETURN n.x AS x"))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'n'}) RETURN n.x AS x")),
	}, [2]string{"s1rb", "s1dr"})
}

func ww12Explicit(w *world) *isolationtest.Spec {
	return adjacentOnly(&isolationtest.Spec{
		Name: "ww12-write-after-peer-rollback-explicit",
		Doc: "WW12, explicit second writer. s1 writes n and ROLLS BACK; the vacuum is drained\n" +
			"right after (H1); s2, open since before s1's write, writes n and commits. A write\n" +
			"after the rollback and drain succeeds in every interleaving; one while s1 is open\n" +
			"is refused.\n" + explicitArmDoc,
		Setup: steps(q("mk", "CREATE (:Item {name:'n', x:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:Item {name:'n'}) SET n.x = 1 RETURN n.x AS x"),
				rollback("s1rb"), w.drain("s1dr"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (n:Item {name:'n'}) SET n.x = 2 RETURN n.x AS x"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'n'}) RETURN n.x AS x")),
	}, [2]string{"s1rb", "s1dr"})
}

const ww13Doc = "is a void primitive: a conflict it hits cannot be returned by the\n" +
	"statement, so it is RECORDED on the transaction and must surface at COMMIT as a\n" +
	"serialization conflict, applying nothing (F3, rmp #2354). A COMMIT that succeeds\n" +
	"after a recorded conflict is a silently dropped write."

func ww13Label(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name:  "ww13-void-label-removal",
		Doc:   "WW13, label arm. Both sessions REMOVE n:Flag. Label removal " + ww13Doc,
		Setup: steps(q("mk", "CREATE (:Item:Flag {name:'n'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1x", "MATCH (n:Item {name:'n'}) REMOVE n:Flag RETURN n.name AS name"), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2x", "MATCH (n:Item {name:'n'}) REMOVE n:Flag RETURN n.name AS name"), commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'n'}) RETURN n:Flag AS flagged")),
	}
}

func ww13LabelLPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name:  "ww13-void-label-removal-lpg",
		Doc:   "WW13, label arm, through the lpg API: RemoveNodeLabel " + ww13Doc,
		Setup: steps(w.lpgFixture(lpgNode{key: "n", labels: []string{"Item", "Flag"}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(s1.removeLabel("s1x", "n", "Flag"), s1.commit("s1c"))},
			{Name: "s2", Setup: steps(s2.begin("s2b")), Steps: steps(s2.removeLabel("s2x", "n", "Flag"), s2.commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'n'}) RETURN n:Flag AS flagged")),
	}
}

func ww13PropertyDelete(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name:  "ww13-void-property-delete",
		Doc:   "WW13, property arm. Both sessions REMOVE n.p. Property delete " + ww13Doc,
		Setup: steps(q("mk", "CREATE (:Item {name:'n', p:1})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1x", "MATCH (n:Item {name:'n'}) REMOVE n.p RETURN n.name AS name"), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2x", "MATCH (n:Item {name:'n'}) REMOVE n.p RETURN n.name AS name"), commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'n'}) RETURN n.p AS p")),
	}
}

func ww13PropertyDeleteLPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "ww13-void-property-delete-lpg",
		Doc:  "WW13, property arm, through the lpg API: DelNodeProperty " + ww13Doc,
		Setup: steps(w.lpgFixture(lpgNode{key: "n", labels: []string{"Item"},
			props: map[string]int64{"p": 1}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(s1.delProp("s1x", "n", "p"), s1.commit("s1c"))},
			{Name: "s2", Setup: steps(s2.begin("s2b")), Steps: steps(s2.delProp("s2x", "n", "p"), s2.commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'n'}) RETURN n.p AS p")),
	}
}

func ww13EdgeProperty(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name:  "ww13-void-edge-property",
		Doc:   "WW13, edge arm. Both sessions SET r.w on one edge. An edge side-store write " + ww13Doc,
		Setup: steps(q("mk", "CREATE (:Item {name:'a'})-[:LINK {w:0}]->(:Item {name:'b'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1x", "MATCH (:Item {name:'a'})-[r:LINK]->(:Item {name:'b'}) SET r.w = 1 RETURN r.w AS w"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2x", "MATCH (:Item {name:'a'})-[r:LINK]->(:Item {name:'b'}) SET r.w = 2 RETURN r.w AS w"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (:Item {name:'a'})-[r:LINK]->(:Item {name:'b'}) RETURN r.w AS w")),
	}
}

func ww14(*world) *isolationtest.Spec {
	const read = "MATCH (n:Item {name:'n'}) RETURN n.v AS v"
	return &isolationtest.Spec{
		Name: "ww14-chain-walk-in-flight-head",
		Doc: "WW14. s0 pins a read snapshot; s1 writes n and commits; s2 then writes n and keeps\n" +
			"its transaction open; s0 reads; s3 reads in a new autocommit statement. s0 sees\n" +
			"the ORIGINAL value through a chain whose head is in flight and whose middle\n" +
			"committed after its snapshot; s3 sees s1's committed value and never s2's (F1).\n" +
			"Named interleavings: s2 must BEGIN after s1 commits, or its write is refused.",
		Setup: steps(q("mk", "CREATE (:Item {name:'n', v:'original'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s0", Setup: steps(beginRead("s0b")), Steps: steps(
				q("s0r1", read), q("s0r2", read), commit("s0c"))},
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:Item {name:'n'}) SET n.v = 'committed' RETURN n.v AS v"), commit("s1c"))},
			{Name: "s2", Steps: steps(
				begin("s2b"),
				q("s2w", "MATCH (n:Item {name:'n'}) SET n.v = 'in-flight' RETURN n.v AS v"),
				rollback("s2rb"))},
			{Name: "s3", Steps: steps(q("s3r", read))},
		},
		Permutations: perms(
			"s0r1 s1w s1c s2b s2w s0r2 s3r s2rb s0c",
			"s1w s0r1 s1c s2b s2w s3r s0r2 s0c s2rb",
		),
		Final: steps(q("final", read)),
	}
}

// ww15Blob builds a 64 KiB string of one repeated character in Cypher: sixteen
// doublings of a one-character seed.
func ww15Blob(c string) string { return fmt.Sprintf("reduce(s = '%s', i IN range(1, 16) | s + s)", c) }

func ww15(*world) *isolationtest.Spec {
	read := "MATCH (n:Doc {name:'d'}) RETURN size(n.blob) AS bytes, substring(n.blob, 0, 3) AS head, " +
		"n.blob = " + ww15Blob("z") + " AS original"
	write := func(name, c string) isolationtest.Step {
		return q(name, "MATCH (n:Doc {name:'d'}) SET n.blob = "+ww15Blob(c)+" RETURN size(n.blob) AS bytes")
	}
	return &isolationtest.Spec{
		Name: "ww15-long-chain-large-values",
		Doc: "WW15. s0 pins a read snapshot over a 64 KiB string; s1 commits four successive\n" +
			"64 KiB rewrites of it, building a chain of five versions. s0 must read the value\n" +
			"current at its BEGIN, byte-identical (original=true), in every interleaving (F1).",
		// The blob is written inline in the CREATE map; a map value carrying its
		// own commas (the reduce) parses since rmp #2975.
		Setup: steps(q("mk", "CREATE (:Doc {name:'d', blob: "+ww15Blob("z")+"})")),
		Sessions: []*isolationtest.Session{
			{Name: "s0", Setup: steps(beginRead("s0b")), Steps: steps(q("s0r1", read), q("s0r2", read), commit("s0c"))},
			{Name: "s1", Steps: steps(write("s1w1", "a"), write("s1w2", "b"), write("s1w3", "c"), write("s1w4", "d"))},
		},
		Final: steps(q("final", "MATCH (n:Doc {name:'d'}) RETURN size(n.blob) AS bytes, substring(n.blob, 0, 3) AS head")),
	}
}

// ww15Check asserts that every pinned read returned the original value in full.
func ww15Check() isolationtest.Observer {
	return func(o isolationtest.Observation) error {
		if !strings.HasPrefix(o.Step, "s0r") {
			return nil
		}
		if o.Err != nil {
			return fmt.Errorf("pinned read failed: %w", o.Err)
		}
		if len(o.Rows) != 1 || o.Rows[0][0] != "65536" || o.Rows[0][2] != "true" {
			return fmt.Errorf("pinned reader saw %v, want the original 65536-byte value", o.Rows)
		}
		return nil
	}
}

// ---------------------------------------------------------------------------
// §1.2 Snapshot-isolation anomalies and snapshot reads.

func sk02(*world) *isolationtest.Spec {
	const final = "MATCH (f:Fruit) RETURN f.name AS name, f.kind AS kind ORDER BY name"
	return &isolationtest.Spec{
		Name: "sk02-swap-write-skew",
		Doc: "SK02. s1 turns every apple into a pear; s2 turns every pear into an apple. Each\n" +
			"reads the set the other writes and they write disjoint nodes, so snapshot\n" +
			"isolation PERMITS both to commit and the sets end up swapped — a result no serial\n" +
			"order produces. PostgreSQL SERIALIZABLE aborts one; GoGraph is SI (F1).",
		Setup: steps(q("mk", "CREATE (:Fruit {name:'f1', kind:'apple'}), (:Fruit {name:'f2', kind:'apple'}), "+
			"(:Fruit {name:'f3', kind:'pear'}), (:Fruit {name:'f4', kind:'pear'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (f:Fruit {kind:'apple'}) SET f.kind = 'pear' RETURN count(f) AS changed"), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (f:Fruit {kind:'pear'}) SET f.kind = 'apple' RETURN count(f) AS changed"), commit("s2c"))},
		},
		Final: steps(q("final", final)),
	}
}

func sk03(*world) *isolationtest.Spec {
	const check = "MATCH (b:Booking {room:'r1'}) WHERE b.start < 14 AND b.end > 13 RETURN count(b) AS overlapping"
	return &isolationtest.Spec{
		Name: "sk03-booking-overlap",
		Doc: "SK03. Predicate-insert write skew. Each session checks that no booking of room r1\n" +
			"overlaps 13-14, finds none, and creates one. Without predicate locks (F7) both\n" +
			"commit and two overlapping bookings exist: snapshot isolation permits it.",
		Setup: steps(q("mk", "CREATE (:Booking {room:'r1', start:9, end:10, owner:'setup'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1r", check),
				q("s1w", "CREATE (b:Booking {room:'r1', start:13, end:14, owner:'s1'}) RETURN b.owner AS owner"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2r", check),
				q("s2w", "CREATE (b:Booking {room:'r1', start:13, end:14, owner:'s2'}) RETURN b.owner AS owner"),
				commit("s2c"))},
		},
		Final: steps(q("final", check)),
	}
}

func sk04(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "sk04-cross-label-write-skew",
		Doc: "SK04. Rule: a project's manager must be a manager. s1 checks that person p is a\n" +
			"manager and creates a project managed by p; s2 checks that no project names p and\n" +
			"demotes p. They read and write different labels, so both commit where their reads\n" +
			"precede each other's commits: SI permits the broken rule.",
		Setup: steps(q("mk", "CREATE (:Person {name:'p', manager:true})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1r", "MATCH (p:Person {name:'p'}) RETURN p.manager AS manager"),
				q("s1w", "CREATE (j:Project {name:'proj', manager:'p'}) RETURN j.name AS project"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2r", "MATCH (j:Project {manager:'p'}) RETURN count(j) AS projects"),
				q("s2w", "MATCH (p:Person {name:'p'}) SET p.manager = false RETURN p.manager AS manager"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (p:Person {name:'p'}) OPTIONAL MATCH (j:Project {manager:'p'}) "+
			"RETURN p.manager AS manager, count(j) AS projects")),
	}
}

func sk05Cypher(*world) *isolationtest.Spec {
	const total = "MATCH (a:Account) RETURN sum(a.bal) AS total"
	return &isolationtest.Spec{
		Name: "sk05-total-cash",
		Doc: "SK05. Two accounts of 300. Each session reads the total (600), decides a withdrawal\n" +
			"of 400 is covered, and debits ITS OWN account. Disjoint writes, so both commit and\n" +
			"the total goes negative (-200): snapshot isolation permits it.",
		Setup: steps(q("mk", "CREATE (:Account {name:'x', bal:300}), (:Account {name:'y', bal:300})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1r", total),
				q("s1w", "MATCH (a:Account {name:'x'}) SET a.bal = a.bal - 400 RETURN a.bal AS bal"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2r", total),
				q("s2w", "MATCH (a:Account {name:'y'}) SET a.bal = a.bal - 400 RETURN a.bal AS bal"),
				commit("s2c"))},
		},
		Final: steps(q("final", total)),
	}
}

func sk05LPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "sk05-total-cash-lpg",
		Doc:  "SK05 through the lpg API: both balances read at the transaction's snapshot, own account debited.",
		Setup: steps(w.lpgFixture(
			lpgNode{key: "x", labels: []string{"Account"}, props: map[string]int64{"bal": 300}},
			lpgNode{key: "y", labels: []string{"Account"}, props: map[string]int64{"bal": 300}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(
				s1.read("s1r", "x.bal", "y.bal"), s1.add("s1w", "x", "bal", -400), s1.commit("s1c"))},
			{Name: "s2", Setup: steps(s2.begin("s2b")), Steps: steps(
				s2.read("s2r", "x.bal", "y.bal"), s2.add("s2w", "y", "bal", -400), s2.commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (a:Account) RETURN sum(a.bal) AS total")),
	}
}

func sk07(*world) *isolationtest.Spec {
	const report = "MATCH (c:Control) OPTIONAL MATCH (r:Receipt) WHERE r.date = c.date - 1 " +
		"RETURN c.date AS date, count(r) AS closed_receipts"
	return &isolationtest.Spec{
		Name: "sk07-receipt-report",
		Doc: "SK07. Three sessions over a batch date. s1 reads the current date and records a\n" +
			"receipt for it; s2 closes the batch (date + 1); s3, read-only, reports the receipts\n" +
			"of the closed date. In the anomalous interleaving s3 reports the closed batch\n" +
			"WITHOUT s1's receipt, which then commits into it: a state no serial order shows.\n" +
			"Snapshot isolation permits it (F1). Named interleavings: the full enumeration is\n" +
			"210.",
		Setup: steps(q("mk", "CREATE (:Control {name:'ctl', date:1})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1r", "MATCH (c:Control) RETURN c.date AS date"),
				// The date is read inline in the CREATE map, as the source spec does.
				// That read must come from s1's snapshot (rmp #2974, fixed); an
				// interleaving in which s2 commits before s1w would otherwise record
				// the receipt under the NEW date.
				q("s1w", "MATCH (c:Control) CREATE (r:Receipt {date: c.date, amount: 4}) RETURN r.date AS date"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (c:Control) SET c.date = c.date + 1 RETURN c.date AS date"),
				commit("s2c"))},
			{Name: "s3", Steps: steps(beginRead("s3b"), q("s3r", report), commit("s3c"))},
		},
		Permutations: perms(
			"s1r s1w s1c s2w s2c s3b s3r s3c",
			"s1r s2w s2c s3b s3r s3c s1w s1c",
			"s1r s2w s2c s1w s3b s3r s1c s3c",
			"s2w s2c s1r s1w s1c s3b s3r s3c",
		),
		Final: steps(q("final", "MATCH (r:Receipt) RETURN r.date AS date, count(r) AS receipts ORDER BY date")),
	}
}

func sk08(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "sk08-two-ids",
		Doc: "SK08. Three sessions over two nodes x and y: s1 writes x; s2 reads x and writes y\n" +
			"from it; s3 reads y. Every write lands on a node no other session writes, so all\n" +
			"three commit in every interleaving — including those a serializable engine would\n" +
			"refuse because the dependencies form a cycle. Snapshot isolation permits it (F1).",
		Setup: steps(q("mk", "CREATE (:Id {name:'x', v:1}), (:Id {name:'y', v:1})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (x:Id {name:'x'}) SET x.v = 10 RETURN x.v AS x"), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (x:Id {name:'x'}), (y:Id {name:'y'}) SET y.v = x.v + 1 RETURN x.v AS x, y.v AS y"),
				commit("s2c"))},
			{Name: "s3", Setup: steps(begin("s3b")), Steps: steps(
				q("s3r", "MATCH (y:Id {name:'y'}) RETURN y.v AS y"), commit("s3c"))},
		},
		Final: steps(q("final", "MATCH (n:Id) RETURN n.name AS name, n.v AS v ORDER BY name")),
	}
}

// sk10Spec is SK01 with the on-call count read twice: once through the indexed
// predicate (a seek) and once through a non-sargable spelling of it (a scan).
//
// The scan arm is an OPTIONAL MATCH, as in the index rows ([seekScan]): with a
// plain MATCH, a scan that finds no doctor leaves no row to group, the statement
// returns no row, and a seek that counted a doctor the scan did not would vanish
// from the transcript instead of disagreeing in it.
func sk10Spec(name, kind, index, seek, scan string) *isolationtest.Spec {
	read := "MATCH (d:Doctor) WHERE " + seek + " WITH count(d) AS seek " +
		"OPTIONAL MATCH (e:Doctor) WHERE " + strings.ReplaceAll(scan, "d.", "e.") + " RETURN seek, count(e) AS scan"
	return &isolationtest.Spec{
		Name: name,
		Doc: "SK10, " + kind + ". SK01 write skew with the on-call count read through an index\n" +
			"seek and, in the same statement, through a scan of the same predicate. The skew\n" +
			"is permitted (both commit), and the seek must equal the scan in every step.",
		// 1 100 off-call doctors beside the two on call: the planner seeks only
		// above a label population and below a selectivity threshold, and with a
		// handful of nodes it scans both arms, which would make seek = scan compare
		// a plan with itself. TestSK10AccessPaths proves the arms differ.
		Setup: steps(
			q("ix", index),
			q("mk", "CREATE (:Doctor {name:'alice', oncall:1}), (:Doctor {name:'bob', oncall:1})"),
			q("pad", "UNWIND range(1, 1100) AS i CREATE (:Doctor {name:'d' + toString(i), oncall:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1r", read),
				q("s1w", "MATCH (d:Doctor {name:'alice'}) SET d.oncall = 0 RETURN d.name AS who"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2r", read),
				q("s2w", "MATCH (d:Doctor {name:'bob'}) SET d.oncall = 0 RETURN d.name AS who"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (d:Doctor) WHERE d.oncall = 1 RETURN count(d) AS oncall")),
	}
}

func sk10Hash(*world) *isolationtest.Spec {
	return sk10Spec("sk10-write-skew-hash-seek", "hash equality",
		"CREATE INDEX doctor_oncall FOR (d:Doctor) ON (d.oncall)", "d.oncall = 1", "d.oncall + 0 = 1")
}

func sk10Btree(*world) *isolationtest.Spec {
	return sk10Spec("sk10-write-skew-btree-range", "btree range",
		"CREATE INDEX doctor_oncall FOR (d:Doctor) ON (d.oncall) OPTIONS {indexType: 'btree'}",
		"d.oncall >= 1", "d.oncall + 0 >= 1")
}

// seekEqualsScan asserts the two columns of an SK10 read agree.
func seekEqualsScan() isolationtest.Observer {
	return func(o isolationtest.Observation) error {
		// A failed step carries no columns, so it is skipped here; the golden
		// records the failure.
		if len(o.Cols) != 2 || o.Cols[0] != "seek" {
			return nil
		}
		for _, row := range o.Rows {
			if row[0] != row[1] {
				return fmt.Errorf("index seek counted %s, scan counted %s", row[0], row[1])
			}
		}
		return nil
	}
}

func sk11Spec(name, arm, guard string) *isolationtest.Spec {
	const count = "MATCH (d:Doctor) WHERE d.oncall = true RETURN count(d) AS oncall"
	off := func(who string) string {
		return "MATCH (d:Doctor {name:'" + who + "'}) SET d.oncall = false WITH d " +
			"MATCH (g:Guard {name:'g'}) SET " + guard + " RETURN d.name AS who, g.v AS guard"
	}
	return &isolationtest.Spec{
		Name: name,
		Doc: "SK11, " + arm + ". SK01 write skew, materialised: each transaction also writes the\n" +
			"shared guard node g in the statement that takes its doctor off call. This is the\n" +
			"documented snapshot-isolation remedy (Fekete et al., TODS 2005) for the absent\n" +
			"locking read (G4).",
		Setup: steps(q("mk", "CREATE (:Doctor {name:'alice', oncall:true}), (:Doctor {name:'bob', oncall:true}), "+
			"(:Guard {name:'g', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(q("s1r", count), q("s1w", off("alice")), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(q("s2r", count), q("s2w", off("bob")), commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (d:Doctor) WHERE d.oncall = true WITH count(d) AS oncall "+
			"MATCH (g:Guard {name:'g'}) RETURN oncall, g.v AS guard")),
	}
}

func sk11Increment(*world) *isolationtest.Spec {
	s := sk11Spec("sk11-materialised-conflict-increment", "increment arm (SET g.v = g.v + 1)", "g.v = g.v + 1")
	s.Doc += "\nThe two increments collide on g, so the second writer is refused and the rule\n" +
		"(at least one doctor on call) holds in every interleaving where both would commit."
	return s
}

func sk11ValuePreserving(*world) *isolationtest.Spec {
	s := sk11Spec("sk11-materialised-conflict-value-preserving", "value-preserving arm (SET g.v = g.v)", "g.v = g.v")
	s.Doc += "\nThis arm writes g's CURRENT value back. The catalogue left its verdict unverified\n" +
		"(G4); this transcript pins it."
	return s
}

func sk12(*world) *isolationtest.Spec {
	const count = "MATCH (n:L) RETURN count(n) AS c"
	return &isolationtest.Spec{
		Name: "sk12-no-phantom-no-non-repeatable",
		Doc: "SK12. s1 counts :L twice in one transaction; s2 inserts one :L node, deletes\n" +
			"another, and commits. Both of s1's counts must be equal in every interleaving:\n" +
			"no phantom and no non-repeatable read under snapshot isolation (F1).",
		Setup: steps(q("mk", "CREATE (:L {name:'n1'}), (:L {name:'n2'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(q("s1c1", count), q("s1c2", count), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2i", "CREATE (n:L {name:'n3'}) RETURN n.name AS created"),
				q("s2d", "MATCH (n:L {name:'n1'}) DETACH DELETE n RETURN count(*) AS deleted"),
				commit("s2c"))},
		},
		Final: steps(q("final", count)),
	}
}

// repeatableCount asserts that within one permutation s1's two counts agree.
func repeatableCount() isolationtest.Observer {
	first := map[string]string{}
	return func(o isolationtest.Observation) error {
		// A failed step carries no rows, so it is skipped here; the golden
		// records the failure.
		if len(o.Rows) != 1 {
			return nil
		}
		switch o.Step {
		case "s1c1":
			first[o.Permutation] = o.Rows[0][0]
		case "s1c2":
			if want := first[o.Permutation]; o.Rows[0][0] != want {
				return fmt.Errorf("second count %s differs from first %s", o.Rows[0][0], want)
			}
		}
		return nil
	}
}

func sk12ReadSkewLPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "sk12-read-skew-lpg",
		Doc: "SK12 through the lpg API, extended to two keys (read skew, A5A). s1 reads x, then\n" +
			"y, then x again inside one transaction; s2 moves 10 from x to y and commits. s1's\n" +
			"two reads of x must agree and x + y must be 100 in every interleaving (F1). This\n" +
			"spec is the negative control's target: a per-key snapshot breaks both.",
		Setup: steps(w.lpgFixture(
			lpgNode{key: "x", labels: []string{"Account"}, props: map[string]int64{"bal": 50}},
			lpgNode{key: "y", labels: []string{"Account"}, props: map[string]int64{"bal": 50}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(
				s1.read("s1x", "x.bal"), s1.read("s1y", "y.bal"), s1.read("s1x2", "x.bal"), s1.commit("s1c"))},
			{Name: "s2", Setup: steps(s2.begin("s2b")), Steps: steps(
				s2.add("s2dx", "x", "bal", -10), s2.add("s2cy", "y", "bal", 10), s2.commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (a:Account) RETURN sum(a.bal) AS total")),
	}
}

// readSkewFree asserts, per permutation, that s1's reads form one consistent
// snapshot: x read twice the same, and x + y equal to the conserved total.
func readSkewFree() isolationtest.Observer {
	type reads struct{ x, y string }
	seen := map[string]*reads{}
	return func(o isolationtest.Observation) error {
		// A failed step carries no rows, so it is skipped here; the golden
		// records the failure.
		if len(o.Rows) != 1 {
			return nil
		}
		r := seen[o.Permutation]
		if r == nil {
			r = &reads{}
			seen[o.Permutation] = r
		}
		v := o.Rows[0][0]
		switch o.Step {
		case "s1x":
			r.x = v
		case "s1y":
			r.y = v
			x, _ := strconv.Atoi(r.x)
			y, _ := strconv.Atoi(v)
			if x+y != 100 {
				return fmt.Errorf("read skew: x=%s y=%s sum to %d, want 100", r.x, v, x+y)
			}
		case "s1x2":
			if v != r.x {
				return fmt.Errorf("non-repeatable read: x read %s then %s", r.x, v)
			}
		}
		return nil
	}
}

func sk13Cypher(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "sk13-snapshot-at-begin",
		Doc: "SK13. s1 BEGINs; s2 creates an :L node in autocommit; s1 then reads for the first\n" +
			"time. s1 sees the node only if it was committed BEFORE s1's BEGIN: GoGraph takes\n" +
			"the snapshot at BEGIN (F6). InnoDB's plain START TRANSACTION takes it at the first\n" +
			"read instead, and would see it; WITH CONSISTENT SNAPSHOT matches GoGraph.",
		Setup: steps(q("mk", "CREATE (:L {name:'early'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(begin("s1b"), q("s1r", "MATCH (n:L) RETURN count(n) AS c"), commit("s1c"))},
			{Name: "s2", Steps: steps(q("s2w", "CREATE (n:L {name:'late'}) RETURN n.name AS created"))},
		},
		Final: steps(q("final", "MATCH (n:L) RETURN count(n) AS c")),
	}
}

func sk13LPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "sk13-snapshot-at-begin-lpg",
		Doc: "SK13 through the lpg API: s1 opens Session.BeginVersionedTx, s2 commits x.v = 2 in\n" +
			"its own transaction, s1 then reads x.v. s1 sees 2 only if s2 committed before s1\n" +
			"began.",
		Setup: steps(w.lpgFixture(lpgNode{key: "x", labels: []string{"Item"}, props: map[string]int64{"v": 1}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(s1.begin("s1b"), s1.read("s1r", "x.v"), s1.commit("s1c"))},
			{Name: "s2", Steps: steps(s2.set("s2w", "x", "v", 2))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'x'}) RETURN n.v AS v")),
	}
}

func sk14(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "sk14-fast-path-aggregates",
		Doc: "SK14. s1 answers count(n) and count(n:L) through the count fast path and through a\n" +
			"filtered scan, inside one transaction; s2 creates one :L node and deletes another,\n" +
			"open and then committed. The fast path must equal the scan, at s1's snapshot, in\n" +
			"every interleaving.",
		Setup: steps(q("mk", "CREATE (:L {name:'l1'}), (:L {name:'l2'}), (:L {name:'l3'}), (:M {name:'m1'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1fa", "MATCH (n) RETURN count(n) AS c"),
				q("s1sa", "MATCH (n) WHERE n.name IS NOT NULL RETURN count(n) AS c"),
				q("s1fl", "MATCH (n:L) RETURN count(n) AS c"),
				q("s1sl", "MATCH (n:L) WHERE n.name IS NOT NULL RETURN count(n) AS c"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "CREATE (:L {name:'l9'}) WITH 1 AS one MATCH (d:L {name:'l1'}) DETACH DELETE d "+
					"RETURN count(*) AS deleted"),
				commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (n:L) RETURN count(n) AS c")),
	}
}

// fastPathEqualsScan asserts, per permutation, that each fast-path count equals
// the scan that follows it.
func fastPathEqualsScan() isolationtest.Observer {
	fast := map[string]string{}
	return func(o isolationtest.Observation) error {
		// A failed step carries no rows, so it is skipped here; the golden
		// records the failure.
		if len(o.Rows) != 1 {
			return nil
		}
		switch o.Step {
		case "s1fa", "s1fl":
			fast[o.Permutation+o.Step[2:]] = o.Rows[0][0]
		case "s1sa", "s1sl":
			key := o.Permutation + "f" + o.Step[3:]
			if want := fast[key]; o.Rows[0][0] != want {
				return fmt.Errorf("scan counted %s, fast path counted %s", o.Rows[0][0], want)
			}
		}
		return nil
	}
}

// ---------------------------------------------------------------------------
// Running the catalogue.

// specName returns the spec a scenario builds, without running it.
func (sc scenario) specName() string { return sc.build(&world{}).Name }

// outcome is what one scenario's run produced: the telemetry fact the example
// prints and the transcript the golden file pins.
type outcome struct {
	permutations int
	steps        int
	ok           int
	conflicts    int
	poisoned     int
	otherErrors  int
	blocked      int
	violations   []string
	transcript   string
}

// runScenario runs one scenario over every interleaving it declares.
func runScenario(ctx context.Context, sc scenario, w *world) (outcome, error) {
	spec := sc.build(w)
	var out outcome
	var check isolationtest.Observer
	if sc.check != nil {
		check = sc.check()
	}
	r := &isolationtest.Runner{
		NewEngine: w.engine,
		// Called on the runner's own goroutine, once per completed step, so the
		// counters need no synchronisation.
		Observe: func(o isolationtest.Observation) error {
			out.steps++
			switch {
			case o.Err == nil:
				out.ok++
			case errors.Is(o.Err, mvcc.ErrSerializationConflict):
				out.conflicts++
			case errors.Is(o.Err, cypher.ErrTxPoisoned):
				out.poisoned++
			default:
				out.otherErrors++
			}
			if check != nil {
				return check(o)
			}
			return nil
		},
	}
	var buf strings.Builder
	if err := r.Run(ctx, spec, &buf); err != nil {
		return out, err
	}
	out.transcript = buf.String()
	out.permutations = len(isolationtest.Permutations(spec))
	out.blocked = blockedSteps(out.transcript)
	out.violations = r.Violations()
	return out, nil
}

// blockedSteps counts the transcript's step lines that report a block. Only
// "step " lines count: a spec's Doc, rendered as "# " lines, may name the marker.
func blockedSteps(transcript string) int {
	n := 0
	for _, line := range strings.Split(transcript, "\n") {
		if !strings.HasPrefix(line, "step ") {
			continue
		}
		if strings.Contains(line, "<waiting ...>") || strings.Contains(line, "INVALID PERMUTATION") ||
			strings.Contains(line, "NEVER COMPLETED") {
			n++
		}
	}
	return n
}

// phaseCatalogue runs the whole catalogue and prints one deterministic fact per
// scenario: permutations run, step outcomes, and blocked steps. The verdict on
// each transcript is the golden file's, in catalogue_test.go; this phase is the
// telemetry an operator reads.
func phaseCatalogue(ctx context.Context, w io.Writer) error {
	fmt.Fprintln(w, "## phase 5 — deterministic MVCC scenario catalogue")
	var blocked, violations int
	cat := catalogue()
	for _, sc := range cat {
		if err := ctx.Err(); err != nil {
			return err
		}
		o, err := runScenario(ctx, sc, &world{})
		if err != nil {
			return fmt.Errorf("catalogue %s: %w", sc.ID, err)
		}
		blocked += o.blocked
		violations += len(o.violations)
		fmt.Fprintf(w, "catalogue.%s.%s spec=%s permutations=%d steps=%d ok=%d conflicts=%d poisoned=%d other_errors=%d blocked=%d violations=%d\n",
			sc.ID, sc.Driver, sc.specName(), o.permutations, o.steps, o.ok, o.conflicts, o.poisoned,
			o.otherErrors, o.blocked, len(o.violations))
	}
	fmt.Fprintf(w, "catalogue.scenarios=%d\n", len(cat))
	fmt.Fprintf(w, "catalogue.blocked_steps=%d\n", blocked)
	fmt.Fprintf(w, "catalogue.violations=%d\n", violations)
	return nil
}
