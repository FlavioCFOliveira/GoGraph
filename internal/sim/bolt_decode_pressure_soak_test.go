//go:build soak || nightly

package sim

// bolt_decode_pressure_soak_test.go — the long-running arms of the Bolt
// inbound-decode pressure surface (rmp #2487).
//
// The short layer proves each clause works and that it can fail. Three things it
// structurally cannot prove:
//
//   - that the harness's closed-form model of the pool's arithmetic is right at
//     ONE ceiling by coincidence. The short arm drives a single 4 MiB ceiling; a
//     model with a compensating error in two terms would name that one boundary
//     correctly and miss every other. The ceiling sweep below moves the ceiling
//     across three orders of magnitude and requires the model to name the last
//     admitted element count EXACTLY at each.
//   - that the pool is leak-free over a LONG-LIVED server. Every short-layer run
//     builds a fresh server, so a leak of a few bytes per message — the shape a
//     mispaired reserve/release actually has — would need thousands of messages
//     to become visible and is invisible by construction to a run that sends
//     twenty. The endurance arm drives one server through thousands and then asks
//     for the whole ceiling back.
//   - that the swarm's overlap construction holds across seeds rather than at the
//     three the short layer samples.
//
// Runs under the soak layer only (docs/test-layers.md).

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestBoltDecodePressure_SoakSeedSweep runs the deterministic scenario across
// many seeds. Both adjudicators must stay clean at every one.
func TestBoltDecodePressure_SoakSeedSweep(t *testing.T) {
	defer goleak.VerifyNone(t)
	const seeds = 300
	for i := uint64(0); i < seeds; i++ {
		seed := 0x2487_0000 + i
		ctx, cancel := context.WithTimeout(context.Background(), boltDecodeTestTimeout)
		ev, err := RunBoltDecodePressure(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("seed %#x: %v", seed, err)
		}
		v := append(checkBoltDecodePressure(ev), checkBoltDecodePressureNonVacuity(ev)...)
		if len(v) > 0 {
			t.Fatalf("seed %#x:\n%s%s", seed, ev, renderViolations(v))
		}
	}
}

// TestBoltDecodeSwarm_SoakSeedSweep runs the concurrent scenario across many
// seeds. It is the arm that establishes the overlap construction's margin is a
// property of the design: the short layer samples three seeds, which cannot tell
// a robust construction from a lucky one.
func TestBoltDecodeSwarm_SoakSeedSweep(t *testing.T) {
	defer goleak.VerifyNone(t)
	const seeds = 100
	worstWide := -1
	for i := uint64(0); i < seeds; i++ {
		seed := 0x2487_A000 + i
		ctx, cancel := context.WithTimeout(context.Background(), boltDecodeTestTimeout)
		ev, err := RunBoltDecodeSwarm(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("seed %#x: %v", seed, err)
		}
		v := append(checkBoltDecodePressure(ev), checkBoltDecodePressureNonVacuity(ev)...)
		if len(v) > 0 {
			t.Fatalf("seed %#x:\n%s%s", seed, ev, renderViolations(v))
		}
		for j := range ev.Honest {
			h := &ev.Honest[j]
			if !h.Wide {
				continue
			}
			if n := int(h.RejectionsAfter - h.RejectionsBefore); worstWide < 0 || n < worstWide {
				worstWide = n
			}
		}
	}
	// The MARGIN is the point of the sweep, not the pass. A run of 100 seeds whose
	// narrowest wide window ever held a single refusal is one scheduling decision
	// from a red short layer, and only a sweep can see that coming.
	t.Logf("across %d seeds the NARROWEST wide honest window held %d refusals (hold %s)",
		seeds, worstWide, boltDecodeSwarmWideHold)
	if worstWide <= 1 {
		t.Errorf("the narrowest wide honest window across %d seeds held only %d refusal(s). The overlap "+
			"clause is meant to rest on a window wider than the interval between refusals; at this "+
			"margin it rests on luck, and boltDecodeSwarmWideHold needs raising", seeds, worstWide)
	}
}

// TestBoltDecodePool_SoakCeilingSweep requires the harness's closed-form model of
// the pool to name the exact admission boundary at ceilings spanning three orders
// of magnitude.
//
// One ceiling can be named right by a model with compensating errors. Nine cannot:
// the fixed term, the per-element term and the string terms all scale differently
// with the ceiling, so agreement at every one of these pins each independently.
func TestBoltDecodePool_SoakCeilingSweep(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	ceilings := []int64{
		2 << 20, 3 << 20, 4 << 20, 5<<20 + 1, 6 << 20, 8 << 20, 12<<20 + 7, 16 << 20, 32 << 20,
	}
	for _, ceiling := range ceilings {
		srv, err := NewSimServerInboundBudget(SimEngineForServer(), clock.Real(), ceiling)
		if err != nil {
			t.Fatalf("ceiling %d: %v", ceiling, err)
		}
		c, err := srv.Dial()
		if err != nil {
			t.Fatalf("ceiling %d dial: %v", ceiling, err)
		}
		if err := c.Connect(ctx); err != nil {
			t.Fatalf("ceiling %d connect: %v", ceiling, err)
		}
		want := boltDecodeModelElementsFor(boltDecodeProbeQuery, boltDecodeParamKey, ceiling)
		for _, d := range []int{-1, 0, 1} {
			p, err := boltDecodeSendProbe(c, boltDecodeProbeQuery, want+d, ceiling)
			if err != nil {
				t.Fatalf("ceiling %d probe %+d: %v", ceiling, d, err)
			}
			if wantAccept := d <= 0; p.Accepted != wantAccept {
				t.Errorf("ceiling %d: n=%d (model hold %d B, slack %+d B) was accepted=%v, want %v. "+
					"The model names the boundary at n=%d; a disagreement here means a packstream "+
					"per-slot cost changed and boltDecodeModelHeld must be re-derived",
					ceiling, p.Elements, p.ModelHeld, p.Slack, p.Accepted, wantAccept, want)
			}
		}
		_ = c.Close()
		_ = srv.Close()
	}
}

// TestBoltDecodePool_SoakEnduranceNoLeak drives ONE server through thousands of
// alternating accepted and refused decodes and then asks for the whole ceiling
// back.
//
// This is the leak arm the short layer cannot write. A reserve/release pair that
// is mismatched by a few bytes per message leaks nothing a twenty-message run can
// see; after this many it has consumed a measurable fraction of the pool, and the
// boundary-sized probe at the end is refused. The probe's slack is under one
// element's charge, so the arm detects a leak averaging a fraction of a byte per
// message.
func TestBoltDecodePool_SoakEnduranceNoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	const ceiling int64 = 4 << 20
	const rounds = 4000

	srv, err := NewSimServerInboundBudget(SimEngineForServer(), clock.Real(), ceiling)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := c.Conn().SetReadDeadline(time.Now().Add(10 * time.Minute)); err != nil {
		t.Fatalf("deadline: %v", err)
	}

	boundary := boltDecodeModelElementsFor(boltDecodeProbeQuery, boltDecodeParamKey, ceiling)
	served, refused := 0, 0
	for i := range rounds {
		// Alternate a message that fits with one that does not, so BOTH the release
		// path after a successful decode and the release path after a failed one are
		// exercised the same number of times. A leak on either alone would be missed
		// by a run that only drove the other.
		n := boundary / 2
		if i%2 == 1 {
			n = boundary + 1 + i%97
		}
		p, err := boltDecodeSendProbe(c, boltDecodeProbeQuery, n, ceiling)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if p.Accepted {
			served++
		} else {
			refused++
		}
	}
	t.Logf("endurance: %d served, %d refused against a %d B ceiling", served, refused, ceiling)
	if served == 0 || refused == 0 {
		t.Fatalf("the endurance loop drove %d served and %d refused decodes: both paths must run for the "+
			"probe below to be a statement about either", served, refused)
	}

	final, err := boltDecodeSendProbe(c, boltDecodeProbeQuery, boundary, ceiling)
	if err != nil {
		t.Fatalf("final probe: %v", err)
	}
	if !final.Accepted {
		t.Fatalf("after %d decodes the pool would no longer admit a boundary-sized message (n=%d, model "+
			"hold %d B, slack %+d B, refused %s): it leaked at least %d bytes, i.e. an average of %.4f "+
			"bytes per message",
			rounds, final.Elements, final.ModelHeld, final.Slack, final.Code,
			final.Slack+1, float64(final.Slack+1)/float64(rounds))
	}
}
