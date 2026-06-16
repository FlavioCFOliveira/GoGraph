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
//	        {"done":true,"nodes":N,"edges":E} reports the final engine counts.
//	exit    0 on success (store written to <dir> and fsynced via store close);
//	        non-zero with a diagnostic on stderr on any harness-level failure.
//
// The helper writes a real WAL under <dir>/"wal" via the prior release's
// txn.Store, so the current code can reopen <dir> with recovery.Open — the
// genuine cross-version data-compatibility boundary.
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
	Kind   string         `json:"kind"`
	Cypher string         `json:"cypher"`
	Params map[string]any `json:"params"`
}

// wireResult is the per-op output line.
type wireResult struct {
	Index     int    `json:"i"`
	Committed bool   `json:"committed"`
	Rows      string `json:"rows"`
}

// wireDone is the final summary line.
type wireDone struct {
	Done  bool  `json:"done"`
	Nodes int64 `json:"nodes"`
	Edges int64 `json:"edges"`
}

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
	if err := enc.Encode(wireDone{Done: true, Nodes: nodes, Edges: edges}); err != nil {
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
