package sim

import (
	"context"
	"fmt"
)

// indexDiversitySpecs are the indexes the index-diversity scenario creates and
// cross-checks: a HASH index on a string property, a BTREE index on a numeric
// property, and a BTREE index on a second string property. Together they cover
// both index kinds and both string and integer value types; the seek-vs-scan
// consistency invariant is identical for each, so a divergence on any one is a
// real index bug for that (kind, value-type).
var indexDiversitySpecs = []IndexSpec{
	{Label: "Person", Property: "name"},               // hash, string
	{Label: "Person", Property: "age", Numeric: true}, // btree, numeric
	{Label: "Person", Property: "city"},               // btree, string
}

// indexDiversityDDL creates the three indexes. They are declared AFTER the bulk
// load so each backfill runs over an above-threshold graph, engaging the
// morsel-parallel phase-2 of the backfill (the > backfillParallelMinNodes path).
var indexDiversityDDL = []string{
	"CREATE INDEX idx_person_name FOR (n:Person) ON (n.name) OPTIONS {indexType:'hash'}",
	"CREATE INDEX idx_person_age FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}",
	"CREATE INDEX idx_person_city FOR (n:Person) ON (n.city) OPTIONS {indexType:'btree'}",
}

// indexDiversityBulk is the number of Person nodes the scenario bulk-loads before
// creating the indexes — comfortably above backfillParallelMinNodes (8192) so the
// parallel backfill phase is exercised.
const indexDiversityBulk = 9000

// indexDiversityScenario verifies index-type diversity and the parallel backfill
// under the DST: it bulk-loads an above-threshold Person graph, creates a HASH
// (string), a BTREE (numeric), and a BTREE (string) index — each backfilled
// through the morsel-parallel phase — then churns writes with crash/recovery,
// running the thorough seek-vs-scan index-consistency check throughout and after
// every recovery. It is bit-reproducible (the parallel backfill produces
// identical index contents regardless of worker scheduling).
func indexDiversityScenario() Scenario {
	return Scenario{
		Name:        ScenarioIndexDiversity,
		Description: "hash + btree + numeric indexes, parallel backfill, seek-vs-scan consistency through crash/recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x10DE5,
		MaxTicks:    260,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 60.0, StabilityWindow: 20},
		run:         runIndexDiversity,
	}
}

// runIndexDiversity bulk-loads the graph, creates the diverse indexes (parallel
// backfill), then drives a churn loop that maintains the indexed properties and
// crashes periodically. It runs the seek-vs-scan consistency check after the
// initial backfill, periodically during churn, and immediately after every
// crash/recovery. It crashes WITHOUT the oracle durability check (the bulk nodes
// are not modelled in the minimal oracle; this scenario's invariant is engine
// self-consistency — the index agreeing with its own base data — not oracle
// parity). It is deterministic.
func runIndexDiversity(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := indexDiversityScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: index-diversity new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	// Bulk-load an above-threshold Person graph with string + numeric properties.
	for i := 0; i < indexDiversityBulk; i++ {
		q := fmt.Sprintf("CREATE (:Person {name:'p%d', age:%d, city:'c%d'})", i, i%500, i%100)
		if !sm.execute(ctx, Op{Kind: OpCreate, Cypher: q}) {
			return nil, fmt.Errorf("sim: index-diversity bulk load failed at %d", i)
		}
	}
	// Create the diverse indexes; each backfills the 9000-node graph in parallel.
	for _, ddl := range indexDiversityDDL {
		if err := sm.engineRunDDL(ctx, ddl); err != nil {
			return nil, fmt.Errorf("sim: index-diversity DDL %q: %w", ddl, err)
		}
	}
	// Consistency right after the parallel backfill.
	if v := CheckIndexConsistency(0, nil, sm.engine, indexDiversitySpecs...); len(v) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: "<post-backfill index check>"}, v), nil
	}

	churn := indexDiversityBulk
	for i := 0; i < cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := sm.clock.Tick()

		// Manual crash (no oracle durability check — see the doc comment). On
		// recovery the indexes are re-registered and re-backfilled; the
		// consistency check must then still hold against the recovered graph.
		if sm.crash.ShouldCrash(tick) {
			sm.store.Crash()
			store, oerr := OpenSimStore(sm.disk, simulatorStoreConfig())
			if oerr != nil {
				return nil, fmt.Errorf("sim: index-diversity crash recovery at tick %d: %w", tick, oerr)
			}
			sm.store = store
			sm.engine = NewEngineAdapter(store.Engine())
			sm.crashCount++
			if v := CheckIndexConsistency(tick, nil, sm.engine, indexDiversitySpecs...); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery index check>"}, v), nil
			}
		}

		// Churn: create a fresh indexed Person, and periodically delete one, so the
		// bound indexes self-maintain on insert and delete.
		churn++
		create := fmt.Sprintf("CREATE (:Person {name:'q%d', age:%d, city:'c%d'})", churn, churn%500, churn%100)
		sm.execute(ctx, Op{Kind: OpCreate, Cypher: create})
		if sm.seed.Float64() < 0.3 {
			del := fmt.Sprintf("MATCH (n:Person {name:'q%d'}) DETACH DELETE n", churn-1)
			sm.execute(ctx, Op{Kind: OpDelete, Cypher: del})
		}

		if tick%40 == 0 {
			if v := CheckIndexConsistency(tick, nil, sm.engine, indexDiversitySpecs...); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<periodic index check>"}, v), nil
			}
		}
	}
	// Terminal consistency check.
	if v := CheckIndexConsistency(int64(cfg.MaxTicks), nil, sm.engine, indexDiversitySpecs...); len(v) > 0 {
		return sm.report(int64(cfg.MaxTicks), Op{Kind: OpMatch, Cypher: "<terminal index check>"}, v), nil
	}
	return nil, nil
}
