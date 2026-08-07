package mvccwrite

// frontier_staleness_test.go — how stale is a new reader, and how much of that is the
// CONTIGUOUS frontier rather than commit latency (rmp #2343)?
//
// # The trade being measured
//
// A reader starts at the contiguous commit frontier: the newest instant below which
// nothing is still in flight (graph/mvcc/mvcc.go ReadTS, graph/mvcc/commitlog.go). On
// the durable path the commit timestamp is allocated BEFORE the WAL fsync, because
// the OpCommit marker must CARRY it — store/txn/txn.go:1557 encodes it into the frame
// that store/txn/txn.go:1387 then fsyncs, and recovery derives the clock floor from
// those records rather than trusting a persisted counter (rmp #2309). So an fsync sits
// inside every allocate-to-publish window, and ONE slow committer holds back the
// visibility of every commit allocated after it, however long those have been
// published.
//
// That is a knowingly accepted trade, taken from Memgraph. It had never been measured
// as reader staleness under load. This measures it.
//
// # What is measured, and how the frontier's share is separated
//
// For each marker commit: the time from its Commit returning (the client has been told
// SUCCESS) to the first moment a NEW reader can see it. That is the quantity a client
// experiences, and it is reported as a DISTRIBUTION, because a mean over a
// convoy-shaped delay says almost nothing.
//
// The SINGLE-WRITER arm is the control: with one writer there is no earlier in-flight
// commit to hold the frontier back, so whatever lag remains is inherent — the read
// path's own cost plus the polling granularity. The frontier's share at N writers is
// the excess over that arm. Without the control the whole figure would be attributed
// to the frontier, which is the error this design exists to avoid.
//
// It is an INSTRUMENT, not a gate: no threshold is asserted.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// stalenessSamples is how many marker commits each arm measures.
const stalenessSamples = 200

// TestFrontierStaleness measures ack-to-visible lag at several writer counts and
// reports the distribution for each.
//
// It runs on the WAL wiring, because the fsync inside the allocate-to-publish window
// is the whole mechanism under study; a store-less arm would measure a window with no
// fsync in it and answer a different question.
func TestFrontierStaleness(t *testing.T) {
	testlayers.RequireSoak(t)

	for _, writers := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprintf("writers=%d", writers), func(t *testing.T) {
			r := newRig(t, wiringWAL)
			defer func() {
				if err := r.close(); err != nil {
					t.Errorf("close rig: %v", err)
				}
			}()
			ctx := context.Background()
			warmUp(t, r.eng)

			// Background writers, on their own key space so they never conflict with
			// the marker committer. They exist only to keep commits in flight, which
			// is what holds the frontier back.
			var (
				stop = make(chan struct{})
				wg   sync.WaitGroup
				bg   atomic.Int64
			)
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						default:
						}
						if err := commit(ctx, r.eng, 1<<30+w, i); err == nil {
							bg.Add(1)
						}
					}
				}(w)
			}

			lags := make([]time.Duration, 0, stalenessSamples)
			for s := 0; s < stalenessSamples; s++ {
				seq := int64(s)
				res, err := r.eng.RunInTx(ctx, "CREATE (n:Mark {seq: $s})",
					map[string]expr.Value{"s": expr.IntegerValue(seq)})
				if err != nil {
					close(stop)
					wg.Wait()
					t.Fatalf("marker %d commit: %v", s, err)
				}
				// ACKNOWLEDGED here: the client has been told SUCCESS. Everything
				// after this is staleness the client can observe.
				acked := time.Now()
				if err := res.Close(); err != nil {
					close(stop)
					wg.Wait()
					t.Fatalf("marker %d close: %v", s, err)
				}
				lags = append(lags, waitVisible(t, ctx, r.eng, seq, acked))
			}

			close(stop)
			wg.Wait()

			if writers > 1 && bg.Load() == 0 {
				t.Fatalf("%d background writers landed ZERO commits: this arm measured an "+
					"idle clock, and its staleness figure is not a figure for a loaded one",
					writers)
			}
			report(t, writers, bg.Load(), lags)
		})
	}
}

// waitVisible polls a FRESH reader until seq is visible and returns the lag from acked.
//
// Each poll is a separate Engine.Run, so each takes its own snapshot at the frontier
// as it stands then — which is exactly the quantity under study. A single long-lived
// reader would pin one instant and never observe the advance.
func waitVisible(t *testing.T, ctx context.Context, eng *cypher.Engine, seq int64, acked time.Time) time.Duration { //nolint:revive // t first, ctx follows
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := eng.Run(ctx, "MATCH (n:Mark {seq: $s}) RETURN count(n) AS c",
			map[string]expr.Value{"s": expr.IntegerValue(seq)})
		if err != nil {
			t.Fatalf("visibility probe for seq %d: %v", seq, err)
		}
		seen := false
		if res.Next() {
			if v, ok := res.Record()["c"].(expr.IntegerValue); ok && int64(v) > 0 {
				seen = true
			}
		}
		_ = res.Close()
		if seen {
			return time.Since(acked)
		}
		if time.Now().After(deadline) {
			t.Fatalf("seq %d never became visible within 30s of its acknowledgement — that "+
				"is a permanent frontier stall, not staleness", seq)
		}
	}
}

// report prints the distribution. A mean is deliberately NOT the headline: the
// mechanism produces a convoy-shaped delay, and a mean over one would hide the tail
// that is the whole point.
func report(t *testing.T, writers int, background int64, lags []time.Duration) {
	t.Helper()
	if len(lags) == 0 {
		t.Fatal("no samples")
	}
	sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
	at := func(q float64) time.Duration {
		i := int(q * float64(len(lags)-1))
		return lags[i]
	}
	t.Logf("writers=%d background_commits=%d samples=%d  p50=%v p90=%v p99=%v max=%v min=%v",
		writers, background, len(lags), at(0.50), at(0.90), at(0.99), lags[len(lags)-1], lags[0])
}
