//go:build r4audit

package server_test

// zz_r4_show_test.go — round-4 audit probe: does SHOW INDEXES / SHOW CONSTRAINTS
// deliver its column values over Bolt?
//
// session.go:1292-1294 reads each row with Result.ValueAt on the stated premise
// that "the engine result is always materialised". SHOW builds a STREAMING
// result (cypher/show.go:164 exec.Run over StaticRows), and ValueAt returns nil
// for a non-materialised result (cypher/api.go:3985-4004).

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

func TestR4_ShowIndexesOverBolt(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	if _, err := session.Run(ctx, "CREATE (:P {name: 'x'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := session.Run(ctx, "CREATE INDEX p_name FOR (n:P) ON (n.name)", nil); err != nil {
		t.Fatalf("create index: %v", err)
	}

	for _, q := range []string{"SHOW INDEXES", "SHOW CONSTRAINTS"} {
		res, err := session.Run(ctx, q, nil)
		if err != nil {
			t.Logf("%-18s RUN ERROR %v", q, err)
			continue
		}
		rows := 0
		nonNil := 0
		var keys []string
		for res.Next(ctx) {
			rec := res.Record()
			keys = rec.Keys
			for _, k := range rec.Keys {
				if v, _ := rec.Get(k); v != nil {
					nonNil++
				}
			}
			rows++
		}
		if err := res.Err(); err != nil {
			t.Logf("%-18s ERR %v", q, err)
			continue
		}
		t.Logf("%-18s rows=%d keys=%v non-nil values=%d", q, rows, keys, nonNil)
		if rows > 0 && nonNil == 0 {
			t.Errorf("%s returned %d row(s) over Bolt with EVERY column null", q, rows)
		}
	}
}
