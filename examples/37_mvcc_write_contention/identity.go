package main

// identity.go — phase 8, GG07: node identity under concurrent CREATE, rollback
// and store reopen (rmp #3015; docs/mvcc-scenario-catalogue.md, GG07).
//
// A CREATE is given a hidden node key drawn from a counter that is
// PROCESS-WIDE and seeded once per process, from the first graph a write
// operator runs against. The keys a store holds were minted by whichever
// process wrote them. So the scenario needs fresh processes: every workload
// below runs in a child — this binary, or the test binary, re-executed — whose
// counter starts at zero, exactly as a restarted application's would.
//
// Three arms, each a gate:
//
//   - reopen: child 1 writes store R (8 concurrent sessions, half the
//     transactions rolled back, a quarter of the commits deleted, the last
//     key minted deleted); child 2 reopens R and runs the same workload.
//   - two_stores: child 1 writes store B; child 2 opens a fresh store A,
//     CREATEs one node in it (the one-shot seed reads A), then opens B and
//     runs the workload on A and B at the same time.
//   - memory: the in-memory engine. Child 2 CREATEs one node in a fresh
//     in-memory graph, then recovers B into a second in-memory graph
//     (recovery.Open, read-only) and runs the workload on both at once.
//
// Every acknowledged CREATE reports the hidden key, the id() and the
// elementId() it was given, read in the process that committed it. The parent
// reopens each persisted store and requires that every committed CREATE is a
// distinct node: no hidden key handed to two acknowledged CREATEs over the
// store's whole history (a deleted node's included), no id() or elementId()
// handed to two acknowledged CREATEs of one process, every acknowledged and
// undeleted CREATE present exactly once with its own labels and properties,
// nothing else present, and every live node's id() and elementId() distinct.
// The in-memory arm is held to the same checks inside its child, whose graphs
// die with it.
//
// id() and elementId() are compared ACROSS processes too, and gated since WAL v2
// step 3 (rmp #3021 A): every commit marker names the exact ids its transaction
// created, so a reopen reproduces them (README, phase 8).
//
// Each arm makes the collision it tests DETERMINISTIC rather than likely: a
// seeding child CREATEs four committed anchors before its concurrent sessions,
// so B's keys 1 to 4 are live, and the first CREATE a second graph receives
// after the one-shot seed draws key 2. The reopen arm's seeding child deletes
// the last key it minted, so the largest key of R is a deleted one.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// identityConfig is phase 8. A nil childCmd skips the phase.
type identityConfig struct {
	// childCmd builds the command that runs one child with the given spec: this
	// binary with -identity-child, or the test binary with the spec in the
	// environment.
	childCmd func(ctx context.Context, spec string) *exec.Cmd
}

const (
	// idSessions concurrent sessions per workload, idTxns transactions each.
	idSessions = 8
	idTxns     = 6
	// idAnchors committed CREATEs a seeding child runs before its sessions.
	idAnchors = 4
	// idChildBudget bounds one child: a child that does not finish in this time
	// is a harness failure, not a slow machine.
	idChildBudget = 60 * time.Second
	// idLedgerFile is where the parent leaves B's seeding child's events beside
	// B, for the memory child.
	idLedgerFile = "gg07-ledger.txt"
)

// idKey names one CREATE: the workload tag, the session and the transaction.
type idKey struct {
	tag  string
	s, i int64
}

// idAck is an acknowledged CREATE and the identity it was given in process proc.
type idAck struct {
	k    idKey
	proc int
	id   int64
	eid  string
	key  string
}

// idLedger is everything the children reported about one graph.
type idLedger struct {
	acks       []idAck
	deleted    map[idKey]bool
	rolledBack int
}

func newIDLedger() *idLedger { return &idLedger{deleted: make(map[idKey]bool)} }

// idGraph is one graph a child writes: its event name, its engine, and the
// graph whose Mapper resolves an id() to its hidden key.
type idGraph struct {
	name string
	eng  *cypher.Engine
	g    *lpg.Graph[string, float64]
}

// idSink serialises a child's event lines.
type idSink struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *idSink) emit(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, format+"\n", args...)
}

// idCreateQuery creates one node of workload tag. The tag is one of this file's
// constants, never input, so it is spliced into the label.
func idCreateQuery(tag string) string {
	return "CREATE (n:P:T_" + tag + " {tag: $tag, s: $s, i: $i}) RETURN id(n), elementId(n)"
}

// idCreate runs one CREATE in an explicit transaction and commits or rolls it
// back. It reports an acknowledged commit as "A", with the hidden key, id() and
// elementId() the CREATE was given, and a rollback as "B".
func idCreate(ctx context.Context, gr idGraph, sink *idSink, tag string, s, i int64, commit bool) error {
	tx, err := gr.eng.BeginTx(ctx)
	if err != nil {
		return err
	}
	rows, err := drain(tx.Exec(idCreateQuery(tag), P("tag", tag, "s", s, "i", i)))
	if err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return errors.Join(fmt.Errorf("CREATE returned %d rows", len(rows)), tx.Rollback())
	}
	id := intAt(rows, 0, 0)
	eid, _ := rows[0][1].(expr.StringValue)
	if !commit {
		if err := tx.Rollback(); err != nil {
			return err
		}
		sink.emit("B %s %s %d %d", gr.name, tag, s, i)
		return nil
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	key, ok := gr.g.AdjList().Mapper().Resolve(graph.NodeID(id)) // #nosec G115 -- an id() is a NodeID
	if !ok {
		return fmt.Errorf("id %d of %v has no key", id, idKey{tag, s, i})
	}
	sink.emit("A %s %s %d %d %d %s %s", gr.name, tag, s, i, id, string(eid), key)
	return nil
}

// idDelete DETACH DELETEs the nodes of tag that pred selects and reports each.
func idDelete(ctx context.Context, gr idGraph, sink *idSink, tag, pred string) error {
	rows, err := drain(gr.eng.RunInTx(ctx, "MATCH (n:T_"+tag+") WHERE "+pred+
		" WITH n, n.s AS s, n.i AS i DETACH DELETE n RETURN s, i", nil))
	if err != nil {
		return err
	}
	for r := range rows {
		sink.emit("D %s %s %d %d", gr.name, tag, intAt(rows, r, 0), intAt(rows, r, 1))
	}
	return nil
}

// idWorkload is idSessions concurrent sessions of idTxns CREATEs each, the odd
// ones rolled back, followed by the deletion of every committed CREATE whose
// transaction number is a multiple of four.
func idWorkload(ctx context.Context, gr idGraph, sink *idSink, tag string, start <-chan struct{}) error {
	var wg sync.WaitGroup
	errs := make([]error, idSessions)
	for s := range idSessions {
		wg.Go(func() {
			<-start
			for i := range idTxns {
				if err := idCreate(ctx, gr, sink, tag, int64(s), int64(i), i%2 == 0); err != nil {
					errs[s] = fmt.Errorf("%s session %d txn %d: %w", tag, s, i, err)
					return
				}
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return idDelete(ctx, gr, sink, tag, "n.s >= 0 AND n.i % 4 = 0")
}

// idOpen opens the persisted store in dir. Its events name it by the base name
// of dir.
func idOpen(dir string) (*store.Opened[string, float64], idGraph, error) {
	o, err := store.Open[string, float64](dir, store.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		return nil, idGraph{}, err
	}
	return o, idGraph{name: filepath.Base(dir), eng: cypher.NewEngineWithOpened(o), g: o.Graph()}, nil
}

// runIdentityChild runs one child workload. spec is "mode|dir[|dir|tag]":
//
//	seed|dir|tag   anchors, the workload, then the last key minted deleted
//	reopen|dir|tag the workload over a store an earlier child wrote
//	two|A|B        one CREATE in fresh A, then the workload on A and B at once
//	memory|B       one CREATE in a fresh in-memory graph, then the workload on it
//	               and on B recovered into memory; prints the checks itself
func runIdentityChild(ctx context.Context, spec string, w io.Writer) error {
	f := strings.Split(spec, "|")
	sink := &idSink{w: w}
	start := make(chan struct{})
	close(start)
	switch {
	case len(f) == 3 && (f[0] == "seed" || f[0] == "reopen"):
		o, gr, err := idOpen(f[1])
		if err != nil {
			return err
		}
		if f[0] == "seed" {
			err = idSeed(ctx, gr, sink, f[2], start)
		} else {
			err = idWorkload(ctx, gr, sink, f[2], start)
		}
		return errors.Join(err, o.Close())
	case len(f) == 3 && f[0] == "two":
		return idTwoStores(ctx, sink, f[1], f[2])
	case len(f) == 2 && f[0] == "memory":
		return idMemory(ctx, sink, f[1])
	}
	return fmt.Errorf("identity child: bad spec %q", spec)
}

// idSeed is the seeding child's workload.
func idSeed(ctx context.Context, gr idGraph, sink *idSink, tag string, start <-chan struct{}) error {
	for i := range idAnchors {
		if err := idCreate(ctx, gr, sink, tag, -2, int64(i), true); err != nil {
			return fmt.Errorf("anchor %d: %w", i, err)
		}
	}
	if err := idWorkload(ctx, gr, sink, tag, start); err != nil {
		return err
	}
	// The last key this process mints, committed and then deleted: the largest
	// key in the store belongs to a deleted node.
	if err := idCreate(ctx, gr, sink, tag, -1, 0, true); err != nil {
		return err
	}
	return idDelete(ctx, gr, sink, tag, "n.s = -1")
}

// idTwoStores is the two_stores child.
func idTwoStores(ctx context.Context, sink *idSink, dirA, dirB string) (err error) {
	oa, ga, err := idOpen(dirA)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, oa.Close()) }()
	if err := idCreate(ctx, ga, sink, "a1", -2, 0, true); err != nil {
		return err
	}
	ob, gb, err := idOpen(dirB)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, ob.Close()) }()
	if err := idCreate(ctx, gb, sink, "b2", -2, 0, true); err != nil {
		return err
	}
	return idPair(ctx, sink, ga, "a2", gb, "b2")
}

// idPair runs the workload on two graphs at the same time.
func idPair(ctx context.Context, sink *idSink, g1 idGraph, t1 string, g2 idGraph, t2 string) error {
	start := make(chan struct{})
	var err1, err2 error
	var wg sync.WaitGroup
	wg.Go(func() { err1 = idWorkload(ctx, g1, sink, t1, start) })
	wg.Go(func() { err2 = idWorkload(ctx, g2, sink, t2, start) })
	close(start)
	wg.Wait()
	return errors.Join(err1, err2)
}

// idMemory is the memory child. It holds both in-memory graphs to the checks
// itself and prints them as "C" lines.
func idMemory(ctx context.Context, sink *idSink, dirB string) error {
	var buf bytes.Buffer
	local := &idSink{w: &buf}
	g1 := newGraph()
	defer func() { _ = g1.Close() }()
	m1 := idGraph{name: "M1", eng: cypher.NewEngine(g1), g: g1}
	if err := idCreate(ctx, m1, local, "m1", -2, 0, true); err != nil {
		return err
	}
	res, err := recovery.Open[string, float64](dirB, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		return err
	}
	defer func() { _ = res.Graph.Close() }()
	m2 := idGraph{name: "M2", eng: cypher.NewEngine(res.Graph), g: res.Graph}
	if err := idCreate(ctx, m2, local, "m2", -2, 0, true); err != nil {
		return err
	}
	if err := idPair(ctx, local, m1, "m1", m2, "m2"); err != nil {
		return err
	}
	ledgers, err := parseIDEvents(bytes.NewReader(buf.Bytes()), 1)
	if err != nil {
		return err
	}
	// M2 started as B's recovered contents, so its ledger begins with the events
	// of B's seeding child (process 0), which the parent left beside B.
	prior, err := os.ReadFile(filepath.Join(dirB, idLedgerFile)) //nolint:gosec // G304: the parent's own ledger file
	if err != nil {
		return err
	}
	seeded, err := parseIDEvents(bytes.NewReader(prior), 0)
	if err != nil {
		return err
	}
	for _, c := range verifyIdentity(ctx, m1.eng, ledgerFor(ledgers, "M1")) {
		sink.emit("C M1 %s %v %s", c.name, c.ok, c.detail)
	}
	merged := mergeIDLedgers(ledgerFor(seeded, filepath.Base(dirB)), ledgerFor(ledgers, "M2"))
	for _, c := range verifyIdentity(ctx, m2.eng, merged) {
		sink.emit("C M2 %s %v %s", c.name, c.ok, c.detail)
	}
	return nil
}

// parseIDEvents reads the A, B and D lines of process proc into one ledger per
// graph.
func parseIDEvents(r io.Reader, proc int) (map[string]*idLedger, error) {
	out := make(map[string]*idLedger)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || (f[0] != "A" && f[0] != "B" && f[0] != "D") {
			continue
		}
		l := out[f[1]]
		if l == nil {
			l = newIDLedger()
			out[f[1]] = l
		}
		s, err1 := strconv.ParseInt(f[3], 10, 64)
		i, err2 := strconv.ParseInt(f[4], 10, 64)
		if err := errors.Join(err1, err2); err != nil {
			return nil, fmt.Errorf("event %q: %w", sc.Text(), err)
		}
		k := idKey{tag: f[2], s: s, i: i}
		switch f[0] {
		case "B":
			l.rolledBack++
		case "D":
			l.deleted[k] = true
		case "A":
			if len(f) != 8 {
				return nil, fmt.Errorf("event %q: want 8 fields", sc.Text())
			}
			id, err := strconv.ParseInt(f[5], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("event %q: %w", sc.Text(), err)
			}
			l.acks = append(l.acks, idAck{k: k, proc: proc, id: id, eid: f[6], key: f[7]})
		}
	}
	return out, sc.Err()
}

func ledgerFor(m map[string]*idLedger, name string) *idLedger {
	if l := m[name]; l != nil {
		return l
	}
	return newIDLedger()
}

func mergeIDLedgers(ls ...*idLedger) *idLedger {
	out := newIDLedger()
	for _, l := range ls {
		out.acks = append(out.acks, l.acks...)
		out.rolledBack += l.rolledBack
		for k := range l.deleted {
			out.deleted[k] = true
		}
	}
	return out
}

// idCheck is one verdict of [verifyIdentity]. A tele check is telemetry: it
// is printed, never gated.
type idCheck struct {
	name   string
	ok     bool
	tele   bool
	detail string
}

// idCheckNames are the gated checks [verifyIdentity] reports, in order.
var idCheckNames = []string{
	"exercised", "keys_distinct", "acked_ids_distinct_in_process", "ids_reused_across_processes",
	"acked_present_once", "nothing_else_present", "no_merged_identity", "ids_distinct",
	"ids_moved_across_processes",
}

// verifyIdentity holds eng's graph to l: every committed CREATE a distinct node.
func verifyIdentity(ctx context.Context, eng *cypher.Engine, l *idLedger) []idCheck {
	var out []idCheck
	add := func(name string, ok bool, format string, args ...any) {
		out = append(out, idCheck{name: name, ok: ok, detail: fmt.Sprintf(format, args...)})
	}
	add("exercised", len(l.acks) > 0 && len(l.deleted) > 0 && l.rolledBack > 0,
		"acked=%d deleted=%d rolled_back=%d", len(l.acks), len(l.deleted), l.rolledBack)

	// No hidden key handed to two acknowledged CREATEs over the store's whole
	// history, a deleted node's included: the property rmp #3015 restored.
	byKey := make(map[string]idKey, len(l.acks))
	var keyDup []string
	// No id() or elementId() handed to two acknowledged CREATEs of one process.
	type procID struct {
		proc int
		v    string
	}
	inProc := make(map[procID]idKey, 2*len(l.acks))
	var procDup []string
	// The same across processes (see the file comment).
	acrossID, acrossEID := make(map[int64]idKey, len(l.acks)), make(map[string]idKey, len(l.acks))
	var crossDup []string
	want := make(map[idKey]idAck, len(l.acks))
	for _, a := range l.acks {
		if p, dup := byKey[a.key]; dup {
			keyDup = append(keyDup, fmt.Sprintf("key %s: %v and %v", a.key, p, a.k))
		}
		byKey[a.key] = a.k
		for _, v := range []string{"id " + strconv.FormatInt(a.id, 10), "elementId " + a.eid} {
			if p, dup := inProc[procID{a.proc, v}]; dup {
				procDup = append(procDup, fmt.Sprintf("%s: %v and %v", v, p, a.k))
			}
			inProc[procID{a.proc, v}] = a.k
		}
		if p, dup := acrossID[a.id]; dup {
			crossDup = append(crossDup, fmt.Sprintf("id %d: %v and %v", a.id, p, a.k))
		}
		if p, dup := acrossEID[a.eid]; dup {
			crossDup = append(crossDup, fmt.Sprintf("elementId %s: %v and %v", a.eid, p, a.k))
		}
		acrossID[a.id], acrossEID[a.eid] = a.k, a.k
		if !l.deleted[a.k] {
			want[a.k] = a
		}
	}
	for _, s := range [][]string{keyDup, procDup, crossDup} {
		sort.Strings(s)
	}
	add("keys_distinct", len(keyDup) == 0, "%d reused: %s", len(keyDup), strings.Join(first(keyDup, 4), "; "))
	add("acked_ids_distinct_in_process", len(procDup) == 0, "%d reused: %s",
		len(procDup), strings.Join(first(procDup, 4), "; "))
	add("ids_reused_across_processes", len(crossDup) == 0, "%d %s", len(crossDup), strings.Join(first(crossDup, 4), "; "))

	rows, err := drain(eng.Run(ctx, "MATCH (n) RETURN n.tag, n.s, n.i, id(n), elementId(n), "+
		"size(labels(n)) = 2 AND 'P' IN labels(n) AND ('T_' + n.tag) IN labels(n) AND size(keys(n)) = 3", nil))
	if err != nil {
		for _, n := range idCheckNames[4:] {
			add(n, false, "read: %v", err)
		}
		return out
	}
	seen := make(map[idKey]int, len(rows))
	var missing, extra, moved, merged []string
	ids, eids := make(map[int64]bool, len(rows)), make(map[string]bool, len(rows))
	for r, row := range rows {
		tag, _ := row[0].(expr.StringValue)
		k := idKey{tag: string(tag), s: intAt(rows, r, 1), i: intAt(rows, r, 2)}
		id := intAt(rows, r, 3)
		eid, _ := row[4].(expr.StringValue)
		ids[id], eids[string(eid)] = true, true
		seen[k]++
		if a, ok := want[k]; !ok {
			extra = append(extra, fmt.Sprintf("%v id %d", k, id))
		} else if a.id != id || a.eid != string(eid) {
			moved = append(moved, fmt.Sprintf("%v acked id %d now %d", k, a.id, id))
		}
		if b, _ := row[5].(expr.BoolValue); !bool(b) {
			merged = append(merged, fmt.Sprintf("%v id %d", k, id))
		}
	}
	for k := range want {
		if seen[k] != 1 {
			missing = append(missing, fmt.Sprintf("%v x%d", k, seen[k]))
		}
	}
	for _, s := range [][]string{missing, extra, moved, merged} {
		sort.Strings(s)
	}
	add("acked_present_once", len(missing) == 0, "%d of %d not present exactly once: %s",
		len(missing), len(want), strings.Join(first(missing, 4), "; "))
	add("nothing_else_present", len(extra) == 0 && len(rows) == len(want),
		"nodes=%d want=%d unexpected=%d: %s", len(rows), len(want), len(extra), strings.Join(first(extra, 4), "; "))
	add("no_merged_identity", len(merged) == 0, "%d nodes carry another CREATE's labels or properties: %s",
		len(merged), strings.Join(first(merged, 4), "; "))
	add("ids_distinct", len(ids) == len(rows) && len(eids) == len(rows),
		"nodes=%d distinct_id=%d distinct_elementId=%d", len(rows), len(ids), len(eids))
	add("ids_moved_across_processes", len(moved) == 0, "%d %s", len(moved), strings.Join(first(moved, 4), "; "))
	return out
}

func first(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// idReport prints c through out: a gated check, or a telemetry line.
func idReport(out *ladderOut, row string, c idCheck) {
	if c.tele {
		n, sample, _ := strings.Cut(c.detail, " ")
		if sample = strings.TrimSpace(sample); sample == "" {
			out.tele(row, idSessions, c.name, n)
			return
		}
		out.tele(row, idSessions, c.name, n, c.name+"_first", strconv.Quote(sample))
		return
	}
	out.check(row, idSessions, c.name, c.ok, "%s", c.detail)
}

// phaseIdentity runs GG07 and reports every check through the returned record.
func phaseIdentity(ctx context.Context, w io.Writer, ic *identityConfig) (*ladderOut, error) {
	out := newPhaseOut(w, "identity")
	root, err := os.MkdirTemp("", "ex37-gg07-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(root) }()
	r, a, b := filepath.Join(root, "R"), filepath.Join(root, "A"), filepath.Join(root, "B")
	for _, d := range []string{r, a, b} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}

	// reopen
	evR, err := idRunChildren(ctx, ic, "seed|"+r+"|r1", "reopen|"+r+"|r2")
	if err != nil {
		return nil, err
	}
	if err := idVerifyStore(ctx, out, "reopen", r, mergeIDLedgers(ledgerFor(evR[0], "R"), ledgerFor(evR[1], "R"))); err != nil {
		return nil, err
	}

	// B, for the memory and two_stores arms; its events are left beside it for
	// the memory child.
	var seedB bytes.Buffer
	if err := idRunChild(ctx, ic, &seedB, "seed|"+b+"|b1"); err != nil {
		return nil, err
	}
	evB, err := parseIDEvents(bytes.NewReader(seedB.Bytes()), 0)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(b, idLedgerFile), seedB.Bytes(), 0o600); err != nil {
		return nil, err
	}

	// memory: B recovered read-only, before two_stores writes to it.
	var mem bytes.Buffer
	if err := idRunChild(ctx, ic, &mem, "memory|"+b); err != nil {
		return nil, err
	}
	got := make(map[string]bool)
	sc := bufio.NewScanner(&mem)
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), " ", 5)
		if len(f) < 4 || f[0] != "C" {
			continue
		}
		c := idCheck{name: f[2], ok: f[3] == "true"}
		if len(f) == 5 {
			c.detail = f[4]
		}
		c.tele = !slices.Contains(idCheckNames, c.name)
		got[f[1]+"."+c.name] = true
		idReport(out, "GG07.memory."+f[1], c)
	}
	for _, g := range []string{"M1", "M2"} {
		for _, n := range idCheckNames {
			if !got[g+"."+n] {
				out.check("GG07.memory."+g, idSessions, n, false, "the memory child never reported it")
			}
		}
	}

	// two_stores
	ev2, err := idRunChildren(ctx, ic, "two|"+a+"|"+b)
	if err != nil {
		return nil, err
	}
	if err := idVerifyStore(ctx, out, "two_stores", a, ledgerFor(ev2[0], "A")); err != nil {
		return nil, err
	}
	// B's history: its seeding child (process 0), then the two_stores child.
	two := ledgerFor(ev2[0], "B")
	for i := range two.acks {
		two.acks[i].proc = 1
	}
	if err := idVerifyStore(ctx, out, "two_stores", b, mergeIDLedgers(ledgerFor(evB, "B"), two)); err != nil {
		return nil, err
	}
	return out, nil
}

// idVerifyStore reopens the store in dir in THIS process and holds it to l.
func idVerifyStore(ctx context.Context, out *ladderOut, arm, dir string, l *idLedger) error {
	o, gr, err := idOpen(dir)
	if err != nil {
		return fmt.Errorf("GG07 %s: reopen %s: %w", arm, dir, err)
	}
	for _, c := range verifyIdentity(ctx, gr.eng, l) {
		idReport(out, "GG07."+arm+"."+gr.name, c)
	}
	return o.Close()
}

// idRunChildren runs each spec in its own child, in order, and returns the
// events of child i as process i.
func idRunChildren(ctx context.Context, ic *identityConfig, specs ...string) ([]map[string]*idLedger, error) {
	out := make([]map[string]*idLedger, 0, len(specs))
	for i, spec := range specs {
		var buf bytes.Buffer
		if err := idRunChild(ctx, ic, &buf, spec); err != nil {
			return nil, err
		}
		ev, err := parseIDEvents(bytes.NewReader(buf.Bytes()), i)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// idRunChild runs one child, its stdout into buf.
func idRunChild(ctx context.Context, ic *identityConfig, buf *bytes.Buffer, spec string) error {
	cctx, cancel := context.WithTimeout(ctx, idChildBudget)
	defer cancel()
	cmd := ic.childCmd(cctx, spec)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = buf, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("GG07 child %q: %w\nstdout:\n%s\nstderr:\n%s", spec, err, buf.String(), stderr.String())
	}
	return nil
}
