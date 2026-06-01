package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// cmdStats counts nodes by label and edges by relationship type and
// writes a single JSON object with the alphabetical keys
// authored / comments / follows / likes / on / posts / replies /
// users to stdout. The counts are computed through the same
// WAL-backed Cypher engine used by `query` so they reflect the
// committed state at invocation time.
//
//	{"authored":N,"comments":N,"follows":N,"likes":N,
//	 "on":N,"posts":N,"replies":N,"users":N}
func cmdStats(args []string) error {
	dir, _, err := parseDataDir("stats", args)
	if err != nil {
		return err
	}
	return runStats(context.Background(), dir, os.Stdout)
}

// runStats is the entry point used by both cmdStats and the round-trip
// test in T9.
func runStats(ctx context.Context, dir string, out io.Writer) (retErr error) {
	o, err := openStore(ctx, dir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := o.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("stats: close store: %w", cerr)
		}
	}()

	counts := make(map[string]any, len(statQueries))
	for _, q := range statQueries {
		n, err := countOne(ctx, o.engine, q.query)
		if err != nil {
			return fmt.Errorf("stats: %s: %w", q.key, err)
		}
		counts[q.key] = n
	}
	return writeJSONObject(out, counts)
}

// statQueries lists the eight Cypher count queries that produce the
// stats output, paired with the JSON key they fill. Keeping them in a
// single slice makes the contract from doc.go visible in one place.
var statQueries = []struct {
	key, query string
}{
	{"users", `MATCH (n:User) RETURN count(n) AS n`},
	{"posts", `MATCH (n:Post) RETURN count(n) AS n`},
	{"comments", `MATCH (n:Comment) RETURN count(n) AS n`},
	{"follows", `MATCH ()-[r:FOLLOWS]->() RETURN count(r) AS n`},
	{"authored", `MATCH ()-[r:AUTHORED]->() RETURN count(r) AS n`},
	{"on", `MATCH ()-[r:ON]->() RETURN count(r) AS n`},
	{"replies", `MATCH ()-[r:REPLY_OF]->() RETURN count(r) AS n`},
	{"likes", `MATCH ()-[r:LIKED]->() RETURN count(r) AS n`},
}

// countOne runs a single count(*) Cypher query against eng and
// returns the integer result from the column named "n".
func countOne(ctx context.Context, eng *cypher.Engine, query string) (int64, error) {
	res, err := eng.RunInTx(ctx, query, nil)
	if err != nil {
		return 0, fmt.Errorf("run: %w", err)
	}
	defer func() { _ = res.Close() }()

	var n int64
	for res.Next() {
		rec := res.Record()
		v, ok := rec["n"]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case int64:
			n = x
		case int:
			n = int64(x)
		default:
			if iv, ok := jsonValue(v).(int64); ok {
				n = iv
			}
		}
	}
	if err := res.Err(); err != nil {
		return 0, fmt.Errorf("iterate: %w", err)
	}
	return n, nil
}
