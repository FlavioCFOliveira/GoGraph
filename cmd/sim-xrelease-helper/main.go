// Command sim-xrelease-helper is the prior-release subprocess driver for the
// DST cross-release harness (internal/sim). It is built FROM A PRIOR GIT TAG'S
// SOURCE TREE — the harness copies this file verbatim into a temporary git
// worktree checked out at the target tag and runs `go build` there, so the
// resulting binary embeds that release's store/txn/wal/cypher code. The current
// (HEAD) process then drives it over a small stdin/stdout protocol to obtain a
// store image written by the prior release and the prior release's observable
// per-op results.
//
// # Why a copied-in file rather than a tag-resident command
//
// A git tag is immutable and prior tags ship no headless CLI to drive. Adding
// an UNTRACKED file to a worktree of the tag does not modify the tag; it simply
// compiles against that tag's packages (every relevant tag shares the current
// module path github.com/FlavioCFOliveira/GoGraph and the same store/cypher API
// surface, verified by the harness build step). The file therefore uses ONLY
// the API that is stable across v0.2.0..HEAD: wal.Open, txn.NewStoreWithOptions,
// txn.New{String,Float64Weight}Codec, cypher.NewEngineWithStore, and the
// cypher.Result reader. It must not reference anything newer, or the build at an
// older tag fails (which the harness reports as "tag unbuildable", a clean skip).
//
// # Protocol
//
// Invocation:  sim-xrelease-helper write <dir>
//
//	stdin   one JSON object per line, each a {"kind","cypher","params"} op.
//	        params values are string | float64 (JSON number) | bool.
//	stdout  one JSON object per line, each {"i","committed","rows"} where rows
//	        is a canonical order-independent signature of the op's result rows
//	        (empty for ops that produced none). A trailing line
//	        {"done":true,"nodes":N,"edges":E,"checkpoint":B} reports the final
//	        engine counts and whether a checkpoint was published.
//	exit    0 on success (store written to <dir> and fsynced via store close);
//	        non-zero with a diagnostic on stderr on any harness-level failure.
//
// The helper writes a real WAL under <dir>/"wal" via the prior release's
// txn.Store, so the current code can reopen <dir> with recovery.Open — the
// genuine cross-version data-compatibility boundary.
//
// # The checkpoint (rmp #2477)
//
// Before exiting, the write path also publishes a durable CHECKPOINT: the prior
// release writes a SNAPSHOT DIRECTORY under <dir>/snapshot and truncates the WAL
// prefix the snapshot now covers. Without it the cross-release harness only ever
// handed the current code a WAL, so a prior release's manifest.json, csr.bin,
// labels.bin, properties.bin and mapper.bin had never been parsed by current
// code at all — the whole snapshot format was outside the cross-version test.
//
// The checkpoint lives in a SEPARATE staged file (checkpoint.go) reached through
// [publishCheckpointHook], so a tag whose checkpoint API differs degrades to a
// WAL-only image that says so on the wire instead of becoming an unbuildable
// skip. See the hook's own documentation.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// wireOp is the on-the-wire shape of one input operation. It mirrors the
// harness's sim.Op (Kind/Cypher/Params) but is restated here so the helper
// depends on no internal package (internal/* is import-restricted and would not
// be importable from a prior tag's tree anyway).
type wireOp struct {
	Params map[string]any `json:"params"`
	Kind   string         `json:"kind"`
	Cypher string         `json:"cypher"`
}

// wireResult is the per-op output line.
type wireResult struct {
	Rows      string `json:"rows"`
	Index     int    `json:"i"`
	Committed bool   `json:"committed"`
}

// wireDone is the final summary line.
type wireDone struct {
	// CheckpointErr is the prior release's checkpoint failure, empty when the
	// checkpoint succeeded or was not attempted. It is reported rather than
	// raised so a tag whose checkpoint cannot run still yields a usable WAL
	// image instead of aborting the whole cross-release run.
	CheckpointErr string `json:"checkpoint_err,omitempty"`
	Done          bool   `json:"done"`
	Nodes         int64  `json:"nodes"`
	Edges         int64  `json:"edges"`
	// Checkpoint reports that this build published a durable CHECKPOINT before
	// exiting: a snapshot directory under <dir>/snapshot plus a WAL truncated to
	// the snapshot's watermark. False means the binary was built WITHOUT the
	// staged checkpoint source (see publishCheckpointHook), so <dir> holds a
	// WAL-only image exactly as it did before rmp #2477.
	Checkpoint bool `json:"checkpoint"`
}

// publishCheckpointHook publishes a durable checkpoint over the store this
// helper just wrote, or is nil when the binary was built without the staged
// checkpoint source.
//
// The hook exists because this file is compiled against a PRIOR RELEASE'S
// package tree, where the checkpoint API may not exist in the shape the current
// tree uses — or at all. Keeping the checkpoint in a SEPARATE staged file
// (cmd/sim-xrelease-helper/checkpoint.go) lets the harness build with it, and,
// if that build fails, drop just that file and rebuild: the tag then yields a
// WAL-only image and SAYS SO on the wire (wireDone.Checkpoint = false), rather
// than the whole tag becoming an unbuildable skip. Declaring the seam here — in
// the file that is always staged — is what makes the second file removable.
var publishCheckpointHook func(dir string, g *lpg.Graph[string, float64], st *txn.Store[string, float64], wlog *wal.Writer) error

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: sim-xrelease-helper write|selfcheck <dir>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "write":
		if err := run(os.Args[2], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "sim-xrelease-helper: %v\n", err)
			os.Exit(1)
		}
	case "selfcheck":
		// Reopen a dir this helper previously wrote using the PRIOR release's OWN
		// recovery, and report the recovered counts. It is the discriminator
		// between a prior-release WAL that does not round-trip in its own release
		// (a prior bug) and one the CURRENT recovery mis-reads (a current
		// regression).
		if err := selfcheck(os.Args[2], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "sim-xrelease-helper: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: sim-xrelease-helper write|selfcheck <dir>")
		os.Exit(2)
	}
}

// selfcheck reopens dir with the prior release's recovery.Open and prints a
// single {"done":true,"nodes":N,"edges":E} line with the recovered counts, so
// the harness can compare the prior release's SELF-recovery against its live
// counts and against the current code's recovery.
func selfcheck(dir string, out *os.File) error {
	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		return fmt.Errorf("self-recovery open: %w", err)
	}
	eng := cypher.NewEngine(res.Graph)
	ctx := context.Background()
	nodes := scalarCount(ctx, eng, "MATCH (n) RETURN count(n)")
	edges := scalarCount(ctx, eng, "MATCH ()-[r]->() RETURN count(r)")
	enc := json.NewEncoder(out)
	return enc.Encode(wireDone{Done: true, Nodes: nodes, Edges: edges})
}

// run opens a WAL-backed store under dir, replays the JSON op stream from in,
// emits a result line per op to out, then closes the store (flush+fsync) so the
// image is durable for the current process to reopen.
func run(dir string, in, out *os.File) (retErr error) {
	walPath := filepath.Join(dir, "wal")
	wlog, err := wal.Open(walPath)
	if err != nil {
		return fmt.Errorf("open WAL %q: %w", walPath, err)
	}
	// Directed simple graph, matching the harness's simulatorStoreConfig so the
	// oracle (which collapses parallel edges) stays a faithful model.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: false})
	store := txn.NewStoreWithOptions(g, wlog, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(store)

	w := bufio.NewWriter(out)
	defer func() {
		// Close the store first so the WAL is flushed+fsynced (durable image),
		// then flush our stdout. A close error is the durability fault we must
		// surface rather than report success.
		closeErr := wlog.Close()
		flushErr := w.Flush()
		if retErr == nil {
			if closeErr != nil {
				retErr = fmt.Errorf("close WAL: %w", closeErr)
			} else if flushErr != nil {
				retErr = fmt.Errorf("flush stdout: %w", flushErr)
			}
		}
	}()

	ctx := context.Background()
	enc := json.NewEncoder(w)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	idx := 0
	for sc.Scan() {
		line := sc.Bytes()
		if strings.TrimSpace(string(line)) == "" {
			continue
		}
		var op wireOp
		if err := json.Unmarshal(line, &op); err != nil {
			return fmt.Errorf("op %d: decode: %w", idx, err)
		}
		committed, rows, err := execOp(ctx, eng, op)
		if err != nil {
			return fmt.Errorf("op %d: %w", idx, err)
		}
		if err := enc.Encode(wireResult{Index: idx, Committed: committed, Rows: rows}); err != nil {
			return fmt.Errorf("op %d: encode result: %w", idx, err)
		}
		idx++
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	nodes, edges := scalarCount(ctx, eng, "MATCH (n) RETURN count(n)"), scalarCount(ctx, eng, "MATCH ()-[r]->() RETURN count(r)")

	// Publish the checkpoint while the WAL writer is still open: the checkpoint
	// truncates the WAL prefix through that writer, so it cannot run after the
	// deferred close. The counts above are read from the LIVE engine and are
	// therefore unaffected by where in this function the checkpoint runs.
	done := wireDone{Done: true, Nodes: nodes, Edges: edges}
	if publishCheckpointHook != nil {
		if cperr := publishCheckpointHook(dir, g, store, wlog); cperr != nil {
			// Report, do not raise. A tag that cannot check-point still produced a
			// valid WAL image, and the harness's contract for that image is
			// unchanged; hiding the failure, or aborting on it, would both be worse
			// than saying which of the two shapes <dir> actually holds.
			done.CheckpointErr = cperr.Error()
		} else {
			done.Checkpoint = true
		}
	}
	if err := enc.Encode(done); err != nil {
		return fmt.Errorf("encode done: %w", err)
	}
	return nil
}

// execOp runs one op against the engine through the appropriate path and
// returns whether it committed cleanly plus a canonical signature of its rows.
func execOp(ctx context.Context, eng *cypher.Engine, op wireOp) (committed bool, rows string, err error) {
	params, err := toExprParams(op.Params)
	if err != nil {
		return false, "", err
	}
	var res *cypher.Result
	if isWrite(op.Kind) {
		res, err = eng.RunInTx(ctx, op.Cypher, params)
	} else {
		res, err = eng.Run(ctx, op.Cypher, params)
	}
	if err != nil {
		// An engine error is a legitimate observable outcome (e.g. a malformed
		// op rejected). It is not a harness failure; report it as not committed.
		return false, "error", nil
	}
	sig := canonicalRows(res)
	drainErr := res.Err()
	_ = res.Close()
	return drainErr == nil, sig, nil
}

// canonicalRows drains a result and returns an order-independent signature: each
// row rendered to a stable string, then sorted and joined, so two engines that
// emit the same multiset in different orders compare equal.
//
// Rows are read via Result.Record (a column-name -> value map, the API stable
// across v0.2.0..HEAD; the positional ValueAt accessor postdates v0.3.0). Values
// are rendered with %v, which prints an expr.Value through its Stringer exactly
// as the current side's renderRowFromRecord does, so prior and current
// signatures compare byte-for-byte.
func canonicalRows(res *cypher.Result) string {
	cols := res.Columns()
	var out []string
	for res.Next() {
		out = append(out, renderRecord(res.Record(), cols))
	}
	sort.Strings(out)
	return "[" + strings.Join(out, "|") + "]"
}

// renderRecord renders a result row (a column-name -> value map) to a stable
// comma-joined string in column order.
func renderRecord(rec map[string]any, cols []string) string {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, fmt.Sprintf("%v", rec[c]))
	}
	return strings.Join(parts, ",")
}

// scalarCount runs a count query and returns the first-column integer, or -1 on
// any error (a count probe must never fail on a healthy engine; -1 makes a
// silent failure visible to the comparing side).
func scalarCount(ctx context.Context, eng *cypher.Engine, query string) int64 {
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		return -1
	}
	defer func() { _ = res.Close() }()
	cols := res.Columns()
	var n int64 = -1
	if res.Next() && len(cols) > 0 {
		if v, ok := res.Record()[cols[0]].(expr.IntegerValue); ok {
			n = int64(v)
		}
	}
	if res.Err() != nil {
		return -1
	}
	return n
}

// isWrite mirrors sim.OpKind.IsWrite: every mutating or malformed op runs
// through the write (RunInTx) path; a match runs through the read path.
func isWrite(kind string) bool {
	switch kind {
	case "OpCreate", "OpMerge", "OpDelete", "OpUpdate", "OpMalformed":
		return true
	default:
		return false
	}
}

// toExprParams converts the JSON-decoded params to the engine's expr.Value map.
// JSON numbers decode as float64; an integral float64 is mapped to IntegerValue
// so an age/count parameter binds as an integer exactly as the in-process
// harness binds it (the harness emits int64 ages, which JSON renders without a
// fraction). A fractional float64 stays a FloatValue.
func toExprParams(params map[string]any) (map[string]expr.Value, error) {
	if len(params) == 0 {
		return nil, nil
	}
	out := make(map[string]expr.Value, len(params))
	for k, v := range params {
		switch t := v.(type) {
		case string:
			out[k] = expr.StringValue(t)
		case bool:
			out[k] = expr.BoolValue(t)
		case float64:
			if t == float64(int64(t)) {
				out[k] = expr.IntegerValue(int64(t))
			} else {
				out[k] = expr.FloatValue(t)
			}
		default:
			return nil, fmt.Errorf("param %q: unsupported type %T", k, v)
		}
	}
	return out, nil
}
