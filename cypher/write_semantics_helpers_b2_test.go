package cypher_test

// write_semantics_helpers_b2_test.go — the engine harness shared by the
// regression gates for rmp #2958, #2952 and #2953. Every gate runs each shape
// twice: on the in-memory engine, and on the WAL-backed engine, whose log is
// then replayed by recovery so the durable outcome is checked as well as the
// live one.

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// engineB2 is one engine under test. recover is nil for the in-memory engine;
// for the WAL-backed engine it closes the log and returns an engine over the
// graph recovery rebuilt from it. wlog is the WAL-backed engine's log writer
// (nil for the in-memory engine); the harness closes it when the test ends.
type engineB2 struct {
	eng     *cypher.Engine
	recover func(t *testing.T) *cypher.Engine
	wlog    *wal.Writer
}

// enginesB2 returns the two engines every shape runs on, keyed by name.
func enginesB2() map[string]func(t *testing.T) engineB2 {
	return map[string]func(t *testing.T) engineB2{
		"memory": func(_ *testing.T) engineB2 {
			return engineB2{eng: cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))}
		},
		"wal": func(t *testing.T) engineB2 {
			t.Helper()
			dir := t.TempDir()
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatalf("wal.Open: %v", err)
			}
			// The writer must be closed before t.TempDir's RemoveAll runs:
			// Open starts a background segment preparer that creates the next
			// segment file in dir/wal.d, and only Close waits for it. An
			// unclosed writer let that file appear while RemoveAll was
			// emptying wal.d (rmp #3095). Registered after t.TempDir, this
			// cleanup runs before it. recover may already have closed it.
			t.Cleanup(func() {
				if err := w.Close(); err != nil && !errors.Is(err, wal.ErrWriterClosed) {
					t.Errorf("wal.Close: %v", err)
				}
			})
			g := lpg.New[string, float64](adjlist.Config{})
			st := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewFloat64WeightCodec(),
			})
			return engineB2{
				eng:  cypher.NewEngineWithStore(st),
				wlog: w,
				recover: func(t *testing.T) *cypher.Engine {
					t.Helper()
					if err := w.Close(); err != nil {
						t.Fatalf("wal.Close: %v", err)
					}
					rec, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
						Codec:       txn.NewStringCodec(),
						WeightCodec: txn.NewFloat64WeightCodec(),
					})
					if err != nil {
						t.Fatalf("recovery.Open: %v", err)
					}
					return cypher.NewEngine(rec.Graph)
				},
			}
		},
	}
}

// queryRowsB2 runs a read query and renders every row as its columns' canonical
// strings (see canon2941), joined in cols order.
func queryRowsB2(t *testing.T, eng *cypher.Engine, query string, cols ...string) []string {
	t.Helper()
	res, err := eng.Run(t.Context(), query, nil)
	if err != nil {
		t.Fatalf("%q: %v", query, err)
	}
	defer res.Close()
	var rows []string
	for res.Next() {
		rec := res.Record()
		line := ""
		for i, c := range cols {
			if i > 0 {
				line += " "
			}
			line += c + "=" + canon2941(rec[c])
		}
		rows = append(rows, line)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%q drain: %v", query, err)
	}
	return rows
}
