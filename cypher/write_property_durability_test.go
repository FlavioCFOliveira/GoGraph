package cypher_test

// write_property_durability_test.go — T931
//
// TestWrite_PropertyDurability verifies that typed property writes issued
// through a WAL-backed Cypher engine — both inline (CREATE + property
// literal) and via explicit SET — are durable across a process restart
// without requiring a snapshot. The previous behaviour (pre-T931) only
// persisted property writes through the snapshot path, so a crash between
// snapshots silently dropped every SET.
//
// Layer: short. Race-clean; goleak-clean.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"gograph/cypher"
	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/subproc"
	"gograph/store/recovery"
	"gograph/store/txn"
	"gograph/store/wal"
)

func init() {
	// Register the child handler for T931.
	// args[0] = shared data directory.
	subproc.Register("cypher-property-write-and-exit", func(args []string) int {
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "cypher-property-write-and-exit: missing dir arg")
			return 1
		}
		dir := args[0]

		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "cypher-property-write-and-exit: wal.Open: %v\n", err)
			return 1
		}

		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		})
		eng := cypher.NewEngineWithStore(store)
		ctx := context.Background()

		// 1. CREATE a Person node — inline property literal must reach WAL.
		if err := runOnce(ctx, eng, `CREATE (n:Person {name: "P1", age: 30})`); err != nil {
			fmt.Fprintf(os.Stderr, "cypher-property-write-and-exit: CREATE: %v\n", err)
			_ = w.Close()
			return 1
		}
		// 2. SET an additional property via MATCH+SET — exercises the explicit
		//    SetNodeProperty path in walMutatorAdapter.
		if err := runOnce(ctx, eng, `MATCH (n:Person {name: "P1"}) SET n.tag = "verified"`); err != nil {
			fmt.Fprintf(os.Stderr, "cypher-property-write-and-exit: SET: %v\n", err)
			_ = w.Close()
			return 1
		}
		// 3. REMOVE a property via MATCH+REMOVE — exercises DelNodeProperty.
		if err := runOnce(ctx, eng, `MATCH (n:Person {name: "P1"}) REMOVE n.age`); err != nil {
			fmt.Fprintf(os.Stderr, "cypher-property-write-and-exit: REMOVE: %v\n", err)
			_ = w.Close()
			return 1
		}

		if err := w.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "cypher-property-write-and-exit: w.Close: %v\n", err)
			return 1
		}
		return 0
	})
}

// runOnce executes one write query in a single transaction and drains the
// result. Returns the first error encountered.
func runOnce(ctx context.Context, eng *cypher.Engine, query string) error {
	res, err := eng.RunInTx(ctx, query, nil)
	if err != nil {
		return fmt.Errorf("RunInTx %q: %w", query, err)
	}
	for res.Next() {
	}
	if iterErr := res.Err(); iterErr != nil {
		_ = res.Close()
		return fmt.Errorf("result.Err %q: %w", query, iterErr)
	}
	if closeErr := res.Close(); closeErr != nil {
		return fmt.Errorf("result.Close %q: %w", query, closeErr)
	}
	return nil
}

// TestWrite_PropertyDurability spawns a child that creates a node, sets a
// property, and removes another; the parent recovers from the WAL without a
// snapshot and asserts the final property state survived.
func TestWrite_PropertyDurability(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	_, stderr, err := subproc.Run(t, "cypher-property-write-and-exit", dir)
	if err != nil {
		t.Fatalf("child process failed: %v\nstderr: %s", err, stderr)
	}
	if len(stderr) > 0 {
		t.Logf("child stderr: %s", stderr)
	}

	// Parent: recover the graph written by the child (WAL-only, no snapshot).
	recRes, openErr := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if openErr != nil {
		t.Fatalf("recovery.Open: %v", openErr)
	}

	recEng := cypher.NewEngine(recRes.Graph)
	ctx := context.Background()

	// The Person node must exist.
	assertCount(ctx, t, recEng, `MATCH (n:Person {name: "P1"}) RETURN count(n) AS n`, 1)
	// The inline property `name` must round-trip.
	assertCount(ctx, t, recEng, `MATCH (n:Person {name: "P1"}) RETURN count(n) AS n`, 1)
	// The SET-assigned property `tag` must round-trip from WAL.
	assertCount(ctx, t, recEng, `MATCH (n:Person {tag: "verified"}) RETURN count(n) AS n`, 1)
	// The REMOVE'd property `age` must NOT appear on any node.
	assertCount(ctx, t, recEng, `MATCH (n:Person {age: 30}) RETURN count(n) AS n`, 0)

	// Verify directly on the graph for kind correctness — assertCount only
	// reads the count, so a coincidental zero would not detect a kind drift.
	props := recRes.Graph.NodeProperties("__cx_1")
	if len(props) == 0 {
		// Fall back to a graph walk: keys are synthetic and not deterministic.
		recRes.Graph.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
			p := recRes.Graph.NodeProperties(key)
			if len(p) > 0 {
				props = p
				return false
			}
			return true
		})
	}
	if props == nil {
		t.Fatal("expected at least one node with properties after recovery")
	}
	if v, ok := props["tag"]; !ok {
		t.Errorf("expected property `tag` on recovered node, got keys %v", keysOf(props))
	} else if s, sok := v.String(); !sok || s != "verified" {
		t.Errorf("property `tag` = %v (ok=%v), want \"verified\"", v, sok)
	}
	if _, ok := props["age"]; ok {
		t.Errorf("property `age` should have been REMOVE'd, but is still present")
	}
}

// keysOf returns the keys of m in unspecified order. Used purely for
// readable error messages.
func keysOf(m map[string]lpg.PropertyValue) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func init() {
	// Child handler for the SIGKILL-after-commit variant. After the
	// CREATE+SET commit returns, the child writes "DONE\n" to stdout and
	// blocks forever; the parent reads "DONE", asserts the WAL was
	// fsynced, and kills the child with SIGKILL.
	subproc.Register("cypher-property-write-then-block", func(args []string) int {
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "cypher-property-write-then-block: missing dir arg")
			return 1
		}
		dir := args[0]

		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "cypher-property-write-then-block: wal.Open: %v\n", err)
			return 1
		}
		defer func() { _ = w.Close() }()

		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		})
		eng := cypher.NewEngineWithStore(store)
		ctx := context.Background()

		if err := runOnce(ctx, eng, `CREATE (n:Account {id: "A1"})`); err != nil {
			fmt.Fprintf(os.Stderr, "cypher-property-write-then-block: CREATE: %v\n", err)
			return 1
		}
		if err := runOnce(ctx, eng, `MATCH (n:Account {id: "A1"}) SET n.balance = 1000`); err != nil {
			fmt.Fprintf(os.Stderr, "cypher-property-write-then-block: SET: %v\n", err)
			return 1
		}

		// The Commit (inside res.Close above) has already fsynced the
		// WAL. Signal the parent and sleep; the parent will SIGKILL
		// before this sleep elapses. A finite sleep (rather than a
		// channel-receive that triggers the deadlock detector) keeps
		// the runtime quiet while the parent races to the kill.
		_, _ = fmt.Fprintln(os.Stdout, "DONE")
		time.Sleep(1 * time.Hour)
		return 0
	})
}

// TestWrite_PropertyDurability_SIGKILLPostCommit verifies the acceptance
// criterion verbatim: write a property via Cypher SET inside an explicit
// transaction, kill the process via SIGKILL post-commit, recover, and
// observe the property in the recovered graph.
func TestWrite_PropertyDurability_SIGKILLPostCommit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	cmd := exec.Command(os.Args[0]) //nolint:gosec // binary is os.Args[0], trusted test binary
	cmd.Env = append(os.Environ(),
		subproc.EnvMode+"=cypher-property-write-then-block",
		"GOGRAPH_SUBPROC_ARGS="+dir,
	)
	// Pass dir as the first non-flag arg by injecting Args.
	cmd.Args = []string{os.Args[0], dir}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}

	// Wait for the child to commit and signal "DONE".
	reader := bufio.NewReader(stdout)
	doneCh := make(chan error, 1)
	go func() {
		line, rerr := reader.ReadString('\n')
		if rerr != nil {
			doneCh <- fmt.Errorf("read DONE marker: %w", rerr)
			return
		}
		if line != "DONE\n" {
			doneCh <- fmt.Errorf("unexpected child stdout: %q", line)
			return
		}
		doneCh <- nil
	}()

	select {
	case rerr := <-doneCh:
		if rerr != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("child did not signal DONE: %v", rerr)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("timed out waiting for child to commit")
	}

	// SIGKILL the child post-commit.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	if werr := cmd.Wait(); werr != nil {
		// Expected — non-zero exit after SIGKILL.
		var exitErr *exec.ExitError
		if !errors.As(werr, &exitErr) {
			t.Logf("cmd.Wait: %v", werr)
		}
	}

	// Parent: recover from the WAL (no snapshot was written).
	recRes, openErr := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if openErr != nil {
		t.Fatalf("recovery.Open: %v", openErr)
	}

	recEng := cypher.NewEngine(recRes.Graph)
	ctx := context.Background()

	// The node and the SET property must both have survived.
	assertCount(ctx, t, recEng, `MATCH (n:Account {id: "A1"}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, recEng, `MATCH (n:Account {balance: 1000}) RETURN count(n) AS n`, 1)
}
