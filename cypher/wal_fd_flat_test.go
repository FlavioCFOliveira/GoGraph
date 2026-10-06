package cypher

// wal_fd_flat_test.go — opening, writing and closing a WAL-backed engine
// releases every file descriptor it took, on every open route (the
// fd-exhaustion of the full straddler enumeration, rmp #2936: whether the module
// or the harness held them).
//
// Layer: short.

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// openFDs reports how many file descriptors the process holds, or -1 where the
// platform exposes no per-process descriptor directory.
func openFDs() int {
	for _, dir := range []string{"/dev/fd", "/proc/self/fd"} {
		if entries, err := os.ReadDir(dir); err == nil {
			return len(entries)
		}
	}
	return -1
}

func TestWALEngine_OpenWriteCloseIsFDFlat(t *testing.T) {
	const iterations = 250
	if openFDs() < 0 {
		t.Skip("no per-process file-descriptor directory on this platform")
	}
	opts := txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
	routes := map[string]func(t *testing.T, dir string){
		// wal.Open → txn store → store.DB, written through an engine, closed
		// through the DB.
		"store": func(t *testing.T, dir string) {
			wr, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatal(err)
			}
			st := txn.NewStoreWithOptions[string, float64](lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true}), wr, opts)
			db := store.New(wr, store.WithQuiesce(st.RunUnderCommitLock))
			if err := straddleRun(NewEngineWithStore(st), `CREATE (:L {s: 'v'})`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		},
		// recovery.Open over a written directory.
		"recovery": func(t *testing.T, dir string) {
			if _, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{Codec: opts.Codec, WeightCodec: opts.WeightCodec}); err != nil {
				t.Fatal(err)
			}
		},
		// A recovered graph reopened through NewEngineWithStoreAndRecovery,
		// written, and closed.
		"recovered-engine": func(t *testing.T, dir string) {
			rec, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{Codec: opts.Codec, WeightCodec: opts.WeightCodec})
			if err != nil {
				t.Fatal(err)
			}
			wr, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatal(err)
			}
			st := txn.NewStoreWithOptions[string, float64](rec.Graph, wr, opts)
			db := store.New(wr, store.WithQuiesce(st.RunUnderCommitLock))
			if err := straddleRun(NewEngineWithStoreAndRecovery(st, rec), `CREATE (:L {s: 'w'})`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		},
	}
	for _, name := range []string{"store", "recovery", "recovered-engine"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			routes["store"](t, dir) // a written directory for the recovery routes
			// Counted with NO garbage collection between the two counts: a
			// collection runs the finalizer of every unreachable *os.File, which
			// closes a descriptor the route leaked and hides the leak (audit R5-3).
			// The collector is off for the measured cycles only — each cycle
			// allocates about 0.8 MB, so a thousand of them would hold 2.4 GB —
			// and a leak of one descriptor per cycle still shows 250-fold.
			base := openFDs()
			func() {
				defer debug.SetGCPercent(debug.SetGCPercent(-1))
				for i := 0; i < iterations; i++ {
					routes[name](t, dir)
				}
			}()
			if got := openFDs(); got > base+4 {
				t.Fatalf("%d open file descriptors after %d open/write/close cycles, %d before: the route leaks",
					got, iterations, base)
			}
		})
	}
}
