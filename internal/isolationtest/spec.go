// Package isolationtest is a declarative, exhaustive, deterministic harness for
// concurrent isolation scenarios — GoGraph's analogue of PostgreSQL's
// src/test/isolation ("isolationtester").
//
// # Why it exists
//
// GoGraph already has substantial isolation testing: ~199 test functions across
// graph/lpg/mvcc_*_test.go and store/txn/, the randomised DST battery in
// internal/sim, and the crash-injection battery in internal/crashinject. What
// none of them does is ENUMERATE the interleavings of a scripted scenario. Every
// concurrent isolation test either fixes one interleaving by hand or samples the
// space randomly.
//
// That gap is on the record. rmp #2333 was a torn total that could not be
// reproduced; rmp #2336 exists because a torn-total sighting from 2026-08-06 is
// still unexplained and has to be chased with a standing randomised search. A
// randomised search can FIND an anomaly; it cannot certify that a scenario is
// free of one, and it reproduces badly. This harness answers the complementary
// question — "over EVERY interleaving of these steps, is the observable outcome
// the expected one?" — and answers it the same way every run.
//
// # The reference, and what was taken from it
//
// Structure adopted from PostgreSQL's src/test/isolation, read at commit
// 0ec3f048bfc15c8eb9933e8228b847593389da1b (2026-08-07): a spec declares setup,
// teardown, named sessions each holding named steps, and optional explicit
// permutations; when no permutation is given the tester runs ALL interleavings
// with each session's own step order preserved; a step that blocks is reported
// as waiting rather than hanging; and each permutation's rendered output is
// compared against a golden file. PostgreSQL ships 135 such specs.
//
// The enumeration is PostgreSQL's "piles" recursion (isolationtester.c,
// run_all_permutations_recurse): conceptually each session's steps are a pile,
// and a permutation is produced by drawing from the piles in every order. It is
// re-implemented here, not transcribed — see [Permutations].
//
// THREE THINGS WERE DELIBERATELY NOT COPIED.
//
//   - The lex/yacc spec language. PostgreSQL needs a text format because its
//     steps are SQL strings shipped over libpq to separate backends; GoGraph's
//     steps are Cypher strings run through an in-process [cypher.Engine], so a
//     Go literal is exactly as declarative, is type-checked, and costs no
//     scanner or grammar to maintain. The spec is DATA either way, which is the
//     property that matters.
//
//   - Lock-view polling for blocking detection. isolationtester recognises a
//     blocked command by looking for it in pg_locks, and therefore only detects
//     heavyweight-lock waits. GoGraph has no such view, and — more to the point
//     — under MVCC-only concurrency control an ordinary read or write acquires
//     nothing and a write-write collision is REFUSED rather than queued, so
//     there is usually nothing to wait on at all. Blocking is detected here by
//     bounded timeout, which is what an out-of-process observer can actually
//     see, and it still catches the case that genuinely does block: a DDL
//     holding the exclusive schema gate.
//
//   - The stabilisation markers (`(*)`, `(othersteep)`, `notices <n>`).
//     PostgreSQL needs them because it launches the next step as soon as the
//     previous one is "done or deemed blocked", so two steps can be in flight
//     and complete in either order. This harness AWAITS each step before
//     launching the next unless that step is blocked, so within one permutation
//     the order of completions is fixed by construction and there is nothing to
//     stabilise. That is the whole determinism argument; see [Runner.Run].
//
// # Concurrency
//
// A [Spec] is immutable data and safe to share. A [Runner] is NOT safe for
// concurrent use, and each permutation gets a freshly built graph and engine, so
// two permutations never observe each other.
package isolationtest

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Control is a step body that drives the session's transaction lifecycle rather
// than the graph. PostgreSQL expresses these as ordinary SQL (`BEGIN;`,
// `COMMIT;`) because they ARE SQL there; in GoGraph they are API calls on
// [cypher.Engine] / [cypher.ExplicitTx], so the harness names them explicitly.
type Control string

const (
	// Begin opens an explicit read-write transaction on the session.
	Begin Control = "BEGIN"
	// BeginRead opens an explicit read-only transaction on the session.
	BeginRead Control = "BEGIN READ"
	// Commit commits the session's open transaction.
	Commit Control = "COMMIT"
	// Rollback rolls the session's open transaction back.
	Rollback Control = "ROLLBACK"
)

// Step is one named unit of work inside a session.
//
// Exactly one of Query and Ctl is set. A step with a Query and no open
// transaction runs as an autocommit statement, which is the same thing a bare
// statement does outside a transaction block in PostgreSQL.
type Step struct {
	// Name identifies the step and MUST be unique across the whole spec: it is
	// what makes a failing permutation re-runnable by name.
	Name string
	// Query is the Cypher this step executes. Empty when Ctl is set.
	Query string
	// Ctl is the transaction-control verb this step performs. Empty when Query
	// is set.
	Ctl Control
	// Hook is an escape hatch: a step body written in Go rather than in Cypher.
	//
	// It exists for the things a query language cannot express and an isolation
	// harness nevertheless has to script — a rendezvous between two sessions, a
	// deliberate wait, a fault injected at a precise point in an interleaving.
	// PostgreSQL needs no equivalent because SQL has pg_sleep and advisory
	// locks; GoGraph's step vocabulary is Cypher, which has neither.
	//
	// A Hook runs on its session's goroutine exactly where a Query would, so it
	// participates in blocking detection identically: a Hook that does not
	// return within the block timeout is reported as `<waiting …>` and its
	// completion is reported when it is observed. Label is what the transcript
	// prints in place of the query text, so a Hook step renders deterministically
	// (a Go closure has no stable text).
	Hook  func(ctx context.Context) error
	Label string
	// Params are the query parameters, if any. Rendered into the output so a
	// golden file records what was actually run.
	Params map[string]any
}

// isControl reports whether this step drives the transaction lifecycle.
func (s *Step) isControl() bool { return s.Ctl != "" }

// display is what the step renders as after "step <name>: ".
func (s *Step) display() string {
	if s.isControl() {
		return string(s.Ctl)
	}
	if s.Hook != nil {
		if s.Label != "" {
			return s.Label
		}
		return "<go hook>"
	}
	if len(s.Params) == 0 {
		return s.Query
	}
	keys := make([]string, 0, len(s.Params))
	for k := range s.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(s.Query)
	b.WriteString("  -- ")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "$%s=%v", k, s.Params[k])
	}
	return b.String()
}

// Session is one scripted actor. Every step of a session runs on that session's
// own goroutine against its own [cypher.Session], so a transaction opened by one
// step is the transaction the next step of the same session sees.
type Session struct {
	// Name identifies the session in the rendered output.
	Name string
	// Setup runs once per permutation, on this session, before any of its steps.
	// Typically a single Begin.
	Setup []Step
	// Steps are the session's units of work, in the order this session must run
	// them. Every enumerated permutation preserves this order.
	Steps []Step
	// Teardown runs once per permutation, on this session, after the permutation
	// completes. Errors here are rendered but do not fail the permutation, so a
	// COMMIT of an already-finished transaction is a harmless no-op to script.
	Teardown []Step
}

// Spec is a complete isolation scenario.
type Spec struct {
	// Name is the spec's identity; the golden file is testdata/<Name>.golden.
	Name string
	// Doc is prose describing what the scenario is FOR. It is rendered into the
	// golden output, so the file explains itself to whoever reads the diff.
	Doc string
	// Setup runs once per permutation on a control session, before any session
	// setup. Build the fixture here.
	Setup []Step
	// Teardown runs once per permutation on the control session, last.
	Teardown []Step
	// Sessions are the scripted actors. Their declaration order is the tie-break
	// the enumeration uses, so it fixes the order permutations are emitted in.
	Sessions []*Session
	// Permutations, when non-empty, are the ONLY interleavings run, each given
	// as a list of step names. When empty every valid interleaving is
	// enumerated. This mirrors PostgreSQL's rule exactly, and it is the escape
	// hatch for a scenario whose full enumeration is too large for its test
	// layer or whose steps genuinely block.
	Permutations [][]string
}

// Validate reports whether the spec is well-formed. It is called by [Runner.Run]
// before anything executes, because every failure mode it catches would
// otherwise surface as a confusing mid-permutation error.
func (s *Spec) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("isolationtest: spec has no Name")
	}
	if len(s.Sessions) == 0 {
		return fmt.Errorf("isolationtest: spec %q has no sessions", s.Name)
	}
	seenStep := make(map[string]string, 16)
	seenSession := make(map[string]struct{}, len(s.Sessions))
	for _, sess := range s.Sessions {
		if sess.Name == "" {
			return fmt.Errorf("isolationtest: spec %q has an unnamed session", s.Name)
		}
		if _, dup := seenSession[sess.Name]; dup {
			return fmt.Errorf("isolationtest: spec %q has two sessions named %q", s.Name, sess.Name)
		}
		seenSession[sess.Name] = struct{}{}
		if len(sess.Steps) == 0 {
			return fmt.Errorf("isolationtest: spec %q session %q has no steps", s.Name, sess.Name)
		}
		for _, st := range sess.Steps {
			if err := validateStep(s.Name, sess.Name, &st); err != nil {
				return err
			}
			if prev, dup := seenStep[st.Name]; dup {
				return fmt.Errorf("isolationtest: spec %q: step name %q is used by both session %q and session %q; "+
					"step names must be unique across the spec so a failing permutation can be named",
					s.Name, st.Name, prev, sess.Name)
			}
			seenStep[st.Name] = sess.Name
		}
		for _, st := range append(append([]Step{}, sess.Setup...), sess.Teardown...) {
			if err := validateStep(s.Name, sess.Name, &st); err != nil {
				return err
			}
		}
	}
	for _, st := range append(append([]Step{}, s.Setup...), s.Teardown...) {
		if err := validateStep(s.Name, "<control>", &st); err != nil {
			return err
		}
	}
	for i, perm := range s.Permutations {
		if len(perm) == 0 {
			return fmt.Errorf("isolationtest: spec %q: permutation %d is empty", s.Name, i)
		}
		for _, name := range perm {
			if _, ok := seenStep[name]; !ok {
				return fmt.Errorf("isolationtest: spec %q: permutation %d names step %q, which no session declares",
					s.Name, i, name)
			}
		}
	}
	return nil
}

func validateStep(spec, sess string, st *Step) error {
	if st.Name == "" {
		return fmt.Errorf("isolationtest: spec %q session %q has an unnamed step", spec, sess)
	}
	set := 0
	if st.Query != "" {
		set++
	}
	if st.Ctl != "" {
		set++
	}
	if st.Hook != nil {
		set++
	}
	if set != 1 {
		return fmt.Errorf("isolationtest: spec %q step %q must set exactly one of Query, Ctl and Hook", spec, st.Name)
	}
	switch st.Ctl {
	case "", Begin, BeginRead, Commit, Rollback:
	default:
		return fmt.Errorf("isolationtest: spec %q step %q has unknown control verb %q", spec, st.Name, st.Ctl)
	}
	return nil
}

// stepIndex maps every step name to the session that owns it and the step
// itself, for the named-permutation path.
func (s *Spec) stepIndex() (map[string]int, map[string]Step) {
	owner := make(map[string]int, 16)
	byName := make(map[string]Step, 16)
	for si, sess := range s.Sessions {
		for _, st := range sess.Steps {
			owner[st.Name] = si
			byName[st.Name] = st
		}
	}
	return owner, byName
}
