package isolationtest

// internal_test.go — the harness's own guard rails.
//
// These are unexported and they are the parts nobody notices until they are
// wrong: a spec validator that accepts a malformed spec, a parameter converter
// that silently drops a value, a diff that does not name the permutation it
// diverged in. This package's entire claim is that it is a trustworthy
// instrument, so its own edges are tested rather than assumed.

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestValidateRejectsMalformedSpecs enumerates every way a spec can be
// ill-formed. Each case must be REJECTED, and rejected with a message that says
// which spec and which step, because these fire while someone is writing a new
// scenario and a vague error there costs more than the check saves.
func TestValidateRejectsMalformedSpecs(t *testing.T) {
	t.Parallel()
	ok := Step{Name: "a", Query: "RETURN 1"}
	cases := []struct {
		name string
		spec *Spec
		want string
	}{
		{"no name", &Spec{Sessions: []*Session{{Name: "s", Steps: []Step{ok}}}}, "no Name"},
		{"no sessions", &Spec{Name: "x"}, "no sessions"},
		{"unnamed session", &Spec{Name: "x", Sessions: []*Session{{Steps: []Step{ok}}}}, "unnamed session"},
		{"duplicate session", &Spec{Name: "x", Sessions: []*Session{
			{Name: "s", Steps: []Step{ok}},
			{Name: "s", Steps: []Step{{Name: "b", Query: "RETURN 1"}}},
		}}, "two sessions named"},
		{"session with no steps", &Spec{Name: "x", Sessions: []*Session{{Name: "s"}}}, "has no steps"},
		{"unnamed step", &Spec{Name: "x", Sessions: []*Session{
			{Name: "s", Steps: []Step{{Query: "RETURN 1"}}},
		}}, "unnamed step"},
		{"step with neither body", &Spec{Name: "x", Sessions: []*Session{
			{Name: "s", Steps: []Step{{Name: "a"}}},
		}}, "exactly one of Query, Ctl and Hook"},
		{"step with two bodies", &Spec{Name: "x", Sessions: []*Session{
			{Name: "s", Steps: []Step{{Name: "a", Query: "RETURN 1", Ctl: Commit}}},
		}}, "exactly one of Query, Ctl and Hook"},
		{"unknown control verb", &Spec{Name: "x", Sessions: []*Session{
			{Name: "s", Steps: []Step{{Name: "a", Ctl: Control("VACUUM")}}},
		}}, "unknown control verb"},
		{"duplicate step across sessions", &Spec{Name: "x", Sessions: []*Session{
			{Name: "s1", Steps: []Step{ok}},
			{Name: "s2", Steps: []Step{ok}},
		}}, "step name \"a\" is used by both"},
		{"bad setup step", &Spec{Name: "x", Setup: []Step{{Name: "s", Query: "RETURN 1", Ctl: Commit}},
			Sessions: []*Session{{Name: "s1", Steps: []Step{ok}}}}, "exactly one of"},
		{"bad session setup step", &Spec{Name: "x", Sessions: []*Session{
			{Name: "s1", Setup: []Step{{Name: "bad"}}, Steps: []Step{ok}},
		}}, "exactly one of"},
		{"empty permutation", &Spec{Name: "x", Sessions: []*Session{{Name: "s", Steps: []Step{ok}}},
			Permutations: [][]string{{}}}, "is empty"},
		{"permutation names an unknown step", &Spec{Name: "x", Sessions: []*Session{{Name: "s", Steps: []Step{ok}}},
			Permutations: [][]string{{"nope"}}}, "which no session declares"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.spec.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a malformed spec (%s)", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not explain the problem\n got: %v\nwant it to contain: %q", err, tc.want)
			}
		})
	}
}

// TestValidateAcceptsWellFormedSpec is the positive control for the table above.
// Without it, a Validate that rejected EVERYTHING would pass every case there.
func TestValidateAcceptsWellFormedSpec(t *testing.T) {
	t.Parallel()
	s := &Spec{
		Name:     "good",
		Setup:    []Step{{Name: "mk", Query: "CREATE (:N)"}},
		Teardown: []Step{{Name: "rm", Query: "MATCH (n:N) DETACH DELETE n"}},
		Sessions: []*Session{{
			Name:     "s1",
			Setup:    []Step{{Name: "b", Ctl: Begin}},
			Steps:    []Step{{Name: "r", Query: "MATCH (n:N) RETURN count(n) AS n"}},
			Teardown: []Step{{Name: "c", Ctl: Rollback}},
		}},
		Permutations: [][]string{{"r"}},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate rejected a well-formed spec: %v", err)
	}
}

// TestPermutationsMatchTheCount asserts that the enumeration and the closed-form
// count agree.
//
// They are computed by completely different means — a recursion over piles and a
// multinomial in big.Int — so agreement is real evidence. It matters because the
// count is what a spec's TEST LAYER is chosen from: if it under-reported, a
// six-figure enumeration would be assigned to the short layer and blow the
// package's budget.
func TestPermutationsMatchTheCount(t *testing.T) {
	t.Parallel()
	shapes := [][]int{{1}, {1, 1}, {3, 3}, {2, 3, 4}, {4, 4, 2}, {1, 2, 3, 4}}
	for _, shape := range shapes {
		t.Run(shapeName(shape), func(t *testing.T) {
			t.Parallel()
			s := specOfShape(shape)
			got := Permutations(s)
			want := CountPermutations(s)
			if big.NewInt(int64(len(got))).Cmp(want) != 0 {
				t.Fatalf("enumerated %d permutations, CountPermutations says %s", len(got), want)
			}
			assertOrderPreservedAndDistinct(t, s, got)
		})
	}
}

// assertOrderPreservedAndDistinct checks the two properties that make the
// enumeration correct rather than merely numerous: every permutation preserves
// each session's own step order, and no permutation is produced twice.
func assertOrderPreservedAndDistinct(t *testing.T, s *Spec, perms []Permutation) {
	t.Helper()
	seen := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		if _, dup := seen[p.Name()]; dup {
			t.Fatalf("permutation %q produced more than once", p.Name())
		}
		seen[p.Name()] = struct{}{}
		next := make([]int, len(s.Sessions))
		for i, name := range p.Steps {
			si := p.Owner[i]
			want := s.Sessions[si].Steps[next[si]].Name
			if name != want {
				t.Fatalf("permutation %q runs %s out of order for session %s; expected %s next",
					p.Name(), name, s.Sessions[si].Name, want)
			}
			next[si]++
		}
	}
}

// TestExplicitPermutationsBypassEnumeration pins that a spec naming its own
// permutations gets exactly those — PostgreSQL's escape hatch, and the one this
// module relies on to keep a 4200-permutation scenario in the short layer.
func TestExplicitPermutationsBypassEnumeration(t *testing.T) {
	t.Parallel()
	s := specOfShape([]int{3, 3})
	s.Permutations = [][]string{{"s0a", "s1a", "s0b"}, {"s1a", "s0a"}}
	got := Permutations(s)
	if len(got) != 2 {
		t.Fatalf("explicit permutations: got %d, want 2", len(got))
	}
	if got[0].Name() != "s0a s1a s0b" {
		t.Errorf("first permutation is %q", got[0].Name())
	}
	// The owner tags must still be resolved, or the runner would send every step
	// to session 0.
	if got[1].Owner[0] != 1 || got[1].Owner[1] != 0 {
		t.Errorf("owners not resolved for an explicit permutation: %v", got[1].Owner)
	}
	if n := CountPermutations(s); n.Cmp(big.NewInt(2)) != 0 {
		t.Errorf("CountPermutations ignored the explicit list: %s", n)
	}
}

// TestToParamsConvertsEverySupportedKind covers the parameter converter,
// including its refusal. A converter that silently dropped an unsupported value
// would change what a scenario tests without changing its transcript — the
// fail-silent shape the module's failure-handling rule forbids outright.
func TestToParamsConvertsEverySupportedKind(t *testing.T) {
	t.Parallel()
	got, err := toParams(map[string]any{
		"i": 7, "i64": int64(8), "f": 1.5, "s": "x", "b": true,
	})
	if err != nil {
		t.Fatalf("toParams: %v", err)
	}
	want := map[string]string{"i": "7", "i64": "8", "f": "1.5", "s": `"x"`, "b": "true"}
	for k, w := range want {
		v, ok := got[k]
		if !ok {
			t.Errorf("parameter $%s missing", k)
			continue
		}
		if v.String() != w {
			t.Errorf("$%s = %s, want %s", k, v.String(), w)
		}
	}
	if _, err := toParams(nil); err != nil {
		t.Errorf("toParams(nil): %v", err)
	}
	if _, err := toParams(map[string]any{"bad": []int{1}}); err == nil {
		t.Error("toParams accepted an unsupported type; it must refuse rather than drop the value")
	}
}

// TestStepDisplayIsDeterministic covers the transcript rendering of a step,
// including the map-ordering hazard: a Go map iterates in random order, so a
// step with two parameters would render differently on every run and flip its
// golden file. Sorting the keys is what prevents it, and this asserts it.
func TestStepDisplayIsDeterministic(t *testing.T) {
	t.Parallel()
	st := Step{Name: "a", Query: "RETURN $x + $y AS s", Params: map[string]any{"y": 2, "x": 1}}
	first := st.display()
	for range 50 {
		if got := st.display(); got != first {
			t.Fatalf("display is not deterministic: %q then %q", first, got)
		}
	}
	if !strings.Contains(first, "$x=1, $y=2") {
		t.Errorf("parameters not rendered in sorted order: %q", first)
	}
	if got := (&Step{Name: "c", Ctl: Commit}).display(); got != "COMMIT" {
		t.Errorf("control step renders as %q", got)
	}
	hook := Step{Name: "h", Hook: func(context.Context) error { return nil }}
	if got := hook.display(); got != "<go hook>" {
		t.Errorf("unlabelled hook renders as %q, want the stable placeholder", got)
	}
	hook.Label = "<rendezvous>"
	if got := hook.display(); got != "<rendezvous>" {
		t.Errorf("labelled hook renders as %q", got)
	}
	if got := (&Step{Name: "q", Query: "RETURN 1"}).display(); got != "RETURN 1" {
		t.Errorf("bare query renders as %q", got)
	}
}

// TestClassifyErrorNormalisesTheOutcomesThatMatter pins the one deliberate
// abstraction in the transcript: a serialization conflict must read the same
// however the engine words it, so a reworded error message is not mistaken for
// an isolation change. Everything else is passed through verbatim, because
// hiding an unexpected error is how a golden file comes to bless a failure.
func TestClassifyErrorNormalisesTheOutcomesThatMatter(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"cypher: serialization failure on node 7": "serialization conflict",
		"write-write conflict detected":           "serialization conflict",
		"conflict on property chain":              "serialization conflict",
		"something else entirely":                 "something else entirely",
	}
	for in, want := range cases {
		if got := classifyError(errors.New(in)); got != want {
			t.Errorf("classifyError(%q) = %q, want %q", in, got, want)
		}
	}
	if got := classifyError(context.DeadlineExceeded); got != "deadline exceeded" {
		t.Errorf("classifyError(DeadlineExceeded) = %q", got)
	}
	if got := classifyError(context.Canceled); got != "canceled" {
		t.Errorf("classifyError(Canceled) = %q", got)
	}
}

// TestDiffLinesNamesAReplayablePermutation covers the failure renderer. It is
// the part a human only ever sees when something is already wrong, which is
// exactly why it must not itself be wrong: a diff that named no permutation
// would leave the reader with a mismatch and no way to reproduce it.
func TestDiffLinesNamesAReplayablePermutation(t *testing.T) {
	t.Parallel()
	want := "spec x: 1 sessions, 2 permutations\n" +
		"\nstarting permutation: a b\nstep a: RETURN 1\n" +
		"\nstarting permutation: b a\nstep b: RETURN 2\n"
	got := strings.Replace(want, "step b: RETURN 2", "step b: RETURN 99", 1)

	d := diffLines(want, got)
	if !strings.Contains(d, "permutation: b a") {
		t.Errorf("diff does not name the permutation that diverged:\n%s", d)
	}
	if !strings.Contains(d, `Runner{Only: "b a"}`) {
		t.Errorf("diff does not say how to replay it:\n%s", d)
	}
	if !strings.Contains(d, "RETURN 2") || !strings.Contains(d, "RETURN 99") {
		t.Errorf("diff does not show both sides:\n%s", d)
	}
	if d := diffLines(want, want); !strings.Contains(d, "only in trailing content") {
		t.Errorf("identical transcripts produced a divergence report:\n%s", d)
	}
}

// TestDiffLinesSuppressesRunawayOutput pins the bound on the diff. A transcript
// that diverges everywhere — the shape produced by a harness change rather than
// by an isolation change — must not print thousands of lines into the test log.
func TestDiffLinesSuppressesRunawayOutput(t *testing.T) {
	t.Parallel()
	var a, b strings.Builder
	for i := range 100 {
		a.WriteString("line A ")
		b.WriteString("line B ")
		a.WriteByte(byte('0' + i%10))
		b.WriteByte(byte('0' + i%10))
		a.WriteByte('\n')
		b.WriteByte('\n')
	}
	d := diffLines(a.String(), b.String())
	if !strings.Contains(d, "further differences suppressed") {
		t.Errorf("a fully divergent transcript was not truncated:\n%s", d[:min(len(d), 400)])
	}
}

func shapeName(shape []int) string {
	var b strings.Builder
	for i, n := range shape {
		if i > 0 {
			b.WriteByte('/')
		}
		b.WriteByte(byte('0' + n))
	}
	return b.String()
}

// specOfShape builds a spec with the given number of steps per session, each
// step a trivial read, so enumeration properties can be tested without an engine.
func specOfShape(shape []int) *Spec {
	s := &Spec{Name: "shape", Sessions: make([]*Session, len(shape))}
	for i, n := range shape {
		sess := &Session{Name: "s" + string(rune('0'+i)), Steps: make([]Step, n)}
		for j := range n {
			sess.Steps[j] = Step{
				Name:  "s" + string(rune('0'+i)) + string(rune('a'+j)),
				Query: "RETURN 1",
			}
		}
		s.Sessions[i] = sess
	}
	return s
}

// failingWriter fails after n successful writes, so a test can truncate a
// transcript at a chosen point.
type failingWriter struct {
	ok  int
	err error
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.ok > 0 {
		f.ok--
		return len(p), nil
	}
	return 0, f.err
}

// TestRunSurfacesATruncatedTranscript pins the claim made by [transcript]: a
// write that fails leaves a TRUNCATED transcript, and a truncated transcript
// either reports a spurious isolation change or — far worse — is accepted with
// -update as a golden that blesses a run which never finished. The latch turns
// it into a plain error instead, and this is what says so.
func TestRunSurfacesATruncatedTranscript(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk gone")
	// Two writes succeed — the header and the permutation heading — and the
	// third, the first step's own line, fails. A working engine is essential:
	// with a failing factory the run would abort before any step wrote anything
	// and the test would pass for the wrong reason.
	w := &failingWriter{ok: 2, err: boom}
	r := &Runner{NewEngine: func() (*Engine, error) {
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		return &Engine{Eng: cypher.NewEngine(g), Close: func() error { return nil }}, nil
	}}
	s := &Spec{Name: "trunc", Sessions: []*Session{{Name: "s", Steps: []Step{{Name: "a", Query: "RETURN 1"}}}}}

	err := r.Run(context.Background(), s, w)
	if err == nil {
		t.Fatal("Run reported success while its transcript was being truncated")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error does not wrap the write failure: %v", err)
	}
}

// TestTranscriptLatchesOnlyTheFirstError pins that the latch does not mask the
// original cause with a later one: the first failure is the diagnosis, and every
// write after it is a consequence.
func TestTranscriptLatchesOnlyTheFirstError(t *testing.T) {
	t.Parallel()
	first := errors.New("first")
	tr := &transcript{w: &failingWriter{ok: 0, err: first}}
	tr.printf("a")
	tr.w = &failingWriter{ok: 0, err: errors.New("second")}
	tr.printf("b")
	tr.print("c")
	if !errors.Is(tr.err, first) {
		t.Errorf("latched %v, want the FIRST error %v", tr.err, first)
	}
}
