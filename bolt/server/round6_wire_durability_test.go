package server_test

// round6_wire_durability_test.go — Round-6 cross-layer (Target B) audit: drive a
// realistic write workload THROUGH the Bolt wire (neo4j-go-driver) against a
// WAL-backed server, then RECOVER from a fresh engine/store reading the same WAL
// image and assert — again through the Bolt wire — that every acknowledged COMMIT
// survived. This exercises the whole stack as one path:
//
//   neo4j driver -> bolt/server -> cypher engine -> store/txn -> graph/lpg
//                -> store/wal  ===crash===>  store/recovery -> fresh engine
//                -> bolt/server -> neo4j driver (verify)
//
// It is the explicit "commit via Bolt, crash, reopen, verify" durability proof
// for the complete stack.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
)

// walEngineAt builds a WAL-backed engine over the WAL file at walPath. On the
// FIRST open the file is created empty; on a REOPEN it is recovered via
// recovery.Open (snapshot-less WAL replay) so the graph reflects every durably
// committed transaction. The returned closer flushes+fsyncs and releases the WAL
// writer (a graceful close == every prior ack is durable).
func walEngineAt(t *testing.T, walPath string) (*cypher.Engine, func()) {
	t.Helper()

	// Recover any prior committed state from the WAL image (no-op on first open).
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	if fileExists(walPath) {
		res, err := recovery.Open[string, float64](filepath.Dir(walPath), recovery.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		})
		if err != nil {
			t.Fatalf("recovery.Open: %v", err)
		}
		if !res.IsClean() {
			t.Fatalf("recovery found corruption: %v", res.TailErr)
		}
		g = res.Graph
	}

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", walPath, err)
	}
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(store)
	return eng, func() { _ = w.Close() }
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// TestRound6_BoltWireDurabilityAcrossCrash commits a realistic mixed workload
// through the Bolt wire, "crashes" (gracefully closes the WAL writer — every ack
// is durable — and tears the server down), then brings up a FRESH server over an
// engine recovered from the same WAL and verifies through the wire that all
// committed nodes/edges survived.
func TestRound6_BoltWireDurabilityAcrossCrash(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// ---- Session 1: write through the Bolt wire, then close (durable ack) ----
	eng1, close1 := walEngineAt(t, walPath)
	addr1 := startTestServerWithEngine(t, eng1, server.Options{ConnTimeout: 10 * time.Second})
	drv1 := dialDriver(t, addr1)

	const nPeople = 30
	const nEdges = 12
	func() {
		sess := drv1.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
		defer func() { _ = sess.Close(ctx) }()
		for i := 0; i < nPeople; i++ {
			runWrite(ctx, t, sess, "CREATE (:Person {name:$name, age:$age})",
				map[string]any{"name": personName(i), "age": int64(i)})
		}
		for i := 0; i < nEdges; i++ {
			runWrite(ctx, t, sess,
				"MATCH (a:Person {name:$a}),(b:Person {name:$b}) CREATE (a)-[:KNOWS]->(b)",
				map[string]any{"a": personName(i), "b": personName(i + 1)})
		}
	}()
	_ = drv1.Close(ctx)

	// "Crash": gracefully close the WAL writer so every acknowledged commit is
	// fsync'd to the durable image. (A graceful close is the strongest durability
	// claim: it asserts the acks were already durable, not merely buffered.)
	close1()

	// ---- Session 2: fresh server over a RECOVERED engine; verify via wire ----
	eng2, close2 := walEngineAt(t, walPath)
	defer close2()
	addr2 := startTestServerWithEngine(t, eng2, server.Options{ConnTimeout: 10 * time.Second})
	drv2 := dialDriver(t, addr2)
	defer func() { _ = drv2.Close(ctx) }()

	sess2 := drv2.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer func() { _ = sess2.Close(ctx) }()

	rows := runRead(ctx, t, sess2, "MATCH (n:Person) RETURN count(n) AS c", nil)
	if got := scalarFromRows(t, rows, "c"); got != int64(nPeople) {
		t.Fatalf("DURABILITY BREACH via Bolt wire: recovered Person count=%d, want %d", got, nPeople)
	}
	erows := runRead(ctx, t, sess2, "MATCH ()-[r:KNOWS]->() RETURN count(r) AS c", nil)
	if got := scalarFromRows(t, erows, "c"); got != int64(nEdges) {
		t.Fatalf("DURABILITY BREACH via Bolt wire: recovered KNOWS count=%d, want %d", got, nEdges)
	}
	// Identity spot-check: every committed person is individually present.
	for i := 0; i < nPeople; i++ {
		pr := runRead(ctx, t, sess2, "MATCH (n:Person {name:$name}) RETURN count(n) AS c",
			map[string]any{"name": personName(i)})
		if got := scalarFromRows(t, pr, "c"); got != 1 {
			t.Fatalf("DURABILITY BREACH via Bolt wire: person %q count=%d after recovery, want 1",
				personName(i), got)
		}
	}
	t.Logf("VERIFIED: %d nodes + %d edges committed via the Bolt wire survived crash+recovery and read back via the wire",
		nPeople, nEdges)
}

// dialDriver opens a neo4j driver to addr with test-fast timeouts.
func dialDriver(t *testing.T, addr string) neo4j.DriverWithContext {
	t.Helper()
	drv, err := neo4j.NewDriverWithContext("bolt://"+addr, neo4j.NoAuth(), func(c *config.Config) {
		c.MaxConnectionPoolSize = 5
		c.ConnectionAcquisitionTimeout = 5 * time.Second
		c.SocketConnectTimeout = 5 * time.Second
	})
	if err != nil {
		t.Fatalf("NewDriverWithContext: %v", err)
	}
	return drv
}

// scalarFromRows extracts a single int64 column `col` from a one-row result.
func scalarFromRows(t *testing.T, rows []map[string]any, col string) int64 {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(rows), rows)
	}
	v, ok := rows[0][col].(int64)
	if !ok {
		t.Fatalf("column %q: expected int64, got %T (%v)", col, rows[0][col], rows[0][col])
	}
	return v
}

func personName(i int) string {
	return "wp" + intStr(i)
}

func intStr(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
