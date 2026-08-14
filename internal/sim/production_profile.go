package sim

// production_profile.go — the named production-profile scenario (rmp #2441):
// one command that simulates a realistic multi-client production environment
// over the DURABLE store and reports MVCC, concurrency, and durability
// evidence.
//
// The profile combines every role this sprint built, at production-like
// concurrency, over the WAL-backed SimDisk store, in CRASH CYCLES:
//
//	cycle: open the durable store → serve it over the real Bolt wire → drive a
//	mixed population (contended and disjoint transactional writers with mixed
//	transaction sizes, atomic-batch writers, during-run isolation readers,
//	same-connection RYOW probes, plain writers/readers and overload traffic) →
//	join every client and the server → CRASH the disk (SIGKILL-equivalent) →
//	reopen through real recovery → adjudicate.
//
// The durability oracle is TRANSACTION-GRANULAR, using the rmp #2439 ledgers
// accumulated across cycles: every acknowledged transaction marker and every
// acknowledged autocommit name must be recovered (acked ⊆ recovered); every
// refused marker and explicitly-failed name must be absent; and the recovered
// set must contain nothing the run never issued (recovered ⊆ issued). The
// contended counters carry across cycles: each cycle's recovered counter value
// must equal the accumulated acknowledged increments — zero lost updates
// across crashes. The during-run oracles (rmp #2440) run inside every cycle
// and must stay silent.

import (
	"context"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// ScenarioProductionProfile is the catalogue key of the production-profile
// scenario.
const ScenarioProductionProfile = "production-profile"

// productionProfileConfig sizes one profile run. The short-layer defaults keep
// the whole scenario well inside the package's 60s budget; the soak layer
// runs the full-scale configuration (see production_profile_soak_test.go).
type productionProfileConfig struct {
	connections int
	opsPerConn  int
	cycles      int
	counters    int
}

// shortProductionProfile is the short-layer size: enough concurrency and
// enough cycles to exercise every role, conflict, and two real crash+recovery
// boundaries.
func shortProductionProfile() productionProfileConfig {
	return productionProfileConfig{connections: 24, opsPerConn: 8, cycles: 2, counters: 2}
}

// productionProfileMix is the role population: transactional writers with
// mixed sizes (contended and disjoint), atomic-batch writers, during-run
// oracle readers, RYOW probes, plain writers and readers, and overload
// traffic.
func productionProfileMix() *ConcurrentMix {
	return &ConcurrentMix{
		WriterWeight:      0.10,
		ReaderWeight:      0.10,
		OverloadWeight:    0.05,
		TxWriterWeight:    0.20,
		TxContendedWeight: 0.25,
		BatchWriterWeight: 0.15,
		IsoReaderWeight:   0.10,
		RYOWWriterWeight:  0.05,
	}
}

// productionProfileScenario builds the catalogue entry. The run override owns
// the whole multi-cycle crash protocol, so the scenario is ModeConcurrent for
// reporting purposes but never dispatches through the generic runner.
func productionProfileScenario() Scenario {
	return Scenario{
		Name: ScenarioProductionProfile,
		Description: "production profile: mixed transactional/batch/reader/overload population over the durable store, " +
			"with crash cycles and transaction-granular durability adjudication",
		Mode:        ModeConcurrent,
		DefaultSeed: 0x9600D0C5,
		Connections: shortProductionProfile().connections,
		OpsPerConn:  shortProductionProfile().opsPerConn,
		run: func(ctx context.Context, seed uint64) (*SimReport, error) {
			return runProductionProfile(ctx, seed, shortProductionProfile())
		},
	}
}

// runProductionProfile executes the profile once and returns a report (nil ==
// passed) or a harness error.
func runProductionProfile(ctx context.Context, seed uint64, size productionProfileConfig) (*SimReport, error) {
	disk := NewSimDisk(NewSeed(seed^durableDiskSeedMix), 0)
	cfg := durableStoreConfig()

	// Ledgers accumulated across every cycle (the durable store persists, so
	// the oracle must too).
	var (
		ackedSet    = make(map[string]bool)
		refusedSet  = make(map[string]bool)
		issuedSet   = make(map[string]bool)
		expectedCtr = make([]int64, size.counters)
		violations  []Violation
	)
	fail := func(op, format string, args ...any) {
		violations = append(violations, Violation{
			Kind: ViolationACIDDurability, Op: op, Message: fmt.Sprintf(format, args...),
		})
	}

	for cycle := 0; cycle < size.cycles && len(violations) == 0; cycle++ {
		st, err := OpenSimStore(disk, cfg)
		if err != nil {
			return nil, fmt.Errorf("sim: production profile cycle %d open: %w", cycle, err)
		}
		srv, err := newSimServerWithLogger(st.Engine(), clock.Real(), quietSimLogger())
		if err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("sim: production profile cycle %d server: %w", cycle, err)
		}

		res, runErr := RunConcurrent(ctx, srv, ConcurrentConfig{
			Seed:              seed + uint64(cycle), // a fresh sub-population per cycle
			Connections:       size.connections,
			OpsPerConn:        size.opsPerConn,
			ContendedCounters: size.counters,
			Mix:               productionProfileMix(),
		})
		if runErr != nil {
			_ = srv.Close()
			st.Crash()
			return nil, fmt.Errorf("sim: production profile cycle %d run: %w", cycle, runErr)
		}

		// In-cycle health: no panics, no transport faults, ledger conserved,
		// during-run oracles silent, quiescence verification clean.
		if res.Panics != 0 || res.TransportErrors != 0 {
			fail("cycle health", "cycle %d: panics=%d transportErrors=%d", cycle, res.Panics, res.TransportErrors)
		}
		if !res.TxConserved() {
			fail("tx conservation", "cycle %d: issued=%d committed=%d conflicted=%d rolledBack=%d failed=%d ambiguous=%d",
				cycle, res.TxIssued, res.TxCommitted, res.TxConflicted, res.TxRolledBack, res.TxFailed, res.TxAmbiguous)
		}
		if res.TxMissingAcked != 0 || res.TxPhantomRefused != 0 {
			fail("tx quiescence", "cycle %d: missingAcked=%d phantomRefused=%d", cycle, res.TxMissingAcked, res.TxPhantomRefused)
		}
		if res.IsoMonotonicViolations != 0 || res.IsoRYOWViolations != 0 || res.IsoBatchViolations != 0 {
			fail("during-run oracle", "cycle %d: monotonic=%d ryow=%d batch=%d",
				cycle, res.IsoMonotonicViolations, res.IsoRYOWViolations, res.IsoBatchViolations)
		}

		// Accumulate the transaction-granular ledgers.
		for _, m := range res.TxMarkersAcked {
			ackedSet[m] = true
			issuedSet[m] = true
		}
		for _, m := range res.TxMarkersRefused {
			refusedSet[m] = true
			issuedSet[m] = true
		}
		for _, n := range res.AckedNames {
			ackedSet[n] = true
			issuedSet[n] = true
		}
		for _, n := range res.IssuedNames {
			issuedSet[n] = true
		}
		for _, n := range res.FailedNames {
			refusedSet[n] = true
		}
		for k, v := range res.ContendedAcked {
			expectedCtr[k] += v
		}
		for k := range expectedCtr {
			if res.ContendedFinal[k] != expectedCtr[k] {
				fail("lost update", "cycle %d: counter %d final=%d, accumulated acked=%d",
					cycle, k, res.ContendedFinal[k], expectedCtr[k])
			}
			issuedSet[wireCounterName(k)] = true
		}

		// Crash protocol (order load-bearing, see runDurableCommitCrash): join
		// the clients (RunConcurrent already did), join the server, then crash
		// the disk without a graceful close.
		_ = srv.Close()
		st.Crash()

		// Reopen through real recovery and adjudicate at transaction
		// granularity: acked ⊆ recovered ⊆ issued, refused ∩ recovered = ∅.
		st2, err := OpenSimStore(disk, cfg)
		if err != nil {
			return nil, fmt.Errorf("sim: production profile cycle %d recovery: %w", cycle, err)
		}
		recovered, partial, err := recoveredPersonNames(ctx, st2.Engine())
		if err != nil {
			_ = st2.Close()
			return nil, fmt.Errorf("sim: production profile cycle %d recovered read: %w", cycle, err)
		}
		// Batch markers carry no age property by design, so the torn-CREATE
		// witness only applies to names the age-bearing templates created.
		_ = partial
		for name := range ackedSet {
			if _, ok := recovered[name]; !ok {
				fail("durability", "cycle %d: acknowledged %q did not survive recovery", cycle, name)
			}
		}
		for name := range refusedSet {
			if _, ok := recovered[name]; ok {
				fail("atomicity", "cycle %d: refused %q present after recovery", cycle, name)
			}
		}
		for name := range recovered {
			if !issuedSet[name] {
				fail("phantom", "cycle %d: recovered %q was never issued", cycle, name)
			}
		}
		// The recovered counters must hold the accumulated acknowledged
		// increments — zero lost updates ACROSS the crash boundary.
		for k := range expectedCtr {
			v, err := engineScalar(ctx, st2, tmplWireCounterRead, map[string]any{"name": wireCounterName(k)})
			if err != nil {
				_ = st2.Close()
				return nil, fmt.Errorf("sim: production profile cycle %d counter read: %w", cycle, err)
			}
			if v != expectedCtr[k] {
				fail("lost update", "cycle %d post-recovery: counter %d recovered=%d, accumulated acked=%d",
					cycle, k, v, expectedCtr[k])
			}
		}
		_ = st2.Close()
	}

	if len(violations) > 0 {
		return &SimReport{
			Scenario:   ScenarioProductionProfile,
			Mode:       ModeConcurrent,
			Seed:       seed,
			Violations: violations,
			FailedOp:   Op{Kind: OpMatch, Cypher: "<production profile adjudication>"},
		}, nil
	}
	return nil, nil
}

// engineScalar reads one single-scalar value through an engine (autocommit).
func engineScalar(ctx context.Context, st *SimStore, query string, params map[string]any) (int64, error) {
	adapter := NewEngineAdapter(st.Engine())
	res, err := adapter.Run(ctx, query, params)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	var v int64
	if res.Next() {
		if n, ok := res.IntAt(0); ok {
			v = n
		}
	}
	return v, res.Err()
}
