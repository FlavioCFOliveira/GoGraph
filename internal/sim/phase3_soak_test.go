package sim

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestPhase3_ConcurrentLivenessSoak is the soak-layer Phase-3 endurance run: for
// several seeds it drives a large concurrent safety phase (the full actor
// population — honest writers/readers, the bounded overload actor, a burst of
// wire abuse, and a slow consumer — all against one real Bolt server) and then a
// liveness phase that must converge. It asserts, across the whole run, the
// Phase-3 contract: no panic, no goroutine leak, bounded resources, and eventual
// oracle==engine consistency at quiescence.
//
// It is gated to the soak layer because the concurrent connection count and
// per-connection work are scaled up well beyond the short-layer integration
// tests, so a single run is minutes-long. The short layer covers every one of
// these code paths at a smaller scale (the per-actor tests and the end-to-end
// liveness test), so this adds endurance coverage, not new behaviour.
func TestPhase3_ConcurrentLivenessSoak(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)

	seeds := []uint64{0x501, 0x502, 0x503, 0x504}
	for _, seed := range seeds {
		seed := seed
		t.Run(seedName(seed), func(t *testing.T) {
			srv, err := NewSimServer(SimEngineForServer(), clock.Real())
			if err != nil {
				t.Fatalf("NewSimServer: %v", err)
			}
			defer func() { _ = srv.Close() }()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			// Safety phase: large concurrent mixed workload (writers/readers/overload).
			safety, err := RunConcurrent(ctx, srv, ConcurrentConfig{
				Seed:        seed,
				Connections: 64,
				OpsPerConn:  200,
				Mix:         &ConcurrentMix{WriterWeight: 0.5, ReaderWeight: 0.35, OverloadWeight: 0.15},
			})
			if err != nil {
				t.Fatalf("seed %d safety phase: %v", seed, err)
			}
			if safety.Panics != 0 {
				t.Fatalf("seed %d: %d panics in the safety phase", seed, safety.Panics)
			}
			if safety.TransportErrors != 0 {
				t.Fatalf("seed %d: %d unexpected transport errors in the safety phase", seed, safety.TransportErrors)
			}
			if !safety.Consistent() {
				t.Fatalf("seed %d safety phase inconsistent: engine=%d acked=%d",
					seed, safety.EngineNodeCount, safety.AckedCreates)
			}

			// Interleave a burst of wire abuse + a slow consumer against the same
			// server; both must leave it healthy (these run on their own connections
			// and do not perturb the acked-create oracle).
			abuser := BoltAbuser{}
			for fam := AbuseFamily(0); fam < abuseFamilyCount; fam++ {
				out, aerr := abuser.Abuse(srv, fam)
				if aerr != nil {
					t.Fatalf("seed %d abuse(%s): %v", seed, fam, aerr)
				}
				if !out.Acceptable() {
					t.Errorf("seed %d abuse(%s): unacceptable %+v", seed, fam, out)
				}
			}
			sc := NewSlowConsumer(clock.Real())
			if _, serr := sc.Stall(ctx, srv, 50*time.Millisecond, nil); serr != nil {
				t.Fatalf("seed %d slow consumer: %v", seed, serr)
			}

			// Liveness phase: faults healed, honest workload must converge.
			live, err := RunLiveness(ctx, srv, clock.Real(), LivenessConfig{
				Seed:           seed ^ 0x9E3779B9,
				Connections:    32,
				OpsPerConn:     50,
				ConvergeBudget: 30 * time.Second,
			})
			if err != nil {
				t.Fatalf("seed %d liveness phase: %v", seed, err)
			}
			if !live.Converged {
				t.Fatalf("seed %d did not converge:\n%s", seed, live.Report())
			}
		})
	}
}

// seedName renders a seed as a stable subtest name.
func seedName(seed uint64) string {
	return "seed_" + itoa64(seed)
}

// itoa64 renders a uint64 in base 10 without importing strconv into the test for
// one call.
func itoa64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
