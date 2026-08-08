package isolationtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// DefaultBlockTimeout is how long a step may take before the harness reports it
// as blocked and moves on.
//
// It is deliberately enormous relative to the work: a step in these specs is a
// single-node Cypher statement over a graph of a handful of nodes, which is
// microseconds even on the durable wiring. Two seconds therefore separates
// "genuinely waiting on something" from "slow", with four orders of magnitude of
// margin, so a loaded machine cannot turn a completed step into a `<waiting…>`
// line and flip a golden file.
//
// PostgreSQL's equivalent is 360 s, because its steps are real SQL against a
// real server. The number differs; the reasoning — pick a bound no healthy step
// can reach — is the same.
const DefaultBlockTimeout = 2 * time.Second

// DefaultPermutationTimeout bounds a whole permutation, so a scenario that
// deadlocks fails the test instead of hanging the package.
const DefaultPermutationTimeout = 30 * time.Second

// Engine is what a spec runs against: one freshly built engine plus whatever
// must be closed after. The harness builds a NEW one for every permutation, so
// no permutation can observe another's state — which is what makes a failing
// permutation reproducible in isolation.
type Engine struct {
	Eng   *cypher.Engine
	Close func() error
}

// EngineFactory builds the engine for one permutation. Supplying it rather than
// hard-wiring a wiring is what lets the same spec run against the in-memory
// engine and the WAL-backed one, which serialise on different mechanisms.
type EngineFactory func() (*Engine, error)

// Observation is one completed step's structured outcome, handed to an
// [Observer] so a spec can assert a PROPERTY rather than only diff a
// transcript.
type Observation struct {
	// Permutation is the interleaving in force, and is the exact string
	// [Runner.Only] takes to replay it.
	Permutation string
	// Step is the step's name.
	Step string
	// Cols and Rows are the step's result, as rendered strings. Strings rather
	// than typed values because the transcript is the contract: an invariant
	// must be checking the same thing the golden file records, not a parallel
	// view of it that could disagree.
	Cols []string
	Rows [][]string
	// Err is the step's error, if any.
	Err error
}

// Observer is called once per completed step. Returning an error marks the
// invariant violated; the runner records it and keeps going, so ONE run reports
// EVERY interleaving that breaks the property instead of only the first.
//
// This is what makes the harness an assertion and not just a recorder. A golden
// file pins that behaviour did not CHANGE; an Observer pins that behaviour is
// CORRECT — and the two fail for different reasons, which is the point.
type Observer func(Observation) error

// Runner executes a spec.
//
// Runner is NOT safe for concurrent use.
type Runner struct {
	// NewEngine builds the per-permutation engine. Required.
	NewEngine EngineFactory
	// BlockTimeout overrides [DefaultBlockTimeout] when non-zero.
	BlockTimeout time.Duration
	// PermutationTimeout overrides [DefaultPermutationTimeout] when non-zero.
	PermutationTimeout time.Duration
	// Only, when non-empty, restricts the run to the single permutation with
	// this name — the exact string a golden file prints after "starting
	// permutation: ". This is the re-runnability contract: a failing
	// permutation is named in the diff and can be replayed on its own.
	Only string
	// Observe, when non-nil, is called for every completed step. See [Observer].
	Observe Observer
	// violations accumulates what Observe rejected, across every permutation.
	violations []string
}

// Violations returns every invariant breach [Runner.Observe] reported during the
// last [Runner.Run], each naming the permutation and step it occurred in.
//
// It is a slice rather than a bool because a scenario that breaks its invariant
// usually breaks it in a FAMILY of interleavings, and which ones is the whole
// diagnosis: "s2 tears whenever it reads between the debit and the credit" is
// actionable, "some permutation failed" is not.
func (r *Runner) Violations() []string { return r.violations }

// ErrNoPermutation is returned when Only names a permutation the spec does not
// produce.
var ErrNoPermutation = errors.New("isolationtest: no such permutation")

// Run executes the spec and writes the rendered transcript to w.
//
// The transcript is the artefact under test: it is compared against a golden
// file, so anything nondeterministic in it is a defect in this harness rather
// than a fact about the engine.
//
// # Why the transcript is deterministic
//
// Within one permutation the runner LAUNCHES a step and then AWAITS it before
// launching the next. Only a step that is still running after BlockTimeout is
// left in flight, and it is reported as `<waiting …>` at that point; its
// completion is reported later, at the first point the transcript reaches where
// it has been observed to finish. So the order of the transcript's lines is
// fixed by the permutation, not by the scheduler, and the only way two runs can
// differ is if a step's own RESULT differs — which is exactly the thing the
// golden file is there to catch.
//
// This is why the harness needs none of PostgreSQL's stabilisation markers: it
// buys determinism by serialising the observation, where isolationtester buys
// throughput by overlapping it.
func (r *Runner) Run(ctx context.Context, s *Spec, out io.Writer) error {
	w := &transcript{w: out}
	if err := s.Validate(); err != nil {
		return err
	}
	if r.NewEngine == nil {
		return fmt.Errorf("isolationtest: spec %q: Runner.NewEngine is nil", s.Name)
	}
	r.violations = nil
	perms := Permutations(s)
	if r.Only != "" {
		kept := perms[:0]
		for _, p := range perms {
			if p.Name() == r.Only {
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			return fmt.Errorf("%w: %q in spec %q", ErrNoPermutation, r.Only, s.Name)
		}
		perms = kept
	}

	w.printf("spec %s: %d sessions, %d permutations\n", s.Name, len(s.Sessions), len(perms))
	if s.Doc != "" {
		for _, line := range strings.Split(strings.TrimSpace(s.Doc), "\n") {
			w.printf("# %s\n", strings.TrimRight(line, " \t"))
		}
	}
	for _, p := range perms {
		w.printf("\nstarting permutation: %s\n", p.Name())
		if err := r.runPermutation(ctx, s, p, w); err != nil {
			return fmt.Errorf("isolationtest: spec %q permutation %q: %w", s.Name, p.Name(), err)
		}
	}
	if w.err != nil {
		return fmt.Errorf("isolationtest: spec %q: writing transcript: %w", s.Name, w.err)
	}
	return nil
}

// transcript is the run's output, with the first write error latched.
//
// It exists because a transcript is the artefact under test: a write that failed
// halfway leaves a TRUNCATED transcript, and a truncated transcript diffed
// against a golden file reports a spurious isolation change — or, worse, matches
// a golden that was itself written from a truncated run. Latching the first
// error and surfacing it from [Runner.Run] turns that into a plain failure.
type transcript struct {
	w   io.Writer
	err error
}

func (t *transcript) printf(format string, args ...any) {
	if t.err != nil {
		return
	}
	_, t.err = fmt.Fprintf(t.w, format, args...)
}

func (t *transcript) print(s string) {
	if t.err != nil || s == "" {
		return
	}
	_, t.err = io.WriteString(t.w, s)
}

// stepResult is what a session goroutine sends back after running one step.
type stepResult struct {
	step   Step
	render string
	cols   []string
	rows   [][]string
	err    error
}

// sessionRunner owns one scripted session: its goroutine, its cypher session,
// and its open explicit transaction if any.
type sessionRunner struct {
	name string
	sess *cypher.Session
	tx   *cypher.ExplicitTx
	ctx  context.Context //nolint:containedctx // the permutation's context, owned by the runner that built this
	in   chan Step
	out  chan stepResult
	// pending is the step this session was last handed and has not been observed
	// to finish. Non-nil exactly while the session is deemed blocked.
	pending *Step
}

// loop is the session's goroutine. One step in, one result out, forever, so the
// session's own step order is preserved by construction rather than by
// agreement.
func (sr *sessionRunner) loop() {
	for st := range sr.in {
		got, err := sr.exec(&st)
		got.step, got.err = st, err
		sr.out <- got
	}
}

// exec runs one step and returns both the rendered transcript fragment and the
// structured rows behind it.
//
// BOTH, deliberately. The transcript is what the golden file diffs; the
// structured rows are what an [Observer] asserts on. A harness that offered only
// the text would make every invariant a string match, and a harness that offered
// only the rows would lose the diffable artefact — the two answer different
// questions and the cost of carrying both is one slice.
func (sr *sessionRunner) exec(st *Step) (stepResult, error) {
	if st.isControl() {
		return stepResult{step: *st}, sr.control(st.Ctl)
	}
	if st.Hook != nil {
		return stepResult{step: *st}, st.Hook(sr.ctx)
	}
	params, err := toParams(st.Params)
	if err != nil {
		return stepResult{step: *st}, err
	}
	var res *cypher.Result
	if sr.tx != nil {
		res, err = sr.tx.Exec(st.Query, params)
	} else {
		res, err = sr.sess.RunInTx(sr.ctx, st.Query, params)
	}
	if err != nil {
		return stepResult{step: *st}, err
	}
	defer func() { _ = res.Close() }()
	if err := res.Err(); err != nil {
		return stepResult{step: *st}, err
	}
	cols, rows := drainRows(res)
	return stepResult{step: *st, render: renderRows(cols, rows), cols: cols, rows: rows}, nil
}

// control drives the session's transaction lifecycle.
func (sr *sessionRunner) control(c Control) error {
	switch c {
	case Begin, BeginRead:
		if sr.tx != nil {
			return fmt.Errorf("session %s already has an open transaction", sr.name)
		}
		var err error
		if c == Begin {
			sr.tx, err = sr.sess.BeginTx(sr.ctx)
		} else {
			sr.tx, err = sr.sess.BeginReadTx(sr.ctx)
		}
		return err
	case Commit, Rollback:
		if sr.tx == nil {
			return fmt.Errorf("session %s has no open transaction", sr.name)
		}
		tx := sr.tx
		sr.tx = nil
		if c == Commit {
			return tx.Commit()
		}
		return tx.Rollback()
	default:
		return fmt.Errorf("unknown control verb %q", c)
	}
}

// runPermutation runs one interleaving end to end against a fresh engine.
func (r *Runner) runPermutation(ctx context.Context, s *Spec, p Permutation, w *transcript) error {
	permTimeout := r.PermutationTimeout
	if permTimeout <= 0 {
		permTimeout = DefaultPermutationTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, permTimeout)
	defer cancel()

	env, err := r.NewEngine()
	if err != nil {
		return fmt.Errorf("build engine: %w", err)
	}
	defer func() {
		if cerr := env.Close(); cerr != nil {
			w.printf("ERROR closing engine: %v\n", cerr)
		}
	}()

	// Control session: the spec-level setup and teardown, on a session of its
	// own so a fixture build cannot be mistaken for one of the actors' work.
	control := &sessionRunner{name: "<control>", sess: env.Eng.NewSession(), ctx: ctx}
	for _, st := range s.Setup {
		if _, serr := control.exec(&st); serr != nil {
			return fmt.Errorf("setup step %q: %w", st.Name, serr)
		}
	}

	runners := make([]*sessionRunner, len(s.Sessions))
	for i, sess := range s.Sessions {
		sr := &sessionRunner{
			name: sess.Name,
			sess: env.Eng.NewSession(),
			ctx:  ctx,
			in:   make(chan Step),
			out:  make(chan stepResult, 1),
		}
		runners[i] = sr
		go sr.loop()
		for _, st := range sess.Setup {
			if _, serr := sr.exec(&st); serr != nil {
				return fmt.Errorf("session %s setup step %q: %w", sess.Name, st.Name, serr)
			}
		}
	}
	// Close every session's input channel so its goroutine exits. Done on the
	// way out rather than at the end of the happy path so an early return —
	// including a permutation that timed out — still terminates them: an
	// abandoned goroutine here would be a leak goleak reports, in the harness
	// whose whole job is to be trustworthy.
	defer func() {
		for _, sr := range runners {
			close(sr.in)
		}
	}()

	blockTimeout := r.BlockTimeout
	if blockTimeout <= 0 {
		blockTimeout = DefaultBlockTimeout
	}

	_, byName := s.stepIndex()
	for i, name := range p.Steps {
		sr := runners[p.Owner[i]]
		st := byName[name]

		// (A) A session that is still blocked cannot be handed its next step.
		// PostgreSQL calls such an interleaving invalid and cancels it after a
		// timeout; reporting it immediately is strictly better, because it is
		// deterministic, fast, and names the step that is stuck.
		if sr.pending != nil {
			if err := r.drain(sr, p, w, blockTimeout); err != nil {
				return err
			}
			if sr.pending != nil {
				w.printf("step %s: INVALID PERMUTATION — session %s is still blocked on %s\n",
					name, sr.name, sr.pending.Name)
				return nil
			}
		}

		sr.in <- st
		sr.pending = &st
		select {
		case got := <-sr.out:
			sr.pending = nil
			writeStep(w, &got, "")
			r.observe(p, &got)
		case <-time.After(blockTimeout):
			w.printf("step %s: %s <waiting ...>\n", st.Name, st.display())
		case <-ctx.Done():
			return fmt.Errorf("permutation timed out at step %q: %w", st.Name, ctx.Err())
		}

		// Report any OTHER session's blocked step that this step released. Done
		// after every step, in session declaration order, so the transcript
		// records the release at a fixed point rather than wherever the
		// scheduler happened to get to.
		for _, other := range runners {
			if other == sr || other.pending == nil {
				continue
			}
			select {
			case got := <-other.out:
				other.pending = nil
				writeStep(w, &got, "<... completed>")
				r.observe(p, &got)
			default:
			}
		}
	}

	// Every step launched; collect whatever is still in flight before teardown,
	// so a transcript never ends with an unaccounted step.
	for _, sr := range runners {
		if sr.pending == nil {
			continue
		}
		if err := r.drain(sr, p, w, blockTimeout); err != nil {
			return err
		}
		if sr.pending != nil {
			w.printf("step %s: NEVER COMPLETED (session %s still blocked at end of permutation)\n",
				sr.pending.Name, sr.name)
			return nil
		}
	}

	for i, sess := range s.Sessions {
		for _, st := range sess.Teardown {
			if _, terr := runners[i].exec(&st); terr != nil {
				w.printf("teardown %s: %v\n", st.Name, terr)
			}
		}
	}
	for _, st := range s.Teardown {
		if _, terr := control.exec(&st); terr != nil {
			w.printf("teardown %s: %v\n", st.Name, terr)
		}
	}
	return nil
}

// observe hands a completed step to the Observer, if any, and records what it
// rejected. Violations are ACCUMULATED rather than raised, so a single run
// enumerates every interleaving that breaks the invariant.
func (r *Runner) observe(p Permutation, got *stepResult) {
	if r.Observe == nil {
		return
	}
	err := r.Observe(Observation{
		Permutation: p.Name(),
		Step:        got.step.Name,
		Cols:        got.cols,
		Rows:        got.rows,
		Err:         got.err,
	})
	if err != nil {
		r.violations = append(r.violations,
			fmt.Sprintf("permutation %q step %s: %v", p.Name(), got.step.Name, err))
	}
}

// drain waits up to timeout for a session's in-flight step and reports it.
func (r *Runner) drain(sr *sessionRunner, perm Permutation, w *transcript, timeout time.Duration) error {
	select {
	case got := <-sr.out:
		sr.pending = nil
		writeStep(w, &got, "<... completed>")
		r.observe(perm, &got)
		return nil
	case <-time.After(timeout):
		return nil
	}
}

// writeStep renders one completed step. suffix distinguishes a step reported at
// its launch point from one reported later, after it was seen to block.
func writeStep(w *transcript, got *stepResult, suffix string) {
	if suffix != "" {
		w.printf("step %s: %s\n", got.step.Name, suffix)
	} else {
		w.printf("step %s: %s\n", got.step.Name, got.step.display())
	}
	if got.err != nil {
		w.printf("ERROR: %s\n", classifyError(got.err))
		return
	}
	if got.render != "" {
		w.print(got.render)
	}
}

// classifyError renders an error so the golden file records WHAT failed without
// pinning the exact message text. A serialization conflict is the outcome these
// specs care about, and it must read the same however the engine words it.
func classifyError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "serialization") || strings.Contains(msg, "serialisation") ||
		strings.Contains(msg, "write-write") || strings.Contains(msg, "conflict"):
		return "serialization conflict"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return msg
	}
}

// drainRows reads every row of a result into strings. Rows come back in the
// order the engine produced them, so a spec whose output must be stable has to
// say ORDER BY — exactly as PostgreSQL's specs do.
func drainRows(res *cypher.Result) ([]string, [][]string) {
	cols := res.Columns()
	if len(cols) == 0 {
		return nil, nil
	}
	rows := make([][]string, 0, 8)
	for res.Next() {
		cells := make([]string, len(cols))
		for i := range cols {
			// Positional, not by name: ValueAt serves a streaming result as well
			// as a materialised one, whereas the map form of a row is typed
			// `any` and would force a type assertion per cell (#2215).
			if v := res.ValueAt(i); v != nil {
				cells[i] = v.String()
			} else {
				cells[i] = "null"
			}
		}
		rows = append(rows, cells)
	}
	return cols, rows
}

// renderRows formats a drained result the way PostgreSQL's isolationtester does:
// a header, a rule, the rows, then the count.
func renderRows(cols []string, rows [][]string) string {
	if len(cols) == 0 {
		return ""
	}
	width := make([]int, len(cols))
	for i, c := range cols {
		width[i] = len(c)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > width[i] {
				width[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	for i, c := range cols {
		if i > 0 {
			b.WriteString("|")
		}
		fmt.Fprintf(&b, "%-*s", width[i], c)
	}
	b.WriteString("\n")
	for i := range cols {
		if i > 0 {
			b.WriteString("+")
		}
		b.WriteString(strings.Repeat("-", width[i]))
	}
	b.WriteString("\n")
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				b.WriteString("|")
			}
			fmt.Fprintf(&b, "%-*s", width[i], cell)
		}
		b.WriteString("\n")
	}
	if len(rows) == 1 {
		b.WriteString("(1 row)\n")
	} else {
		fmt.Fprintf(&b, "(%d rows)\n", len(rows))
	}
	return b.String()
}

// toParams converts a spec's plain-Go parameter map into engine values. Only the
// kinds a spec needs are supported; anything else is an error rather than a
// silent Null, because a parameter that quietly vanished would change what the
// scenario tests without changing its transcript.
func toParams(in map[string]any) (map[string]expr.Value, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]expr.Value, len(in))
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch v := in[k].(type) {
		case int:
			out[k] = expr.IntegerValue(int64(v))
		case int64:
			out[k] = expr.IntegerValue(v)
		case float64:
			out[k] = expr.FloatValue(v)
		case string:
			out[k] = expr.StringValue(v)
		case bool:
			out[k] = expr.BoolValue(v)
		default:
			return nil, fmt.Errorf("isolationtest: parameter $%s has unsupported type %T", k, in[k])
		}
	}
	return out, nil
}
