package sim

import (
	"context"
	"fmt"
)

// shortestPathPairs bounds how many (source, target) Person pairs
// [CheckShortestPath] probes per check. Pairs are selected deterministically
// from the sorted name set, so the probe is reproducible and bounded regardless
// of graph size.
const shortestPathPairs = 16

// CheckShortestPath validates the Cypher-level shortestPath() operator against
// an INDEPENDENT breadth-first shortest-path computed directly from the oracle's
// KNOWS edge set. For a deterministic, bounded set of (a,b) Person pairs it runs
// `MATCH p=shortestPath((a)-[:KNOWS*]->(b)) RETURN length(p)` and asserts the
// engine's hop count equals the BFS distance — or that both agree there is no
// path. It compares the path-LENGTH invariant, never a specific witness path
// (shortest paths are not unique). It runs on a quiescent graph, including
// immediately after crash/recovery, so the operator is validated against a
// WAL-recovered graph too.
func CheckShortestPath(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	names := oracle.NodeNames()
	if len(names) < 2 {
		return nil
	}
	// Build a name-keyed adjacency over the oracle's directed KNOWS edges, the
	// independent reference the engine is checked against.
	adj := make(map[string][]string, len(names))
	for k := range oracle.edges {
		if k.label != "KNOWS" {
			continue
		}
		s, d := oracle.nameOf(k.src), oracle.nameOf(k.dst)
		if s != "" && d != "" {
			adj[s] = append(adj[s], d)
		}
	}

	var vs []Violation
	ctx := context.Background()
	n := len(names)
	for i := 0; i < shortestPathPairs && i < n; i++ {
		a := names[i]
		b := names[(i*7+3)%n] // deterministic, well-spread target
		if a == b {
			continue
		}
		want := bfsHops(adj, a, b) // -1 == unreachable

		q := fmt.Sprintf(
			"MATCH p=shortestPath((a:Person {name:'%s'})-[:KNOWS*]->(b:Person {name:'%s'})) RETURN length(p)",
			a, b)
		res, err := engine.Run(ctx, q, nil)
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "shortestPath",
				Message: fmt.Sprintf("shortestPath(%q->%q) query error: %v", a, b, err)})
			continue
		}
		var got int64 = -1
		if res.Next() {
			if v, ok := res.IntAt(0); ok {
				got = v
			}
		}
		drainErr := res.Err()
		_ = res.Close()
		if drainErr != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "shortestPath",
				Message: fmt.Sprintf("shortestPath(%q->%q) drain error: %v", a, b, drainErr)})
			continue
		}
		if got != want {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "shortestPath length",
				Message: fmt.Sprintf("shortestPath(%q->%q): engine length=%d, BFS reference=%d", a, b, got, want)})
		}
	}
	return vs
}

// bfsHops returns the minimum number of directed KNOWS hops from src to dst over
// adj, or -1 when dst is unreachable. It is the independent reference for the
// Cypher shortestPath operator.
func bfsHops(adj map[string][]string, src, dst string) int64 {
	if src == dst {
		return 0
	}
	visited := map[string]bool{src: true}
	frontier := []string{src}
	var hops int64
	for len(frontier) > 0 {
		hops++
		var next []string
		for _, u := range frontier {
			for _, v := range adj[u] {
				if v == dst {
					return hops
				}
				if !visited[v] {
					visited[v] = true
					next = append(next, v)
				}
			}
		}
		frontier = next
	}
	return -1
}

// cypherPathsScenario verifies the Cypher-level shortestPath() operator under the
// DST: a write-heavy Person/KNOWS workload builds a graph, and [CheckShortestPath]
// compares the operator's hop count to an independent BFS reference periodically,
// at the end, and immediately after each crash/recovery. It is bit-reproducible.
func cypherPathsScenario() Scenario {
	return Scenario{
		Name:        ScenarioCypherPaths,
		Description: "Cypher shortestPath() hop count vs independent BFS reference + survives crash/recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x5409A78,
		MaxTicks:    600,
		Workload:    WriteHeavyWorkload,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 100.0, StabilityWindow: 25},
		run:         runCypherPaths,
	}
}

// cypherPathsCheckEvery is the tick cadence for the periodic shortestPath check.
const cypherPathsCheckEvery = 100

// runCypherPaths drives the shortestPath safety loop: it builds a Person/KNOWS
// graph and runs [CheckShortestPath] periodically, after every crash/recovery,
// and once at the end. It is deterministic.
func runCypherPaths(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := cypherPathsScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: cypher-paths new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	var lastTick int64
	var lastOp Op
	for i := 0; i < cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := sm.clock.Tick()

		crashesBefore := sm.crashCount
		if report, err := sm.maybeCrash(ctx, tick); err != nil {
			return nil, err
		} else if report != nil {
			return report, nil
		}
		if sm.crashCount > crashesBefore {
			if v := CheckShortestPath(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery shortestPath>"}, v), nil
			}
		}

		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed := sm.execute(ctx, op)
		sm.applyToOracle(op, committed)
		lastTick, lastOp = tick, op

		if tick%int64(sm.cfg.CheckEvery) == 0 {
			if v := sm.checker.Check(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
		if tick%cypherPathsCheckEvery == 0 {
			if v := CheckShortestPath(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
	}
	if v := CheckShortestPath(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	return nil, nil
}
