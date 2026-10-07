package main

// durability.go — phase 7: durability of MVCC commits under concurrent writers
// across a crash (rmp #2935, rows D01-D16 of docs/mvcc-scenario-catalogue.md §3).
//
// Examples 17 and 25 crash a store with ONE writer. This phase crashes a WAL-backed
// store (store.Open) while 8..64 writers commit, and holds the recovered directory
// to the durability contract:
//
//   - every ACKNOWLEDGED commit is present, whole;
//   - every refused or rolled-back write, and every transaction still open at the
//     crash, is absent;
//   - a commit IN FLIGHT at the crash (attempted, not yet acknowledged, not
//     refused) is present or absent AS A WHOLE, never partially;
//   - seek equals scan on every index, UNIQUE holds, the count store equals a scan;
//   - the commit clock is not rewound, and a new session after recovery observes
//     every acknowledged commit.
//
// # The acknowledged-commit log
//
// The log lives OUTSIDE the store: an id is appended to it only after its
// Commit returned nil. An observation (a crash image) is bracketed between two
// reads of the log's length: the count read BEFORE the image and the count read
// AFTER it. Only the ids acknowledged before the image are owed by it; an id
// acknowledged during or after the image is in doubt, because its frames may or
// may not have reached the image.
//
// # What a crash is, in-process
//
// A crash IMAGE is the directory as a crash at one instant leaves it, copied while
// the writers keep running: the WAL cut at the durable offset read after the
// "acknowledged before" count (a power loss: only fsynced bytes survive), or the
// WAL as written to the OS at the copy (a process crash: the page cache
// survives). The live process is then abandoned — its writers stopped, its store
// closed — and nothing it does after the copy can reach the image. The images
// taken inside a checkpoint are copied under the store's commit lock, so they are
// exact instants. The soak layer adds a real kill -9 of a child process
// (durability_kill.go).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/testfs"
	"github.com/FlavioCFOliveira/GoGraph/internal/waltest"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// durabilityConfig is the shape of one phase-7 run.
type durabilityConfig struct {
	// levels are the writer counts of the in-process arms. Empty skips the phase.
	levels []int
	// totalTxns is the number of logical transactions the writers of one arm
	// perform together; the crash lands when half of them are acknowledged.
	totalTxns int
	// killRuns is the number of kill -9 runs (0 skips the arm); killLevel their
	// writer count.
	killRuns  int
	killLevel int
	// childCmd builds the command that runs the kill -9 child against dir.
	childCmd func(ctx context.Context, dir string, level int) *exec.Cmd
	// dropLastAcked is the negative-control seam (D14): it cuts the durable crash
	// image of the abandon arm at the last WAL frame that carries an acknowledged
	// transaction's tag, so the "acknowledged present" gate must fail. Off by
	// default.
	dropLastAcked bool
	// seed fixes the random choices of the writers.
	seed uint64
	// syncLatency, when non-nil, delays every WAL fsync of the live stores under
	// load (store.Options.SyncLatency), so a run on a RAM drive keeps a real
	// device's commit window (rmp #3022). The tests set it through
	// internal/synclatency; the binary leaves it nil.
	syncLatency *wal.SyncLatency
	// checkpointTxns, when positive, replaces totalTxns for the checkpoint arm
	// (D09, D16). The default is the smallest total measured to still fail with
	// the fixes of rmp #2990 and #2991 reverted (README.md, "Sizes (rmp #2993)");
	// an explicit -durability-txns clears it.
	checkpointTxns int
}

func defaultDurabilityConfig() durabilityConfig {
	return durabilityConfig{levels: []int{8, 64}, totalTxns: 384, checkpointTxns: 24, killLevel: 32, seed: 1}
}

// hotKeys is the number of shared counter nodes every transaction increments: the
// contended set that produces refused attempts.
const hotKeys = 4

// largeTxns and largeBlob size D12: transactions carrying one 1 MiB string.
const (
	largeTxns = 4
	largeBlob = 1 << 20
)

// walFile is the WAL's name inside a store directory (store.Open, recovery.Open).
const walFile = "wal"

// ---------------------------------------------------------------------------
// The acknowledged-commit log.

// outcome is what the client learned about one transaction attempt.
type ackOutcome uint8

const (
	oPending    ackOutcome = iota // attempted; no outcome yet (in doubt at a crash)
	oAcked                        // Commit returned nil
	oRefused                      // typed refusal: serialization conflict or constraint
	oRolledBack                   // rolled back by the workload
	oFailed                       // any other error (an fsync failure, in the D04 arm)
	oOpen                         // the D06 holder: open at the crash, never committed
)

// attempt is one transaction attempt: its kind ('D' regular, 'L' large) and outcome.
type attempt struct {
	kind byte
	o    ackOutcome
}

// ackLog is the acknowledged-commit log, kept outside the store.
type ackLog struct {
	mu    sync.Mutex
	acks  []int64 // acknowledged ids, in acknowledgement order
	state map[int64]attempt
	// clockMax is the largest MVCC instant reported with an acknowledgement (the
	// kill -9 arm, whose child prints the clock with each ACK line).
	clockMax uint64
}

func newAckLog() *ackLog { return &ackLog{state: make(map[int64]attempt, 1024)} }

func (l *ackLog) begin(id int64, kind byte) {
	l.mu.Lock()
	l.state[id] = attempt{kind: kind, o: oPending}
	l.mu.Unlock()
}

func (l *ackLog) end(id int64, o ackOutcome) {
	l.mu.Lock()
	a := l.state[id]
	a.o = o
	l.state[id] = a
	if o == oAcked {
		l.acks = append(l.acks, id)
	}
	l.mu.Unlock()
}

func (l *ackLog) acked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.acks)
}

// snapshot returns the first n acknowledged ids and a copy of every attempt.
func (l *ackLog) snapshot(n int) ([]int64, map[int64]attempt) {
	l.mu.Lock()
	defer l.mu.Unlock()
	acks := append([]int64(nil), l.acks[:min(n, len(l.acks))]...)
	st := make(map[int64]attempt, len(l.state))
	for k, v := range l.state {
		st[k] = v
	}
	return acks, st
}

// count returns how many attempts ended with o.
func (l *ackLog) count(o ackOutcome) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, a := range l.state {
		if a.o == o {
			n++
		}
	}
	return n
}

// bracket is one observation: the acknowledged count before and after it, the
// clock read with the "before" count, and the store size the image holds.
type bracket struct {
	before, after int
	clockBefore   uint64
	walLimit      int64
	imageBytes    int64
}

// ---------------------------------------------------------------------------
// The workload.

// tagOf is the string every regular transaction stores on its :D node. It is
// written to the WAL verbatim, so the frames of one transaction can be located in
// the file (D14, D15).
func tagOf(id int64) string { return fmt.Sprintf("dtag-%08d", id) }

// blobOf is the deterministic 1 MiB value of large transaction id (D12).
func blobOf(id int64) string {
	unit := fmt.Sprintf("%08d|", id)
	return strings.Repeat(unit, largeBlob/len(unit)+1)[:largeBlob]
}

// The three statements of one regular transaction (D05: a multi-statement
// transaction in flight at the crash must be wholly present or wholly absent).
const (
	qStmtD = "CREATE (:D {id:$a, g:$g, tag:$t})"
	qStmtE = "MATCH (d:D {id:$a}) CREATE (d)-[:R {id:$a}]->(:E {id:$a})"
	qStmtH = "MATCH (h:H {k:$k}) SET h.v = h.v + 1"
	qLarge = "CREATE (:L {id:$a, blob:$b})"
)

// setupSchema declares the schema and seeds the shared counters.
func setupSchema(ctx context.Context, eng *cypher.Engine) error {
	for _, q := range []string{
		"CREATE CONSTRAINT d_id FOR (d:D) REQUIRE d.id IS UNIQUE",
		"CREATE INDEX e_id FOR (e:E) ON (e.id)",
		"CREATE INDEX l_id FOR (l:L) ON (l.id) OPTIONS {indexType: 'btree'}",
	} {
		if err := mustRun(ctx, eng, q, nil); err != nil {
			return err
		}
	}
	return mustRun(ctx, eng, "UNWIND range(0, $n - 1) AS k CREATE (:H {k:k, v:0})", P("n", hotKeys))
}

// sink receives the outcome of every attempt: the in-process ackLog, or the kill
// child's stdout.
type sink interface {
	begin(id int64, kind byte)
	end(id int64, o ackOutcome)
}

// writerSet drives the regular writers, the large-value writer (D12), the open
// holder (D06) and the DDL cycler (D11) against one engine.
type writerSet struct {
	eng    *cypher.Engine
	sink   sink
	nextID atomic.Int64
	stop   atomic.Bool
	st     txStats
	// ddlOK and ddlRefused count the DDL cycler's statements.
	ddlOK, ddlRefused atomic.Int64
	// stopOnFailure ends a writer at its first non-refusal error (the D04 arm,
	// where every commit after the poisoning fails).
	stopOnFailure bool
}

// regular runs one logical transaction with retries; each attempt takes a fresh
// id, so a refused attempt's id stays refused.
func (ws *writerSet) regular(ctx context.Context, gid int, commit bool, k int) error {
	return ws.st.retry(ctx, func() error {
		id := ws.nextID.Add(1)
		ws.sink.begin(id, 'D')
		err := inTx(ctx, ws.eng, commit, func(tx *cypher.ExplicitTx) error {
			if _, e := drain(tx.Exec(qStmtD, P("a", id, "g", gid, "t", tagOf(id)))); e != nil {
				return e
			}
			if _, e := drain(tx.Exec(qStmtE, P("a", id))); e != nil {
				return e
			}
			_, e := drain(tx.Exec(qStmtH, P("k", k)))
			return e
		})
		ws.sink.end(id, classify(err))
		return err
	})
}

// classify maps an attempt's error to its outcome.
func classify(err error) ackOutcome {
	switch {
	case err == nil:
		return oAcked
	case errors.Is(err, errRolledBack):
		return oRolledBack
	case isTypedRefusal(err):
		return oRefused
	default:
		return oFailed
	}
}

// runWriters runs n regular writers of perWriter transactions each, until done or
// stopped. One transaction in eight is rolled back by the workload.
func (ws *writerSet) runWriters(ctx context.Context, n, perWriter int, seed uint64) error {
	return fanOut(ctx, n, func(ctx context.Context, gid int) error {
		rng := newRand(seed, 0x2935+uint64(gid)) // #nosec G115 -- small id
		for range perWriter {
			if ws.stop.Load() {
				return nil
			}
			err := ws.regular(ctx, gid, rng.IntN(8) != 0, rng.IntN(hotKeys))
			if workerErr(err) != nil && !isConflict(err) {
				if ws.stopOnFailure {
					return nil
				}
				return fmt.Errorf("writer %d: %w", gid, err)
			}
		}
		return nil
	})
}

// runLarge commits largeTxns transactions of one 1 MiB value each (D12).
func (ws *writerSet) runLarge(ctx context.Context) error {
	for range largeTxns {
		if ws.stop.Load() {
			return nil
		}
		id := ws.nextID.Add(1)
		ws.sink.begin(id, 'L')
		_, err := drain(ws.eng.RunInTx(ctx, qLarge, P("a", id, "b", blobOf(id))))
		ws.sink.end(id, classify(err))
		if err != nil {
			return fmt.Errorf("large transaction %d: %w", id, err)
		}
	}
	return nil
}

// holdOpen opens one transaction, writes its first two statements, and keeps it
// open until release is closed; then it rolls back (D06). It never touches the
// shared counters, so it blocks no writer.
func (ws *writerSet) holdOpen(ctx context.Context, opened chan<- struct{}, release <-chan struct{}) error {
	id := ws.nextID.Add(1)
	ws.sink.begin(id, 'D')
	tx, err := ws.eng.BeginTx(ctx)
	if err != nil {
		close(opened)
		return err
	}
	if _, err := drain(tx.Exec(qStmtD, P("a", id, "g", -1, "t", tagOf(id)))); err != nil {
		_ = tx.Rollback()
		close(opened)
		return err
	}
	if _, err := drain(tx.Exec(qStmtE, P("a", id))); err != nil {
		_ = tx.Rollback()
		close(opened)
		return err
	}
	ws.sink.end(id, oOpen)
	close(opened)
	<-release
	if err := tx.Rollback(); err != nil {
		return err
	}
	ws.sink.end(id, oRolledBack)
	return nil
}

// cycleDDL runs CREATE/DROP INDEX and CREATE/DROP CONSTRAINT cycles until stopped
// (D11). Refusals are counted, not failed: a DDL racing a writer may be refused.
func (ws *writerSet) cycleDDL(ctx context.Context) {
	stmts := []string{
		"CREATE INDEX cyc_g FOR (d:D) ON (d.g)",
		"CREATE CONSTRAINT cyc_e FOR (e:E) REQUIRE e.id IS UNIQUE",
		"DROP INDEX cyc_g",
		"DROP CONSTRAINT cyc_e",
	}
	for i := 0; !ws.stop.Load(); i++ {
		if _, err := drain(ws.eng.RunInTx(ctx, stmts[i%len(stmts)], nil)); err != nil {
			ws.ddlRefused.Add(1)
		} else {
			ws.ddlOK.Add(1)
		}
	}
}

// ---------------------------------------------------------------------------
// Crash images.

// copyTree copies every regular file under src to dst, cutting the WAL at
// walLimit bytes when walLimit >= 0 and skipping the WAL lock file. It returns the
// bytes copied.
func copyTree(src, dst string, walLimit int64) (int64, error) {
	var total int64
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // removed by a concurrent rename: not in this instant
			}
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if !info.Mode().IsRegular() || rel == walFile+".lock" {
			return nil
		}
		limit := int64(-1)
		if walLimit >= 0 && filepath.Dir(rel) == filepath.Base(wal.SegmentDir(walFile)) {
			limit = segmentCopyLimit(path, walLimit)
		}
		n, cerr := copyFile(path, target, limit)
		total += n
		if errors.Is(cerr, os.ErrNotExist) {
			return nil
		}
		return cerr
	})
	if err != nil {
		return 0, fmt.Errorf("copy %s: %w", src, err)
	}
	return total, nil
}

// segmentCopyLimit returns how many bytes of the WAL segment at path hold frames
// below the log position walLimit: the segment header plus the frames from the
// segment's first position up to walLimit, or -1 (all of it) for a segment
// with no frame.
func segmentCopyLimit(path string, walLimit int64) int64 {
	const segHeader = 32
	b := make([]byte, segHeader+wal.HeaderSizeV2)
	f, err := os.Open(path) // #nosec G304 -- a segment of this example's own store directory
	if err != nil {
		return -1
	}
	defer func() { _ = f.Close() }()
	if _, err := io.ReadFull(f, b); err != nil {
		return -1
	}
	first := int64(binary.LittleEndian.Uint64(b[segHeader+12 : segHeader+20])) // #nosec G115 -- a log position
	if walLimit <= first {
		return segHeader
	}
	return segHeader + (walLimit - first)
}

func copyFile(src, dst string, limit int64) (int64, error) {
	in, err := os.Open(src) // #nosec G304 -- a file of this example's own store directory
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- this example's own image directory
	if err != nil {
		return 0, err
	}
	var n int64
	if limit >= 0 {
		n, err = io.CopyN(out, in, limit)
		if errors.Is(err, io.EOF) {
			err = nil
		}
	} else {
		n, err = io.Copy(out, in)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// frameInfo is one WAL frame: its log position, its size, the segment file and
// file offset it lies at, and the transaction ids whose tag it carries.
type frameInfo struct {
	off, size int64
	path      string
	fileOff   int64
	ids       []int64
}

// walFrames reads every frame of every segment of the WAL at path.
func walFrames(path string) ([]frameInfo, error) {
	locs, err := waltest.LocateFrames(path)
	if err != nil {
		return nil, err
	}
	var out []frameInfo
	marker := []byte("dtag-")
	for _, l := range locs {
		f := l.Frame
		fi := frameInfo{off: int64(f.Pos), size: l.Size, path: l.Path, fileOff: l.Offset} // #nosec G115 -- a log position
		p := f.Payload
		for {
			i := bytes.Index(p, marker)
			if i < 0 || i+len(marker)+8 > len(p) {
				break
			}
			if id, perr := strconv.ParseInt(string(p[i+len(marker):i+len(marker)+8]), 10, 64); perr == nil {
				fi.ids = append(fi.ids, id)
			}
			p = p[i+len(marker):]
		}
		out = append(out, fi)
	}
	return out, nil
}

// truncateFile cuts path to size bytes.
func truncateFile(path string, size int64) error { return os.Truncate(path, size) }

// flipByte inverts the byte at off.
func flipByte(path string, off int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- this example's own image directory
	if err != nil {
		return err
	}
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, off); err != nil {
		_ = f.Close()
		return err
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b, off); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ---------------------------------------------------------------------------
// Reading a recovered state.

// state is what a recovered store holds, keyed by transaction id.
type state struct {
	d, e, r   map[int64]int // occurrences of the :D node, the :E node, the consistent :R edge
	l         map[int64]string
	hSum      int64
	hVals     []int64
	dupsD     int64
	dupsE     int64
	fingerpr  string
	nodeCount int
}

// whole reports whether every fact of regular transaction id is present exactly once.
func (s *state) whole(id int64) bool { return s.d[id] == 1 && s.e[id] == 1 && s.r[id] == 1 }

// absent reports whether no fact of regular transaction id is present.
func (s *state) absent(id int64) bool { return s.d[id] == 0 && s.e[id] == 0 && s.r[id] == 0 }

func ival(v expr.Value) int64 {
	if i, ok := v.(expr.IntegerValue); ok {
		return int64(i)
	}
	return -1
}

func sval(v expr.Value) string {
	if s, ok := v.(expr.StringValue); ok {
		return string(s)
	}
	return ""
}

// readState reads every fact through r.
func readState(ctx context.Context, r cyRunner) (*state, error) {
	s := &state{d: map[int64]int{}, e: map[int64]int{}, r: map[int64]int{}, l: map[int64]string{}}
	h := sha256.New()
	rows, err := drain(r.Run(ctx, "MATCH (d:D) RETURN d.id, d.g, d.tag ORDER BY d.id", nil))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		s.d[ival(row[0])]++
		fmt.Fprintf(h, "D%d/%d/%s;", ival(row[0]), ival(row[1]), sval(row[2]))
	}
	rows, err = drain(r.Run(ctx, "MATCH (e:E) RETURN e.id ORDER BY e.id", nil))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		s.e[ival(row[0])]++
		fmt.Fprintf(h, "E%d;", ival(row[0]))
	}
	rows, err = drain(r.Run(ctx, "MATCH (d:D)-[x:R]->(e:E) WHERE d.id = x.id AND e.id = x.id RETURN x.id ORDER BY x.id", nil))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		s.r[ival(row[0])]++
		fmt.Fprintf(h, "R%d;", ival(row[0]))
	}
	rows, err = drain(r.Run(ctx, "MATCH (h:H) RETURN h.k, h.v ORDER BY h.k", nil))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		s.hSum += ival(row[1])
		s.hVals = append(s.hVals, ival(row[1]))
		fmt.Fprintf(h, "H%d=%d;", ival(row[0]), ival(row[1]))
	}
	rows, err = drain(r.Run(ctx, "MATCH (l:L) RETURN l.id, l.blob ORDER BY l.id", nil))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		b := sval(row[1])
		s.l[ival(row[0])] = b
		sum := sha256.Sum256([]byte(b))
		fmt.Fprintf(h, "L%d/%x;", ival(row[0]), sum[:8])
	}
	rows, err = drain(r.Run(ctx, "MATCH (n) RETURN count(n)", nil))
	if err != nil {
		return nil, err
	}
	s.nodeCount = int(intAt(rows, 0, 0))
	fmt.Fprintf(h, "N%d", s.nodeCount)
	for _, q := range []struct {
		dst *int64
		q   string
	}{
		{&s.dupsD, "MATCH (n:D) WITH n.id AS k, count(*) AS c WHERE c > 1 RETURN count(*)"},
		{&s.dupsE, "MATCH (n:E) WITH n.id AS k, count(*) AS c WHERE c > 1 RETURN count(*)"},
	} {
		rows, err = drain(r.Run(ctx, q.q, nil))
		if err != nil {
			return nil, err
		}
		*q.dst = intAt(rows, 0, 0)
	}
	s.fingerpr = hex.EncodeToString(h.Sum(nil))[:16]
	return s, nil
}

// recoverState runs recovery.Open over dir (read-only) and reads its state.
func recoverState(ctx context.Context, dir string) (*state, recovery.Result[string, float64], uint64, error) {
	res, err := recovery.OpenCtx[string, float64](ctx, dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if res.Graph == nil {
		return nil, res, 0, err
	}
	eng := cypher.NewEngine(res.Graph)
	defer func() { _ = eng.Close() }()
	clock := res.Graph.MVCCStats().Now
	s, rerr := readState(ctx, eng)
	if rerr != nil {
		return nil, res, clock, rerr
	}
	return s, res, clock, err
}

// ---------------------------------------------------------------------------
// Scoring.

// score is the durability verdict of one recovered state against the log.
type score struct {
	acked, recovered                int
	missing, phantom, partial       int
	inDoubtPresent, inDoubtAbsent   int
	openPresent, refusedPresent     int
	regularPresent                  int
	largeChecked, blobMismatch      int
	unknownPresent                  int
	firstMissing, firstPhantom      []int64
	firstPartial                    []int64
	hConserved                      bool
	ackedBeforeSet                  map[int64]bool
	recoveredSet                    map[int64]bool
	ackedBeforeIDs                  []int64
	attempts                        map[int64]attempt
	pending                         int
	ackedAfterImage, refusedOrAbort int
}

func firstFew(dst *[]int64, id int64) {
	if len(*dst) < 5 {
		*dst = append(*dst, id)
	}
}

// scoreState holds s to the log: acks are the ids acknowledged before the image,
// attempts every attempt with its final outcome.
func scoreState(s *state, acks []int64, attempts map[int64]attempt) *score {
	sc := &score{acked: len(acks), ackedBeforeSet: make(map[int64]bool, len(acks)),
		recoveredSet: map[int64]bool{}, ackedBeforeIDs: acks, attempts: attempts}
	for _, id := range acks {
		sc.ackedBeforeSet[id] = true
	}
	for id, a := range attempts {
		var whole, absent bool
		if a.kind == 'L' {
			_, present := s.l[id]
			whole, absent = present, !present
			if present {
				sc.largeChecked++
				if s.l[id] != blobOf(id) {
					sc.blobMismatch++
				}
			}
		} else {
			whole, absent = s.whole(id), s.absent(id)
			if whole && a.o != oOpen {
				sc.regularPresent++
			}
		}
		if whole {
			sc.recoveredSet[id] = true
		}
		switch {
		case !whole && !absent:
			sc.partial++
			firstFew(&sc.firstPartial, id)
		case sc.ackedBeforeSet[id]:
			if absent {
				sc.missing++
				firstFew(&sc.firstMissing, id)
			}
		case a.o == oRefused || a.o == oRolledBack || a.o == oFailed:
			sc.refusedOrAbort++
			if whole {
				sc.phantom++
				sc.refusedPresent++
				firstFew(&sc.firstPhantom, id)
			}
		case a.o == oOpen:
			if whole {
				sc.phantom++
				sc.openPresent++
				firstFew(&sc.firstPhantom, id)
			}
		default: // pending at the end, or acknowledged after the "before" count
			if a.o == oPending {
				sc.pending++
			} else {
				sc.ackedAfterImage++
			}
			if whole {
				sc.inDoubtPresent++
			} else {
				sc.inDoubtAbsent++
			}
		}
	}
	// A present fact of an id nobody attempted is a phantom too.
	for _, m := range []map[int64]int{s.d, s.e, s.r} {
		for id := range m {
			if _, ok := attempts[id]; !ok {
				sc.unknownPresent++
				sc.phantom++
				firstFew(&sc.firstPhantom, id)
			}
		}
	}
	for id := range s.l {
		if _, ok := attempts[id]; !ok {
			sc.unknownPresent++
			sc.phantom++
		}
	}
	sc.recovered = len(sc.recoveredSet)
	// Every whole regular transaction incremented exactly one counter by one.
	sc.hConserved = s.hSum == int64(sc.regularPresent)
	return sc
}

// holes counts acknowledged-before transactions absent while a transaction whose
// tag sits LATER in the image's WAL was recovered (D15).
func holes(frames []frameInfo, sc *score) (holes, tagged int) {
	pos := map[int64]int64{}
	for _, f := range frames {
		for _, id := range f.ids {
			pos[id] = f.off
		}
	}
	type ip struct {
		id  int64
		off int64
	}
	order := make([]ip, 0, len(pos))
	for id, off := range pos {
		order = append(order, ip{id, off})
	}
	sort.Slice(order, func(i, j int) bool { return order[i].off < order[j].off })
	lastRecovered := int64(-1)
	for _, x := range order {
		if sc.recoveredSet[x.id] && x.off > lastRecovered {
			lastRecovered = x.off
		}
	}
	for _, x := range order {
		if sc.ackedBeforeSet[x.id] && !sc.recoveredSet[x.id] && x.off < lastRecovered {
			holes++
		}
	}
	return holes, len(order)
}

// ---------------------------------------------------------------------------
// Verification of one image.

// verifyOpts selects the checks of one image.
type verifyOpts struct {
	// fullOpen runs the post-recovery session checks (store.Open, a new session,
	// seek = scan, a new commit). Off for images recovery is expected to refuse.
	fullOpen bool
	// holes runs the D15 position check.
	holes bool
	// damaged marks an image whose WAL was damaged on purpose (D08): an
	// acknowledged commit in the damaged frame may be lost, so the acknowledged and
	// clock gates are replaced by the caller's differential check.
	damaged bool
}

// verified is the outcome of one image.
type verified struct {
	st    *state
	sc    *score
	clean bool
}

// verifyImage recovers img, scores it against the log, and reports every check
// under row.
func verifyImage(ctx context.Context, out *ladderOut, row string, level int, img string,
	br bracket, log *ackLog, vo verifyOpts,
) (*verified, error) {
	acks, attempts := log.snapshot(br.before)
	walPath := filepath.Join(img, walFile)
	var frames []frameInfo
	if vo.holes {
		f, ferr := walFrames(walPath)
		if ferr != nil {
			return nil, fmt.Errorf("%s: read wal frames: %w", row, ferr)
		}
		frames = f
	}
	t0 := time.Now()
	s, res, clockAfter, err := recoverState(ctx, img)
	recElapsed := time.Since(t0)
	if s == nil {
		return nil, fmt.Errorf("%s: recovery: %w", row, err)
	}
	clean := err == nil && res.IsClean()
	sc := scoreState(s, acks, attempts)
	out.tele(row, level, "acknowledged", br.before, "acknowledged_after", br.after,
		"recovered", sc.recovered, "missing", sc.missing, "phantom", sc.phantom, "partial", sc.partial,
		"in_doubt_present", sc.inDoubtPresent, "in_doubt_absent", sc.inDoubtAbsent,
		"refused_or_rolled_back", sc.refusedOrAbort, "large_checked", sc.largeChecked,
		"first_missing", fmt.Sprint(sc.firstMissing), "first_phantom", fmt.Sprint(sc.firstPhantom),
		"first_partial", fmt.Sprint(sc.firstPartial),
		"recovery_clean", clean, "tail_err", fmt.Sprintf("%q", errText(res.TailErr)),
		"wal_ops", res.WALOps, "snapshot_hit", res.SnapshotHit, "recovery_elapsed", recElapsed.Round(time.Microsecond),
		"clock_before", br.clockBefore, "clock_after", clockAfter, "wal_limit", br.walLimit,
		"store_bytes_at_crash", br.imageBytes)
	out.check(row, level, "acknowledged_seen", br.before > 0, "the crash landed before any acknowledgement")
	if !vo.damaged {
		out.check(row, level, "acked_present", sc.missing == 0, "%d acknowledged commits missing, first %v", sc.missing, sc.firstMissing)
		out.check(row, level, "clock_not_rewound", clockAfter >= br.clockBefore, "clock %d after recovery, %d before the crash", clockAfter, br.clockBefore)
	}
	out.check(row, level, "refused_absent", sc.phantom == 0, "%d refused, rolled-back, open or unknown transactions present, first %v",
		sc.phantom, sc.firstPhantom)
	out.check(row, level, "whole_or_absent", sc.partial == 0, "%d transactions partially present, first %v", sc.partial, sc.firstPartial)
	out.check(row, level, "counters_conserved", sc.hConserved, "sum of counters %d, whole transactions %d", s.hSum, sc.regularPresent)
	out.check(row, level, "open_absent", sc.openPresent == 0, "%d transactions open at the crash present", sc.openPresent)
	if sc.largeChecked > 0 {
		out.check(row, level, "blobs_identical", sc.blobMismatch == 0, "%d of %d large values differ", sc.blobMismatch, sc.largeChecked)
	}
	if vo.holes {
		h, tagged := holes(frames, sc)
		out.tele(row, level, "wal_frames", len(frames), "wal_tagged_transactions", tagged, "holes", h)
		out.check(row, level, "wal_tags_seen", tagged > 0, "no transaction tag found in the image's WAL: the position check would be vacuous")
		out.check(row, level, "no_hole", h == 0, "%d acknowledged transactions absent before a recovered one", h)
	}
	v := &verified{st: s, sc: sc, clean: clean}
	if !vo.fullOpen {
		return v, nil
	}
	return v, verifySession(ctx, out, row, level, img, br, s, sc, acks)
}

func errText(err error) string {
	if err == nil {
		return "none"
	}
	return err.Error()
}

// The seek/scan pairs held after recovery (D10, D11).
const (
	qSeekD     = "MATCH (n:D {id:$v}) WITH count(n) AS seek OPTIONAL MATCH (m:D) WHERE m.id + 0 = $v RETURN seek, count(m) AS scan"
	qSeekE     = "MATCH (n:E {id:$v}) WITH count(n) AS seek OPTIONAL MATCH (m:E) WHERE m.id + 0 = $v RETURN seek, count(m) AS scan"
	qSeekL     = "MATCH (n:L) WHERE n.id >= $v WITH count(n) AS seek OPTIONAL MATCH (m:L) WHERE m.id + 0 >= $v RETURN seek, count(m) AS scan"
	qSeekG     = "MATCH (n:D {g:$v}) WITH count(n) AS seek OPTIONAL MATCH (m:D) WHERE m.g + 0 = $v RETURN seek, count(m) AS scan"
	qCountD    = "MATCH (n:D) WITH count(n) AS seek OPTIONAL MATCH (m) WHERE 'D' IN labels(m) RETURN seek, count(m) AS scan"
	qCountE    = "MATCH (n:E) WITH count(n) AS seek OPTIONAL MATCH (m) WHERE 'E' IN labels(m) RETURN seek, count(m) AS scan"
	qPostWrite = "CREATE (:Post {id:$v})"
)

// verifySession opens the image for writing (store.Open), registers the recovered
// schema, and checks what a new session observes (D07, D10, D11).
func verifySession(ctx context.Context, out *ladderOut, row string, level int, img string,
	br bracket, s *state, sc *score, acks []int64,
) error {
	o, err := store.Open[string, float64](img, store.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		out.check(row, level, "reopens_for_writing", false, "store.Open: %v", err)
		return nil
	}
	eng := cypher.NewEngineWithOpened(o)
	closeAll := func() {
		_ = o.Close()
		_ = eng.Close()
	}
	sess := eng.NewSession()
	s2, err := readState(ctx, sess)
	if err != nil {
		closeAll()
		return fmt.Errorf("%s: new session read: %w", row, err)
	}
	unseen := 0
	for _, id := range acks {
		a := sc.attempts[id]
		present := s2.whole(id)
		if a.kind == 'L' {
			_, present = s2.l[id]
		}
		if !present {
			unseen++
		}
	}
	out.check(row, level, "new_session_sees_acked", unseen == 0 && s2.fingerpr == s.fingerpr,
		"%d acknowledged commits not observed by a new session; fingerprint %s after open, %s from recovery", unseen, s2.fingerpr, s.fingerpr)

	// Seek = scan on every index, the count store, UNIQUE.
	var vals []int64
	for id := range sc.attempts {
		vals = append(vals, id)
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	var mismatches []string
	compared := 0
	pair := func(name, q string, v int64) error {
		seek, scan, perr := seekScanPair(ctx, sess, q, P("v", v))
		if perr != nil {
			return fmt.Errorf("%s: %s: %w", row, name, perr)
		}
		compared++
		if seek != scan {
			mismatches = append(mismatches, fmt.Sprintf("%s %d seek=%d scan=%d", name, v, seek, scan))
		}
		return nil
	}
	for _, v := range vals {
		for _, c := range []struct{ name, q string }{{"d_id", qSeekD}, {"e_id", qSeekE}} {
			if err := pair(c.name, c.q, v); err != nil {
				closeAll()
				return err
			}
		}
	}
	for g := int64(-1); g < int64(level); g++ {
		if err := pair("cyc_g", qSeekG, g); err != nil {
			closeAll()
			return err
		}
	}
	for _, c := range []struct{ name, q string }{{"l_id", qSeekL}, {"count_d", qCountD}, {"count_e", qCountE}} {
		if err := pair(c.name, c.q, 0); err != nil {
			closeAll()
			return err
		}
	}
	idx := []string{}
	for _, ix := range o.Recovery().Indexes {
		idx = append(idx, ix.Name)
	}
	cons := []string{}
	for _, c := range o.Recovery().Constraints {
		cons = append(cons, c.Name)
	}
	first := "none"
	if len(mismatches) > 0 {
		first = strings.Join(mismatches[:min(5, len(mismatches))], "; ")
	}
	out.tele(row, level, "seek_scan_compared", compared, "seek_scan_mismatches", len(mismatches),
		"seek_scan_first_mismatches", fmt.Sprintf("%q", first),
		"recovered_indexes", strings.Join(idx, ","), "recovered_constraints", strings.Join(cons, ","))
	out.check(row, level, "seek_equals_scan", len(mismatches) == 0, "%d mismatches: %s", len(mismatches), first)
	out.check(row, level, "unique_holds", s2.dupsD == 0 && (s2.dupsE == 0 || !contains(cons, "cyc_e")),
		"%d duplicated :D ids, %d duplicated :E ids (cyc_e present: %v)", s2.dupsD, s2.dupsE, contains(cons, "cyc_e"))

	// A new commit takes an instant above every acknowledged one.
	if _, err := drain(eng.RunInTx(ctx, qPostWrite, P("v", 1))); err != nil {
		closeAll()
		return fmt.Errorf("%s: post-recovery write: %w", row, err)
	}
	post := o.Graph().MVCCStats().Now
	closeAll()
	size, err := dirSize(img)
	if err != nil {
		return err
	}
	out.tele(row, level, "clock_after_write", post, "store_bytes_after_recovery", size)
	out.check(row, level, "post_recovery_commit_is_new", post > br.clockBefore,
		"post-recovery commit at %d, clock before the crash %d", post, br.clockBefore)
	return nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The driver.

// phaseDurability runs every arm and returns the output record. The returned
// error is a harness failure; a failed check is recorded in out.
func phaseDurability(ctx context.Context, w io.Writer, dc *durabilityConfig) (*ladderOut, error) {
	out := newPhaseOut(w, "durability")
	fmt.Fprintf(w, "## phase 7 — durability under concurrent writers across a crash (levels=%v kill_runs=%d seam=%v)\n",
		dc.levels, dc.killRuns, dc.dropLastAcked)
	for _, level := range dc.levels {
		for _, arm := range []func(context.Context, *durabilityConfig, *ladderOut, int) error{
			armAbandon, armCheckpoint, armFsync,
		} {
			if err := arm(ctx, dc, out, level); err != nil {
				return out, err
			}
		}
	}
	for run := range dc.killRuns {
		if err := armKill(ctx, dc, out, run); err != nil {
			return out, err
		}
	}
	return out, nil
}

// liveStore is one WAL-backed store under load.
type liveStore struct {
	dir string
	o   *store.Opened[string, float64]
	eng *cypher.Engine
}

func openLive(ctx context.Context, row string, level int, lat *wal.SyncLatency) (*liveStore, error) {
	dir, err := storeDirFor(row, level, "live")
	if err != nil {
		return nil, err
	}
	o, err := store.Open[string, float64](dir, store.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
		SyncLatency: lat,
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	ls := &liveStore{dir: dir, o: o, eng: cypher.NewEngineWithOpened(o)}
	if err := setupSchema(ctx, ls.eng); err != nil {
		ls.close()
		return nil, err
	}
	return ls, nil
}

func (ls *liveStore) close() {
	_ = ls.o.Close()
	_ = ls.eng.Close()
	_ = os.RemoveAll(ls.dir)
}

// waitAcked waits until log holds n acknowledgements or done is closed.
func waitAcked(log *ackLog, n int, done <-chan struct{}) bool {
	for log.acked() < n {
		select {
		case <-done:
			return false
		case <-time.After(200 * time.Microsecond):
		}
	}
	return true
}

func perWriter(dc *durabilityConfig, level int) int { return max(minOpsPerWorker, dc.totalTxns/level) }

// armAbandon is D01, D03, D05, D06, D07, D08, D10, D11, D12, D13, D14, D15: an
// in-process crash image taken while the writers, the large-value writer, the
// open holder and the DDL cycler run.
func armAbandon(ctx context.Context, dc *durabilityConfig, out *ladderOut, level int) error {
	ls, err := openLive(ctx, "D01", level, dc.syncLatency)
	if err != nil {
		return err
	}
	liveClosed := false
	defer func() {
		if !liveClosed {
			ls.close()
		}
	}()
	sizeBefore, err := dirSize(ls.dir)
	if err != nil {
		return err
	}
	log := newAckLog()
	ws := &writerSet{eng: ls.eng, sink: log}
	release := make(chan struct{})
	opened := make(chan struct{})
	var bg sync.WaitGroup
	var bgErr atomic.Pointer[error]
	keep := func(err error) {
		if err != nil {
			bgErr.CompareAndSwap(nil, &err)
		}
	}
	bg.Add(3)
	go func() { defer bg.Done(); keep(ws.holdOpen(ctx, opened, release)) }()
	go func() { defer bg.Done(); keep(ws.runLarge(ctx)) }()
	go func() { defer bg.Done(); ws.cycleDDL(ctx) }()
	<-opened
	writersDone := make(chan struct{})
	var wErr error
	go func() {
		wErr = ws.runWriters(ctx, level, perWriter(dc, level), dc.seed)
		close(writersDone)
	}()

	// The crash: half of the transactions acknowledged, the writers still running.
	running := waitAcked(log, dc.totalTxns/2, writersDone)
	base := filepath.Join(filepath.Dir(ls.dir), filepath.Base(ls.dir)+"-img")
	durable, written := base+"-durable", base+"-written"
	defer func() {
		for _, d := range []string{durable, written, base + "-ref", base + "-torn", base + "-garbled", base + "-double"} {
			_ = os.RemoveAll(d)
		}
	}()
	var br bracket
	br.before = log.acked()
	br.clockBefore = ls.o.Graph().MVCCStats().Now
	br.walLimit = ls.o.WAL().DurableOffset()
	if br.imageBytes, err = copyTree(ls.dir, durable, br.walLimit); err != nil {
		return err
	}
	brW := br
	brW.walLimit = -1
	if brW.imageBytes, err = copyTree(ls.dir, written, -1); err != nil {
		return err
	}
	br.after = log.acked()
	brW.after = br.after

	// Abandon the live process: stop everything, then close it. The images are
	// already taken, so nothing below reaches them.
	ws.stop.Store(true)
	close(release)
	<-writersDone
	bg.Wait()
	ls.close()
	liveClosed = true
	if wErr != nil {
		return wErr
	}
	if p := bgErr.Load(); p != nil {
		return *p
	}
	reportTx(out, "D01", level, "abandon", &ws.st, 0)
	out.tele("D01", level, "writers_running_at_crash", running, "store_bytes_before", sizeBefore,
		"ddl_ok", ws.ddlOK.Load(), "ddl_refused", ws.ddlRefused.Load(),
		"refused_attempts_logged", log.count(oRefused), "rolled_back_logged", log.count(oRolledBack))
	out.check("D01", level, "writers_running_at_crash", running, "every writer finished before the crash")
	out.check("D01", level, "no_unexpected_errors", ws.st.otherErrs.Load() == 0 && log.count(oFailed) == 0,
		"first: %s", ws.st.errText())
	out.check("D11", level, "ddl_ran", ws.ddlOK.Load() > 0, "the DDL cycler committed nothing")

	// D14: the negative-control seam, off by default.
	if dc.dropLastAcked {
		cut, cerr := seamCut(filepath.Join(durable, walFile), log, br.before)
		if cerr != nil {
			return cerr
		}
		out.tele("D14", level, "seam_cut_at", cut)
	}
	// The untouched reference for D08 and D13: the session checks below write a
	// commit into the images they open.
	if _, err := copyTree(durable, base+"-ref", -1); err != nil {
		return err
	}

	// D01, D03, D05, D06, D07, D10, D11, D12, D15: the power-loss image.
	vd, err := verifyImage(ctx, out, "D01.durable", level, durable, br, log, verifyOpts{fullOpen: true, holes: true})
	if err != nil {
		return err
	}
	// The process-crash image (bytes written to the OS survive).
	if _, err := verifyImage(ctx, out, "D01.written", level, written, brW, log, verifyOpts{fullOpen: true, holes: true}); err != nil {
		return err
	}
	// D03: the crash-point classes the durable image distinguishes.
	out.tele("D03", level, "durable_but_unacknowledged", vd.sc.inDoubtPresent,
		"appended_not_durable_or_in_flight", vd.sc.inDoubtAbsent)

	// D08 and D13, from the untouched reference copy of the durable image.
	return tornArms(ctx, out, level, base, br, log)
}

// tornArms derives the torn (D08), garbled (D08) and double-crash (D13) images
// from a fresh copy of the power-loss image.
func tornArms(ctx context.Context, out *ladderOut, level int, base string, br bracket, log *ackLog) error {
	ref := base + "-ref"
	vr, err := verifyImage(ctx, out, "D08.reference", level, ref, br, log, verifyOpts{})
	if err != nil {
		return err
	}
	frames, err := walFrames(filepath.Join(ref, walFile))
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		out.check("D08", level, "wal_has_frames", false, "the reference image has no frame")
		return nil
	}
	last := frames[len(frames)-1]
	end := last.off + last.size
	for _, v := range []struct {
		row string
		mut func(path string) error
	}{
		{"D08.torn", func(p string) error { return truncateFile(p, last.fileOff+last.size-3) }},
		{"D08.garbled", func(p string) error { return flipByte(p, last.fileOff+last.size-1) }},
	} {
		img := base + "-" + strings.TrimPrefix(v.row, "D08.")
		if _, err := copyTree(ref, img, end); err != nil {
			return err
		}
		rel, rerr := filepath.Rel(ref, last.path)
		if rerr != nil {
			return rerr
		}
		if err := v.mut(filepath.Join(img, rel)); err != nil {
			return err
		}
		vt, err := verifyImage(ctx, out, v.row, level, img, br, log, verifyOpts{damaged: true})
		if err != nil {
			return err
		}
		// Differential: the damaged frame belongs to at most one transaction, so the
		// recovered set loses at most that one and gains nothing.
		lost, gained := 0, 0
		for id := range vr.sc.recoveredSet {
			if !vt.sc.recoveredSet[id] {
				lost++
			}
		}
		for id := range vt.sc.recoveredSet {
			if !vr.sc.recoveredSet[id] {
				gained++
			}
		}
		out.tele(v.row, level, "lost_vs_reference", lost, "gained_vs_reference", gained, "last_frame_bytes", last.size)
		out.check(v.row, level, "damaged_record_discarded_alone", lost <= 1 && gained == 0 && vt.sc.partial == 0,
			"lost %d, gained %d, partial %d against the undamaged image", lost, gained, vt.sc.partial)
		if v.row == "D08.garbled" {
			// A garbled durable frame must never be accepted as data: recovery either
			// stops at it as a torn tail, or refuses the directory loudly.
			opened := true
			o, oerr := store.Open[string, float64](img, store.Options[string, float64]{
				Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
			})
			if oerr != nil {
				opened = false
			} else {
				_ = o.Close()
			}
			out.tele(v.row, level, "reopened_for_writing", opened)
			out.check(v.row, level, "refused_unless_clean", opened == vt.clean,
				"store.Open opened=%v while recovery clean=%v", opened, vt.clean)
		}
	}
	// D13: a crash during recovery. Recovery is interrupted (its context cancelled
	// mid-replay), then the store is opened for writing (which repairs the torn
	// tail) and closed, then recovered again: the state equals one recovery of the
	// torn image.
	torn := base + "-torn"
	double := base + "-double"
	if _, err := copyTree(torn, double, -1); err != nil {
		return err
	}
	tornState, _, _, err := recoverState(ctx, torn)
	if err != nil || tornState == nil {
		return fmt.Errorf("D13: recover torn image: %w", err)
	}
	// Recovery is read-only until the store opens for writing, so an interrupted
	// recovery is abandoned at whatever point it observes its context. The
	// interruption is STRUCTURAL, not a time window (rmp #3000): the context
	// reports cancellation from its second Err() call on. Recovery checks its
	// context once at entry (passes) and then when it replays the first WAL frame
	// (recovery.OpenCtx: every 4096 frames, starting at the first), so the
	// interruption lands inside the replay, after the snapshot probe and the WAL
	// open, on any machine at any load. A delay-based cancel missed every replay
	// in a saturated -race run, where recovery finished before the timer fired.
	ictx := newErrCountdown(ctx, 1)
	ires, ierr := recovery.OpenCtx[string, float64](ictx, double, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	ictx.stop()
	if ires.Graph != nil {
		_ = ires.Graph.Close()
	}
	interruptedInReplay := errors.Is(ierr, context.Canceled) && ictx.calls.Load() > 1
	o, err := store.Open[string, float64](double, store.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		out.check("D13", level, "reopens_after_interrupted_recovery", false, "store.Open: %v", err)
		return nil
	}
	_ = o.Close()
	again, _, _, err := recoverState(ctx, double)
	if err != nil || again == nil {
		out.check("D13", level, "recovers_after_double_crash", false, "recovery: %v", err)
		return nil
	}
	out.tele("D13", level, "interrupted_in_replay", interruptedInReplay, "context_checks", ictx.calls.Load(),
		"interrupted_wal_ops", ires.WALOps, "fingerprint_single", tornState.fingerpr, "fingerprint_double", again.fingerpr)
	out.check("D13", level, "recovery_interrupted", interruptedInReplay,
		"recovery was not interrupted inside the WAL replay (error %v after %d context checks): the double crash was not exercised",
		ierr, ictx.calls.Load())
	out.check("D13", level, "double_recovery_identical", again.fingerpr == tornState.fingerpr,
		"double-crash state %s, single recovery %s", again.fingerpr, tornState.fingerpr)
	return nil
}

// errCountdown is a context whose Err reports cancellation from its
// (allow+1)-th call on: it cancels itself on that call, so Done, Err and the
// parent's cancellation stay consistent. D13 uses it to interrupt recovery at a
// chosen context check rather than after a delay.
type errCountdown struct {
	context.Context //nolint:containedctx // errCountdown IS a context: it wraps its parent to count Err calls
	cancel          context.CancelFunc
	left            atomic.Int64
	calls           atomic.Int64
}

func newErrCountdown(parent context.Context, allow int64) *errCountdown {
	ctx, cancel := context.WithCancel(parent)
	c := &errCountdown{Context: ctx, cancel: cancel}
	c.left.Store(allow)
	return c
}

// Err counts the call and cancels the context once the allowance is spent.
func (c *errCountdown) Err() error {
	c.calls.Add(1)
	if c.left.Add(-1) < 0 {
		c.cancel()
	}
	return c.Context.Err()
}

// stop releases the context's resources.
func (c *errCountdown) stop() { c.cancel() }

// seamCut is the D14 negative control: it cuts the WAL at path at the start of
// the last frame carrying the tag of a transaction acknowledged before the
// image, which drops that acknowledged commit.
func seamCut(path string, log *ackLog, before int) (int64, error) {
	acks, _ := log.snapshot(before)
	acked := make(map[int64]bool, len(acks))
	for _, id := range acks {
		acked[id] = true
	}
	frames, err := walFrames(path)
	if err != nil {
		return 0, err
	}
	cut := -1
	for i, f := range frames {
		for _, id := range f.ids {
			if acked[id] {
				cut = i
			}
		}
	}
	if cut < 0 {
		return 0, errors.New("D14 seam: no acknowledged transaction tag in the WAL")
	}
	return frames[cut].off, truncateFile(frames[cut].path, frames[cut].fileOff)
}

// armCheckpoint is D09, D16 and D10 at checkpoint boundaries: the first checkpoint
// of a run under load is imaged at two instants, each copied under the store's
// commit lock (writers drained and blocked): before the capture, and after the
// checkpoint has returned.
func armCheckpoint(ctx context.Context, dc *durabilityConfig, out *ladderOut, level int) error {
	if dc.checkpointTxns > 0 {
		sized := *dc
		sized.totalTxns = dc.checkpointTxns
		dc = &sized
	}
	ls, err := openLive(ctx, "D09", level, dc.syncLatency)
	if err != nil {
		return err
	}
	defer ls.close()
	log := newAckLog()
	ws := &writerSet{eng: ls.eng, sink: log}
	base := filepath.Join(filepath.Dir(ls.dir), filepath.Base(ls.dir)+"-img")
	phases := []string{"pre_capture", "post_checkpoint"}
	imgs := map[string]string{}
	brs := map[string]bracket{}
	defer func() {
		for _, p := range phases {
			_ = os.RemoveAll(base + "-" + p)
			_ = os.RemoveAll(base + "-" + p + "-nosnap")
		}
	}()
	var calls atomic.Int32
	var imgErr error
	image := func(phase string) {
		if imgErr != nil {
			return
		}
		var br bracket
		br.before = log.acked()
		br.clockBefore = ls.o.Graph().MVCCStats().Now
		br.walLimit = ls.o.WAL().DurableOffset()
		dst := base + "-" + phase
		if imgErr = os.RemoveAll(dst); imgErr != nil {
			return
		}
		br.imageBytes, imgErr = copyTree(ls.dir, dst, br.walLimit)
		br.after = log.acked()
		imgs[phase], brs[phase] = dst, br
	}
	// A checkpoint calls the serialiser once, for the capture: its phase 3
	// unlinks whole WAL segments without the commit lock (docs/design-wal-v2.md
	// §2.4). The post-checkpoint image is taken under the lock after it returns.
	serialise := func(fn func() error) error {
		return ls.o.Store().RunUnderCommitLock(func() error {
			calls.Add(1)
			image("pre_capture")
			return fn()
		})
	}
	var unused sync.Mutex
	cp := checkpoint.New(checkpoint.Config{Dir: ls.dir}, ls.o.Graph(), ls.o.WAL(), &unused,
		checkpoint.WithCommitSerialiser[string, float64](serialise),
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
		checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()),
		checkpoint.WithConstraintSpecs[string, float64](ls.eng.ConstraintSpecsForSnapshot),
		checkpoint.WithIndexSpecs[string, float64](ls.eng.IndexSpecsForSnapshot))
	writersDone := make(chan struct{})
	var wErr error
	go func() {
		wErr = ws.runWriters(ctx, level, perWriter(dc, level), dc.seed+1)
		close(writersDone)
	}()
	waitAcked(log, dc.totalTxns/3, writersDone)
	// ONE attempt, no retry (rmp #2991): a capture refusal under explicit-transaction
	// load was a defect, not a mode, and checkpoint_ran gates the first attempt. The
	// refusal itself (snapshot.ErrCaptureNotQuiesced) was retired by WAL v2 step 1.
	var running bool
	select {
	case <-writersDone:
		running = false
	default:
		running = true
	}
	cpErr := cp.RunCheckpoint()
	if cpErr == nil {
		_ = ls.o.Store().RunUnderCommitLock(func() error {
			image("post_checkpoint")
			return nil
		})
	}
	<-writersDone
	if wErr != nil {
		return wErr
	}
	if imgErr != nil {
		return imgErr
	}
	reportTx(out, "D09", level, "checkpoint", &ws.st, 0)
	out.tele("D09", level, "writers_running_at_checkpoint", running, "checkpoint_error", fmt.Sprintf("%q", errText(cpErr)),
		"serialiser_calls", calls.Load(), "wal_truncated_bytes", cp.Stats().WALTruncBytes)
	out.check("D09", level, "checkpoint_ran", cpErr == nil && len(imgs) == len(phases),
		"checkpoint error %v, %d of %d phase images taken", cpErr, len(imgs), len(phases))
	if len(imgs) != len(phases) {
		return nil
	}
	for _, p := range phases {
		_, err := verifyImage(ctx, out, "D09."+p, level, imgs[p], brs[p], log, verifyOpts{fullOpen: true})
		if err != nil {
			return err
		}
	}
	// D16: snapshot plus tail replay equals a full replay of the same WAL. The
	// post-checkpoint image holds the published snapshot and, while the log fits
	// in its first segment, every frame (a checkpoint unlinks whole segments
	// only). The full-replay reference is that image without the snapshot and
	// with the WAL control file it had before the checkpoint, from the
	// pre-capture image: the directory as it stood before any checkpoint.
	post := base + "-post_checkpoint"
	pre := post + "-fullreplay"
	defer func() { _ = os.RemoveAll(pre) }()
	if err := cloneWithoutSnapshot(post, pre, brs["post_checkpoint"].walLimit); err != nil {
		return err
	}
	preCtl, err := os.ReadFile(wal.ControlPath(filepath.Join(base+"-pre_capture", walFile))) // #nosec G304 -- this example's own image directory
	if err != nil {
		return fmt.Errorf("D16: read pre-checkpoint control file: %w", err)
	}
	if err := os.WriteFile(wal.ControlPath(filepath.Join(pre, walFile)), preCtl, 0o600); err != nil { // #nosec G703 -- this example's own image directory
		return fmt.Errorf("D16: restore pre-checkpoint control file: %w", err)
	}
	firstSeg := wal.SegmentPath(filepath.Join(pre, walFile), 1)
	if _, err := os.Stat(firstSeg); err != nil {
		out.check("D16", level, "full_wal_retained", false, "the checkpoint unlinked the first segment, so no full replay is possible: %v", err)
		return nil
	}
	withSnap := post + "-ref"
	defer func() { _ = os.RemoveAll(withSnap) }()
	if _, err := copyTree(post, withSnap, brs["post_checkpoint"].walLimit); err != nil {
		return err
	}
	sSnap, rSnap, _, err := recoverState(ctx, withSnap)
	if err != nil || sSnap == nil {
		return fmt.Errorf("D16: recover with snapshot: %w", err)
	}
	sFull, rFull, _, err := recoverState(ctx, pre)
	if err != nil || sFull == nil {
		return fmt.Errorf("D16: full replay: %w", err)
	}
	out.tele("D16", level, "snapshot_hit", rSnap.SnapshotHit, "full_replay_snapshot_hit", rFull.SnapshotHit,
		"fingerprint_snapshot_plus_tail", sSnap.fingerpr, "fingerprint_full_replay", sFull.fingerpr)
	out.check("D16", level, "snapshot_used", rSnap.SnapshotHit && !rFull.SnapshotHit,
		"snapshot hit %v with the snapshot, %v without", rSnap.SnapshotHit, rFull.SnapshotHit)
	out.check("D16", level, "checkpoint_plus_tail_equals_full_replay", sSnap.fingerpr == sFull.fingerpr,
		"snapshot+tail %s, full replay %s", sSnap.fingerpr, sFull.fingerpr)

	// D09: a missing snapshot is refused loudly. After the checkpoint the WAL
	// control file records that the log's history requires the snapshot;
	// without it the directory is missing data, and recovery must say so rather
	// than open a shorter history.
	noSnap := post + "-nosnap"
	if err := cloneWithoutSnapshot(post, noSnap, brs["post_checkpoint"].walLimit); err != nil {
		return err
	}
	sMiss, rMiss, _, merr := recoverState(ctx, noSnap)
	missingAcked := -1
	if sMiss != nil {
		acks, attempts := log.snapshot(brs["post_checkpoint"].before)
		missingAcked = scoreState(sMiss, acks, attempts).missing
	}
	out.tele("D09.missing_segment", level, "recovery_error", fmt.Sprintf("%q", errText(merr)),
		"recovery_clean", rMiss.IsClean(), "acked_missing_if_opened", missingAcked)
	// Gated (rmp #2990): recovery must refuse with the typed error and report the
	// directory not clean; opening the shorter history is the defect.
	loud := errors.Is(merr, recovery.ErrMissingSnapshot) && !rMiss.IsClean()
	out.check("D09.missing_segment", level, "refused_loudly", loud,
		"recovery error %v, clean %v, %d acknowledged commit(s) missing if opened", merr, rMiss.IsClean(), missingAcked)
	return nil
}

// cloneWithoutSnapshot copies src to dst without its snapshot directory.
func cloneWithoutSnapshot(src, dst string, walLimit int64) error {
	if _, err := copyTree(src, dst, walLimit); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(dst, "snapshot"))
}

// faultWALFS is the D04 WAL filesystem: the os package, except that every
// segment file is opened behind a [testfs.FaultFile] carrying faults. The
// control file, the seal stub and directory fsyncs are unaffected. Each segment
// counts its own fsyncs; the D04 workload stays inside one segment, so its
// commit fsyncs all reach the tail segment's counter.
//
// Concurrency: the wal.Writer that owns it serialises every call.
type faultWALFS struct {
	segDir string
	faults testfs.Faults
}

func (fs faultWALFS) OpenFile(path string, flag int) (wal.WALFile, error) {
	f, err := os.OpenFile(path, flag, 0o600) //nolint:gosec // G304: the path is the example's own store directory
	if err != nil {
		return nil, err
	}
	if filepath.Dir(path) != fs.segDir {
		return f, nil
	}
	return testfs.Wrap(f, fs.faults), nil
}

func (faultWALFS) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func (faultWALFS) Remove(path string) error { return os.Remove(path) }

func (faultWALFS) ParentDirSync(childPath string) error {
	d, err := os.Open(filepath.Dir(childPath))
	if err != nil {
		return err
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	return serr
}

func (faultWALFS) ReadDir(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(ents))
	for i, e := range ents {
		names[i] = e.Name()
	}
	return names, nil
}

func (faultWALFS) MkdirAll(dir string) error { return os.MkdirAll(dir, 0o700) }

// fsyncFailAfter is how many WAL fsyncs the D04 arm lets succeed before every
// later one fails.
const fsyncFailAfter = 12

// armFsync is D04: a group-commit fsync failure. The WAL's tail segment runs over
// a fault file whose fsyncs fail after fsyncFailAfter successes (discarding the unsynced
// suffix, as a kernel that drops dirty pages does). No commit of the failed group
// may be acknowledged or become visible, the store must be poisoned, and a reopen
// recovers exactly the acknowledged commits.
func armFsync(ctx context.Context, dc *durabilityConfig, out *ladderOut, level int) error {
	dir, err := storeDirFor("D04", level, "fsync")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// Seed through a healthy store, then close it.
	{
		o, err := store.Open[string, float64](dir, store.Options[string, float64]{
			Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
		})
		if err != nil {
			return err
		}
		eng := cypher.NewEngineWithOpened(o)
		serr := setupSchema(ctx, eng)
		_ = o.Close()
		_ = eng.Close()
		if serr != nil {
			return serr
		}
	}
	res, err := recovery.OpenCtx[string, float64](ctx, dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		return err
	}
	walPath := filepath.Join(dir, walFile)
	wlog, err := wal.OpenFS(faultWALFS{segDir: wal.SegmentDir(walPath), faults: testfs.Faults{FailSyncAfter: fsyncFailAfter}}, walPath)
	if err != nil {
		_ = res.Graph.Close()
		return err
	}
	st := res.NewStoreCapped(wlog, txn.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	}, 0)
	eng := cypher.NewEngineWithStoreAndRecovery(st, res)
	log := newAckLog()
	ws := &writerSet{eng: eng, sink: log, stopOnFailure: true}
	wErr := ws.runWriters(ctx, level, perWriter(dc, level), dc.seed+2)
	poisoned := wlog.Poisoned()
	// After the poisoning every commit fails.
	_, postErr := drain(eng.RunInTx(ctx, qStmtD, P("a", int64(99_000_000), "g", 0, "t", tagOf(99_000_000))))
	// None of the failed commits is visible to a new reader of the live engine.
	live, lerr := readState(ctx, eng.NewSession())
	visibleFailed := 0
	if lerr == nil {
		_, attempts := log.snapshot(0)
		for id, a := range attempts {
			if a.o == oFailed && !live.absent(id) {
				visibleFailed++
			}
		}
	}
	_ = eng.Close()
	_ = wlog.Close() // the fault is still armed: the close's own fsync fails
	if wErr != nil {
		return wErr
	}
	if lerr != nil {
		return fmt.Errorf("D04: live read: %w", lerr)
	}
	failed := log.count(oFailed)
	reportTx(out, "D04", level, "fsync", &ws.st, 0)
	out.tele("D04", level, "fsync_fail_after", fsyncFailAfter, "waiters_failed", failed,
		"wal_sync_failed", wlog.Stats().SyncFailed, "poisoned", poisoned != nil,
		"post_poison_error", fmt.Sprintf("%q", errText(postErr)), "visible_failed", visibleFailed)
	out.check("D04", level, "failure_seen", failed > 0 && poisoned != nil,
		"%d commits failed, poisoned=%v: the fault never fired", failed, poisoned != nil)
	out.check("D04", level, "post_poison_commit_refused", postErr != nil, "a commit after the poisoning succeeded")
	out.check("D04", level, "failed_not_visible", visibleFailed == 0, "%d failed commits visible to a new reader", visibleFailed)
	br := bracket{before: log.acked(), after: log.acked()}
	br.walLimit = -1
	if br.imageBytes, err = dirSize(dir); err != nil {
		return err
	}
	v, err := verifyImage(ctx, out, "D04.reopen", level, dir, br, log, verifyOpts{fullOpen: true})
	if err != nil {
		return err
	}
	out.check("D04", level, "recovers_exactly_the_acknowledged", v.sc.pending == 0 && v.sc.inDoubtPresent == 0 && v.sc.missing == 0,
		"pending %d, unacknowledged present %d, missing %d", v.sc.pending, v.sc.inDoubtPresent, v.sc.missing)
	return nil
}
