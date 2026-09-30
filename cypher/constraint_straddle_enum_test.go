package cypher

// constraint_straddle_enum_test.go — exhaustive enumeration of a transaction
// straddling constraint DDL against a concurrent peer (rmp #2936, fourth and
// fifth audits).
//
// Layer: short for a deterministic slice of the space; soak for all of it on
// the in-memory wiring and nightly for all of it on the WAL-backed wiring
// (constraint_straddle_enum_soak_test.go).
//
// # The space
//
// Two families, over a seeded graph, each interleaved with ONE constraint DDL
// that is a CREATE, a DROP, a DROP followed by a CREATE, or a DROP whose
// durable commit fails and is rewound; the straddler T ends by commit or
// rollback; the engine is in-memory or WAL-backed.
//
//   - Single operations: T and the peer P each run ONE operation from
//     straddleOps, in every order of {T begin, T write, T end, P begin,
//     P write, P commit, DDL} that keeps each transaction's own events in order
//     (7!/(3!·3!) = 140 orders); the constraint is UNIQUE or NOT NULL.
//     2 kinds × 4 DDL shapes × 9 × 9 operations × 140 orders × 2 ends =
//     181,440 cases per wiring.
//   - Sequences: T runs TWO operations — a release of the value node 1 holds
//     (its label, its value, its property or its life) and a reservation of
//     that value (on a new node, on a new node deleted in the same statement,
//     or on node 2), in either order — and P runs one operation from
//     straddleOps, in every order of the eight events (8!/(4!·3!) = 280
//     orders), so the DDL falls before, between and after T's two writes. A
//     release followed by a reservation of the same value is the shape of audit
//     R5-1, which one operation per transaction cannot express. UNIQUE only:
//     NOT NULL keeps no value-set and no reservation, and its commit check reads
//     T's final state alone, which the single-operation family already covers
//     for every node T can leave. 4 DDL shapes × 24 sequences × 9 operations ×
//     280 orders × 2 ends = 483,840 cases per wiring.
//
// 665,280 cases per wiring in all.
//
// # The oracle
//
//  1. While the constraint is live at the end, the committed graph satisfies it.
//  2. For UNIQUE, the value-set equals the set the committed graph holds, over
//     the whole value universe: a held value is refused to a new node, and a
//     value no node holds is accepted — no missing value and no phantom.
//  3. A refusal is justified by a node-level write-write conflict, whatever
//     error reports it: the two transactions were concurrent — their
//     lifetimes overlap — and the other one wrote, before the refusal, a node
//     the refused one wrote (see straddleConflict). Otherwise a
//     serialization conflict never is, and a constraint violation only when a
//     serial order — the other transaction first, or the refused one alone —
//     also violates. The serial order keeps the constraint's timeline: an other
//     transaction that committed having written only before a CREATE
//     CONSTRAINT runs before it, and a CREATE CONSTRAINT that fell between a
//     transaction's own statements stays between them.
//
// A refusal justified only because the refused transaction violates ALONE,
// with the CREATE CONSTRAINT between the same statements, is not caused by the
// other transaction: it is the engine's behaviour for a straddler whose
// release precedes the constraint and whose reservation of the same value
// follows it — the statement-time UNIQUE check sees no release mark, since
// none was recorded before the constraint existed, and refuses a transaction
// whose final state is valid. Those refusals are counted and reported
// separately (straddleReport.isolated), never passed silently. Two transactions that share a node without overlapping in
//     time have no conflict, so a refusal between them is an over-refusal.
//
// A case whose constraint is not live at the end — the DROP shape by design,
// and a CREATE refused by its validation, which reads uncommitted writes and is
// conservative — is judged by rule 3 only; the enumeration counts those cases
// per DDL shape and reports the counts.
//
// # The mutation check
//
// TestConstraintStraddle_EnumerationDetectsSeededDefect restores the R5-1
// defect inside the real validation ([exec.ConstraintRegistry.SetStraddleAnyMarkForTest])
// and requires the oracle to report it, so a green enumeration is evidence and
// not an oracle that cannot fail.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// straddleSeed is the committed graph every case starts from.
const straddleSeed = `CREATE (:L {id: 1, s: 'a'}), ({id: 2, s: 'b'}), ({id: 3})`

var straddleDebug = false

// straddleValues is the value universe the value-set is checked over.
var straddleValues = []string{"a", "b", "x", "y"}

// straddleOp is one operation a transaction runs, and the seeded nodes it
// touches (a created node touches nothing the other side can name).
type straddleOp struct {
	name  string
	query string
	nodes []int
}

var straddleOps = []straddleOp{
	{"gainL2", `MATCH (n {id: 2}) SET n:L`, []int{2}},
	{"gainL3", `MATCH (n {id: 3}) SET n:L`, []int{3}},
	{"loseL1", `MATCH (n {id: 1}) REMOVE n:L`, []int{1}},
	{"set2a", `MATCH (n {id: 2}) SET n.s = 'a'`, []int{2}},
	{"chg1x", `MATCH (n {id: 1}) SET n.s = 'x'`, []int{1}},
	{"rm1s", `MATCH (n {id: 1}) REMOVE n.s`, []int{1}},
	{"createX", `CREATE (:L {id: 9, s: 'x'})`, nil},
	{"del1", `MATCH (n {id: 1}) DETACH DELETE n`, []int{1}},
	{"move1", `MATCH (n {id: 1}) SET n.s = 'y' CREATE (:L {id: 8, s: 'a'})`, []int{1}},
}

// straddleOpNamed returns the operation of straddleOps called name.
func straddleOpNamed(name string) straddleOp {
	for _, op := range straddleOps {
		if op.name == name {
			return op
		}
	}
	panic("straddle: no operation " + name)
}

// straddleSequences returns the straddler's two-operation sequences: every
// release of node 1's value 'a' paired with every reservation of 'a', in both
// orders — 4 × 3 × 2 = 24.
func straddleSequences() [][]straddleOp {
	releases := []straddleOp{straddleOpNamed("loseL1"), straddleOpNamed("chg1x"), straddleOpNamed("rm1s"), straddleOpNamed("del1")}
	reserves := []straddleOp{
		{"createA", `CREATE (:L {id: 7, s: 'a'})`, nil},
		{"createDelA", `CREATE (n:L {id: 7, s: 'a'}) DELETE n`, nil},
		straddleOpNamed("set2a"),
	}
	out := make([][]straddleOp, 0, 2*len(releases)*len(reserves))
	for _, rel := range releases {
		for _, res := range reserves {
			out = append(out, []straddleOp{rel, res}, []straddleOp{res, rel})
		}
	}
	return out
}

var straddleDDLKinds = map[string]string{
	"unique":  "CREATE CONSTRAINT c FOR (n:L) REQUIRE n.s IS UNIQUE",
	"notnull": "CREATE CONSTRAINT c FOR (n:L) REQUIRE n.s IS NOT NULL",
}

var straddleDDLShapes = []string{"create", "drop", "drop-create", "drop-rewound"}

// straddleOrders returns every order of T's begin, tWrites writes and end, P's
// begin, write and commit, and the DDL, that keeps T's and P's own events in
// order.
func straddleOrders(tWrites int) [][]string {
	tEv := []string{"Tb"}
	for i := 1; i <= tWrites; i++ {
		tEv = append(tEv, fmt.Sprintf("Tw%d", i))
	}
	tEv = append(tEv, "Te")
	pEv := []string{"Pb", "Pw", "Pc"}
	var out [][]string
	var rec func(prefix []string, t, p int, d bool)
	rec = func(prefix []string, t, p int, d bool) {
		if t == len(tEv) && p == len(pEv) && d {
			out = append(out, append([]string(nil), prefix...))
			return
		}
		if t < len(tEv) {
			rec(append(prefix, tEv[t]), t+1, p, d)
		}
		if p < len(pEv) {
			rec(append(prefix, pEv[p]), t, p+1, d)
		}
		if !d {
			rec(append(prefix, "D"), t, p, true)
		}
	}
	rec(nil, 0, 0, false)
	return out
}

// straddleCase is one point of the space.
type straddleCase struct {
	kind, shape string
	tOps        []straddleOp
	pOp         straddleOp
	order       []string
	rollback    bool
	walBacked   bool
}

func (c *straddleCase) String() string {
	end := "commit"
	if c.rollback {
		end = "rollback"
	}
	w := "mem"
	if c.walBacked {
		w = "wal"
	}
	return fmt.Sprintf("%s/%s/T=%s/P=%s/%v/%s/%s", c.kind, c.shape, straddleSeqName(c.tOps), c.pOp.name, c.order, end, w)
}

func straddleSeqName(ops []straddleOp) string {
	names := make([]string, len(ops))
	for i, op := range ops {
		names[i] = op.name
	}
	return strings.Join(names, "+")
}

// straddleSide is one transaction of a case: its operations, the positions in
// the order of its begin, its writes and its end, and how it went.
type straddleSide struct {
	ops        []straddleOp
	begin, end int
	wpos       []int
	ran        []bool // ran[i]: write i ran, successfully or not
	err        error  // the first refusal: a statement's or the commit's
	errAt      int    // the position of the event that returned err
	committed  bool
}

// lastWrite returns the position of s's last write.
func (s *straddleSide) lastWrite() int {
	last := -1
	for _, p := range s.wpos {
		last = max(last, p)
	}
	return last
}

// straddleConflict reports whether self's refusal is justified by a node-level
// write-write conflict with other: the two transactions were concurrent —
// other ended after self began — and a write other ran before self's refusal
// touched a node that a write self ran up to its refusal touched.
//
// A write counts once it ran, whether its statement succeeded or not: a failed
// statement poisons its transaction, and its partial writes — reservations
// included — stand until the transaction rolls back. Concurrency is the
// overlap of the two lifetimes, the definition first-committer-wins is stated
// over (Berenson et al., "A Critique of ANSI SQL Isolation Levels", SIGMOD
// 1995, §4.2); it includes a peer that rolled back before self wrote, which
// this engine refuses conservatively.
func straddleConflict(self, other *straddleSide) bool {
	if other.end < self.begin {
		return false
	}
	for i, ws := range self.ops {
		if self.wpos[i] > self.errAt {
			continue
		}
		for j, wo := range other.ops {
			if other.ran[j] && other.wpos[j] < self.errAt && straddleShareNode(ws, wo) {
				return true
			}
		}
	}
	return false
}

// straddleRunSeq runs ops on eng: one autocommit statement for one operation,
// one explicit transaction for several.
func straddleRunSeq(eng *Engine, ops []straddleOp) error {
	if len(ops) == 1 {
		return straddleRun(eng, ops[0].query)
	}
	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		return err
	}
	for _, op := range ops {
		if err := txExecErr(tx, op.query); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return nil
}

// straddleSerialViolates reports whether ops violates the constraint when run
// alone after first (which may be nil) on a fresh in-memory engine carrying the
// seed and the constraint. With firstBefore, first runs BEFORE the constraint is
// created, as it did in a case whose other transaction wrote only before a
// CREATE CONSTRAINT and committed; the reference then has no constraint when
// the creation is refused, and nothing violates it. Cached: the answer depends on (kind, first,
// firstBefore, ops) only.
func straddleSerialViolates(t *testing.T, cache *straddleCache, kind string, first []straddleOp, firstBefore bool, ops []straddleOp) bool {
	key := kind + "/" + straddleSeqName(ops)
	if first != nil {
		key += "/after/" + straddleSeqName(first)
		if firstBefore {
			key += "/unconstrained"
		}
	}
	cache.mu.Lock()
	v, ok := cache.m[key]
	cache.mu.Unlock()
	if ok {
		return v
	}
	eng, _, _ := straddleEngine(t, false)
	_ = straddleRun(eng, straddleSeed)
	constrained := true
	if first != nil && firstBefore {
		_ = straddleRunSeq(eng, first)
		constrained = straddleRun(eng, straddleDDLKinds[kind]) == nil
	} else {
		_ = straddleRun(eng, straddleDDLKinds[kind])
		if first != nil {
			_ = straddleRunSeq(eng, first)
		}
	}
	v = constrained && errors.Is(straddleRunSeq(eng, ops), exec.ErrConstraintViolation)
	cache.mu.Lock()
	cache.m[key] = v
	cache.mu.Unlock()
	return v
}

// straddleScript is one transaction's events relative to the DDL, in the
// order a case ran them: "w0" and "w1" for its writes, "D" for the DDL and "C"
// for its commit. prefix names the side ('T' or 'P').
func straddleScript(order []string, prefix byte) []string {
	var out []string
	for _, ev := range order {
		switch {
		case ev == "D":
			out = append(out, "D")
		case ev[0] != prefix || ev[1] == 'b':
		case ev == "Te" || ev == "Pc":
			out = append(out, "C")
		case ev == "Pw":
			out = append(out, "w0")
		default:
			out = append(out, "w"+string(rune(ev[2]-1)))
		}
	}
	return out
}

// straddleReplayViolates reports whether the transaction (ops, script) violates
// the constraint when run after the transaction (firstOps, firstScript), which
// may be nil, with no overlap between the two and the CREATE CONSTRAINT at the
// position each script gives it — the first occurrence runs it. It is the serial
// reference for a case whose DDL fell between a transaction's own statements.
func straddleReplayViolates(t *testing.T, cache *straddleCache, kind string, firstOps []straddleOp, firstScript []string, ops []straddleOp, script []string) bool {
	key := fmt.Sprintf("replay/%s/%s%v/%s%v", kind, straddleSeqName(firstOps), firstScript, straddleSeqName(ops), script)
	cache.mu.Lock()
	v, ok := cache.m[key]
	cache.mu.Unlock()
	if ok {
		return v
	}
	eng, _, _ := straddleEngine(t, false)
	_ = straddleRun(eng, straddleSeed)
	ddlDone, constrained := false, false
	run := func(ops []straddleOp, script []string) error {
		tx, err := eng.BeginTx(context.Background())
		if err != nil {
			return err
		}
		for _, step := range script {
			switch step {
			case "D":
				if !ddlDone {
					ddlDone = true
					constrained = straddleRun(eng, straddleDDLKinds[kind]) == nil
				}
			case "C":
				if err := tx.Commit(); err != nil {
					_ = tx.Rollback()
					return err
				}
				return nil
			default:
				if err := txExecErr(tx, ops[step[1]-'0'].query); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
		}
		_ = tx.Rollback()
		return nil
	}
	if firstOps != nil {
		_ = run(firstOps, firstScript)
	}
	err := run(ops, script) // before reading constrained, which the run sets
	v = constrained && errors.Is(err, exec.ErrConstraintViolation)
	cache.mu.Lock()
	cache.m[key] = v
	cache.mu.Unlock()
	return v
}

// straddleCache memoises straddleSerialViolates; safe for concurrent use.
type straddleCache struct {
	mu sync.Mutex
	m  map[string]bool
}

func newStraddleCache() *straddleCache { return &straddleCache{m: map[string]bool{}} }

// straddleReport collects an enumeration's findings and its per-shape count of
// cases whose constraint was not live at the end. Safe for concurrent use.
type straddleReport struct {
	t       *testing.T
	quiet   bool // count findings without failing t: the mutation check
	mu      sync.Mutex
	cases   int
	finds   int
	first   []string
	notLive map[string]int
	// isolated counts the refusals justified only because the refused
	// transaction violates alone with the DDL between the same statements,
	// and isoFirst keeps the first few.
	isolated int
	isoFirst []string
}

func (r *straddleReport) isolatedStraddle(msg string) {
	r.mu.Lock()
	r.isolated++
	if len(r.isoFirst) < 3 {
		r.isoFirst = append(r.isoFirst, msg)
	}
	r.mu.Unlock()
}

func (r *straddleReport) Errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.mu.Lock()
	r.finds++
	if len(r.first) < 3 {
		r.first = append(r.first, msg)
	}
	r.mu.Unlock()
	if !r.quiet {
		r.t.Error(msg)
	}
}

func (r *straddleReport) summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	shapes := make([]string, 0, len(r.notLive))
	for s := range r.notLive {
		shapes = append(shapes, s)
	}
	sort.Strings(shapes)
	var b strings.Builder
	fmt.Fprintf(&b, "%d cases, %d findings, %d refusals reproduced by the refused transaction alone with the DDL between its statements; constraint not live at the end (judged by rule 3 only):", r.cases, r.finds, r.isolated)
	for _, s := range shapes {
		fmt.Fprintf(&b, " %s=%d", s, r.notLive[s])
	}
	return b.String()
}

// straddleCaseEngine is straddleEngine for one enumeration case: the caller
// closes it when the case ends.
func straddleCaseEngine(t *testing.T, walBacked bool) (*Engine, func()) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if !walBacked {
		return NewEngine(g), func() {}
	}
	wr, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatal(err)
	}
	st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	db := store.New(wr, store.WithQuiesce(st.RunUnderCommitLock))
	return NewEngineWithStore(st), func() { _ = db.Close() }
}

// runStraddleCases runs cases on workers goroutines, each case on its own
// engine, with the R5-1 defect restored when mutate, and asserts that the run
// leaves the process's file descriptors where it found them. It returns the
// report; unless mutate, every finding has also failed t.
func runStraddleCases(t *testing.T, cases []straddleCase, workers int, mutate bool) *straddleReport {
	rep := &straddleReport{t: t, quiet: mutate, notLive: map[string]int{}}
	base := openFDs()
	defer func() {
		if base >= 0 {
			if got := openFDs(); got > base+4*workers {
				t.Errorf("%d open file descriptors after %d cases, %d before: a case's engine was not closed",
					got, len(cases), base)
			}
		}
	}()
	cache := newStraddleCache()
	next := make(chan *straddleCase)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range next {
				live := runStraddleCase(t, rep, c, cache, mutate)
				rep.mu.Lock()
				rep.cases++
				if !live {
					rep.notLive[c.shape]++
				}
				rep.mu.Unlock()
			}
		}()
	}
	for i := range cases {
		next <- &cases[i]
	}
	close(next)
	wg.Wait()
	return rep
}

func straddleShareNode(a, b straddleOp) bool {
	for _, x := range a.nodes {
		for _, y := range b.nodes {
			if x == y {
				return true
			}
		}
	}
	return false
}

// runStraddleCase runs one case, reports every oracle violation on rep, and
// returns whether the constraint was live at the end.
func runStraddleCase(t *testing.T, rep *straddleReport, c *straddleCase, cache *straddleCache, mutate bool) bool {
	ctx := context.Background()
	// Closed when the case ends, not by t.Cleanup: the enumeration runs every
	// case under one test, and a cleanup would keep every case's WAL and lock
	// open until the last case — the fd exhaustion of the first full run.
	eng, closeEng := straddleCaseEngine(t, c.walBacked)
	defer closeEng()
	if mutate {
		eng.constraintReg.SetStraddleAnyMarkForTest(true)
	}
	if err := straddleRun(eng, straddleSeed); err != nil {
		rep.Errorf("%v: seed: %v", c, err)
		return true
	}
	ddl := straddleDDLKinds[c.kind]
	if c.shape != "create" {
		if err := straddleRun(eng, ddl); err != nil {
			rep.Errorf("%v: initial constraint: %v", c, err)
			return true
		}
	}
	var tx, peer *ExplicitTx
	tS := &straddleSide{ops: c.tOps, wpos: make([]int, len(c.tOps)), ran: make([]bool, len(c.tOps)), errAt: -1}
	pS := &straddleSide{ops: []straddleOp{c.pOp}, wpos: make([]int, 1), ran: make([]bool, 1), errAt: -1}
	live := c.shape != "create"
	ddlAt := -1
	fail := func(s *straddleSide, err error, at int) {
		if s.err == nil {
			s.err, s.errAt = err, at
		}
	}
	for at, ev := range c.order {
		switch ev {
		case "Tb":
			tS.begin = at
			tx, _ = eng.BeginTx(ctx)
		case "Tw1", "Tw2":
			i := int(ev[2] - '1')
			tS.wpos[i] = at
			if tS.err != nil {
				continue // a failed statement poisons the transaction
			}
			tS.ran[i] = true
			if err := txExecErr(tx, c.tOps[i].query); err != nil {
				fail(tS, err, at)
			}
		case "Te":
			tS.end = at
			if c.rollback || tS.err != nil {
				_ = tx.Rollback()
				continue
			}
			if err := tx.Commit(); err != nil {
				fail(tS, err, at)
				_ = tx.Rollback()
			} else {
				tS.committed = true
			}
		case "Pb":
			pS.begin = at
			peer, _ = eng.BeginTx(ctx)
		case "Pw":
			pS.wpos[0] = at
			pS.ran[0] = true
			if err := txExecErr(peer, c.pOp.query); err != nil {
				fail(pS, err, at)
			}
		case "Pc":
			pS.end = at
			if pS.err != nil {
				_ = peer.Rollback()
				continue
			}
			if err := peer.Commit(); err != nil {
				fail(pS, err, at)
				_ = peer.Rollback()
			} else {
				pS.committed = true
			}
		case "D":
			ddlAt = at
			switch c.shape {
			case "create":
				live = straddleRun(eng, ddl) == nil
			case "drop":
				live = straddleRun(eng, "DROP CONSTRAINT c") != nil
			case "drop-create":
				_ = straddleRun(eng, "DROP CONSTRAINT c")
				live = straddleRun(eng, ddl) == nil
			case "drop-rewound":
				eng.dropCommitErrForTest = func() error { return errors.New("test: injected DROP commit failure") }
				_ = straddleRun(eng, "DROP CONSTRAINT c")
				eng.dropCommitErrForTest = nil
				live = eng.constraintAlreadyRegistered(straddleKind(c.kind), "L", "s")
			}
		}
	}

	// 3. Every refusal is justified.
	judge := func(who string, self, other *straddleSide) {
		switch err := self.err; {
		case err == nil:
		case straddleConflict(self, other):
			// A node-level write-write conflict justifies any refusal. Two writers
			// of one value on one node report it as a violation when the second
			// meets the first's in-flight reservation, which the value-set cannot
			// attribute to a transaction (see ConstraintRegistry.ReserveSetProperty).
		case errors.Is(err, mvcc.ErrSerializationConflict):
			rep.Errorf("%v: %s refused by a serialization conflict with no node-level write-write conflict: %v", c, who, err)
		case errors.Is(err, exec.ErrConstraintViolation):
			// The other transaction first — before the constraint when it
			// committed having made every write before a CREATE CONSTRAINT —
			// or the refused one alone. A transaction whose writes all precede
			// the creation and that commits after it has its final state
			// validated against the constraint at commit, which is the effect
			// of running it before the creation.
			otherBefore := c.shape == "create" && other.committed && other.lastWrite() < ddlAt
			if straddleSerialViolates(t, cache, c.kind, other.ops, otherBefore, self.ops) || straddleSerialViolates(t, cache, c.kind, nil, false, self.ops) {
				break
			}
			// A CREATE CONSTRAINT that fell between a transaction's own
			// statements: the serial orders keep it there for each of them.
			if c.shape == "create" && ddlAt >= 0 {
				selfScript := straddleScript(c.order, who[0])
				otherScript := straddleScript(c.order, otherWho(who)[0])
				// The other one first, when it committed or was still live at
				// the refusal: a live transaction's reservation refuses the
				// same value as if it had committed, which is the documented
				// conservative rule for an in-flight duplicate (see
				// ConstraintRegistry.ReserveSetProperty).
				if (other.committed || other.end > self.errAt) && straddleReplayViolates(t, cache, c.kind, other.ops, otherScript, self.ops, selfScript) {
					break
				}
				if straddleReplayViolates(t, cache, c.kind, nil, nil, self.ops, selfScript) {
					// The refused transaction violates ALONE, with the DDL
					// between the same statements: the refusal is the engine's
					// straddle behaviour, not an interaction with the other
					// transaction. Counted and reported, never silent.
					rep.isolatedStraddle(fmt.Sprintf("%v: %s: %v", c, who, err))
					break
				}
			}
			rep.Errorf("%v: %s refused by a constraint violation no serial order produces: %v", c, who, err)
		default:
			rep.Errorf("%v: %s refused by an unexpected error: %v", c, who, err)
		}
	}
	if straddleDebug {
		t.Logf("%v: T=%v/%v P=%v/%v live=%v", c, tS.committed, tS.err, pS.committed, pS.err, live)
	}
	judge("T", tS, pS)
	judge("P", pS, tS)

	if !live {
		return false
	}
	// 1 and 2.
	if c.kind == "notnull" {
		if n := commitStateCount(t, eng, `MATCH (n:L) WHERE n.s IS NULL RETURN count(n) AS c`); n != 0 {
			rep.Errorf("%v: %d :L nodes lack s under a live NOT NULL constraint", c, n)
		}
		return true
	}
	for _, v := range straddleValues {
		n := straddleNodes(t, eng, v)
		if n > 1 {
			rep.Errorf("%v: s = %q is held by %d :L nodes under a live UNIQUE constraint", c, v, n)
			continue
		}
		err := straddleRun(eng, fmt.Sprintf(`CREATE (:L {s: '%s'})`, v))
		switch {
		case n == 1 && !errors.Is(err, exec.ErrConstraintViolation):
			rep.Errorf("%v: s = %q is held, yet a second node with it returned %v: a missing value", c, v, err)
		case n == 0 && err != nil:
			rep.Errorf("%v: s = %q is held by no node, yet was refused (%v): a phantom value", c, v, err)
		}
	}
	return true
}

// otherWho names the other side of a case.
func otherWho(who string) string {
	if who == "T" {
		return "P"
	}
	return "T"
}

func straddleKind(kind string) exec.ConstraintKind {
	if kind == "unique" {
		return exec.ConstraintUnique
	}
	return exec.ConstraintNotNull
}

// straddleCases enumerates the single-operation family, keeping the case i
// when keep(i).
func straddleCases(walBacked bool, keep func(i int) bool) []straddleCase {
	var out []straddleCase
	i := 0
	orders := straddleOrders(1)
	for _, kind := range []string{"unique", "notnull"} {
		for _, shape := range straddleDDLShapes {
			for _, tOp := range straddleOps {
				for _, pOp := range straddleOps {
					for _, o := range orders {
						for _, rb := range []bool{false, true} {
							if keep(i) {
								out = append(out, straddleCase{kind: kind, shape: shape, tOps: []straddleOp{tOp}, pOp: pOp, order: o, rollback: rb, walBacked: walBacked})
							}
							i++
						}
					}
				}
			}
		}
	}
	return out
}

// straddleSeqCases enumerates the sequence family, keeping the case i when
// keep(i).
func straddleSeqCases(walBacked bool, keep func(i int) bool) []straddleCase {
	var out []straddleCase
	i := 0
	orders := straddleOrders(2)
	for _, shape := range straddleDDLShapes {
		for _, seq := range straddleSequences() {
			for _, pOp := range straddleOps {
				for _, o := range orders {
					for _, rb := range []bool{false, true} {
						if keep(i) {
							out = append(out, straddleCase{kind: "unique", shape: shape, tOps: seq, pOp: pOp, order: o, rollback: rb, walBacked: walBacked})
						}
						i++
					}
				}
			}
		}
	}
	return out
}

// straddleShortStride keeps one single-operation case in straddleShortStride in
// the short layer, and straddleSeqShortStride one sequence case in its stride:
// every kind, DDL shape, operation and end is still covered, over a rotating
// subset of the orders. The soak and nightly layers run every case.
const (
	straddleShortStride    = 97
	straddleSeqShortStride = 499
)

// straddleAll keeps every case.
func straddleAll(int) bool { return true }

func TestConstraintStraddle_Enumeration(t *testing.T) {
	mem := straddleCases(false, func(i int) bool { return i%straddleShortStride == 0 })
	mem = append(mem, straddleSeqCases(false, func(i int) bool { return i%straddleSeqShortStride == 0 })...)
	walCases := straddleCases(true, func(i int) bool { return i%(straddleShortStride*16) == 0 })
	walCases = append(walCases, straddleSeqCases(true, func(i int) bool { return i%(straddleSeqShortStride*16) == 0 })...)
	rep := runStraddleCases(t, append(mem, walCases...), 1, false)
	t.Logf("enumerated %d in-memory and %d WAL-backed of %d cases per wiring: %s; first reproduced-alone refusals: %q",
		len(mem), len(walCases), len(straddleCases(false, straddleAll))+len(straddleSeqCases(false, straddleAll)), rep.summary(), rep.isoFirst)
}

// TestConstraintStraddle_EnumerationDetectsSeededDefect restores the R5-1
// defect — every reservation mark counted as an insertion — and requires the
// enumeration to report it: over the sequence cases in which T releases node
// 1's value and then takes it on a new node while P moves it away, on the
// in-memory wiring.
func TestConstraintStraddle_EnumerationDetectsSeededDefect(t *testing.T) {
	var cases []straddleCase
	for _, c := range straddleSeqCases(false, straddleAll) {
		if c.shape == "create" && c.pOp.name == "move1" && c.tOps[0].name == "loseL1" && strings.HasPrefix(c.tOps[1].name, "create") {
			cases = append(cases, c)
		}
	}
	rep := runStraddleCases(t, cases, 1, true)
	t.Logf("seeded R5-1 defect: %s; first findings: %q", rep.summary(), rep.first)
	if rep.finds == 0 {
		t.Fatalf("the enumeration reported nothing over %d cases with the R5-1 defect restored: its oracle cannot fail", len(cases))
	}
}
