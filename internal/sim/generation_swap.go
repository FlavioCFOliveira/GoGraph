package sim

// generation_swap.go — rmp #2491: the DST scenario for graph/generation, the
// lock-free CSR publisher with a refcount lifecycle.
//
// # What this scenario adjudicates
//
// graph/generation publishes an immutable [csr.CSR] under an
// [atomic.Pointer] and keeps every superseded generation alive until its
// refcount drains to zero. Its failure modes are lifecycle failure modes —
// a refcount that leaks, a generation recycled while a reader still holds
// it, a drain that never completes, a Close that wedges — and none of them
// is visible in the ANSWER a reader gets. A reader handed the wrong
// generation still returns a well-formed graph; it simply returns the wrong
// one. That is why the oracle here is an IDENTITY oracle, not a
// well-formedness one.
//
// # Why the identity oracle is self-locating
//
// Every generation the plan builds is a DIFFERENT graph, and it carries its
// own publish sequence number INSIDE its content: node [genSwapMarkerNode]
// (0) has exactly one out-neighbour, and that neighbour is 1+seq. So a
// reader can decode, from the artefact alone, which generation it believes
// it is holding, and then compare the WHOLE traversal against the model's
// independently-computed fingerprint for that sequence number. Three
// independent identity channels must agree:
//
//	(1) the content's marker  → the sequence number the artefact declares;
//	(2) the model's plan row  → the adjacency that sequence number must have;
//	(3) the generation POINTER → the sequence number the publisher recorded
//	    for that pointer (checked terminally, see below).
//
// A reader that is served generation k+1's adjacency while holding
// generation k fails (2) against (1); a generation whose content changed
// under a stable pointer fails the per-reader pointer-stability clause; a
// generation the publisher never published fails (3).
//
// The package's own csr_rotation_consistency_test.go cannot reach any of
// this. Its makeCSR builds a graph with exactly ONE edge for every seed, so
// its "Size() must equal 1" oracle holds for every generation that ever
// existed and cannot distinguish one from another. It is a well-formedness
// check wearing a torn-swap name.
//
// The fingerprint is computed by the MODEL from its own adjacency map and
// by the READER from the engine's read path ([csr.CSR.NeighboursByID]).
// Neither side asks the engine what the answer should be, so this is an
// independent-reference check, not the engine validating itself.
//
// # Why the refcount clauses cannot be flaky
//
// [generation.Generation.Refcount] is explicitly documented as an
// observability counter that races with concurrent Acquire/Release, so
// "the refcount is N" is never a sound assertion. Every refcount clause
// here is therefore either a STRUCTURAL BOUND that holds at every instant,
// or is taken at a point where no goroutine can move the counter:
//
//   - FLOOR: a reader inside its own access window holds one reference, so
//     the count cannot be below 1. Any value below 1 is a lost increment or
//     a double decrement, never a scheduling artefact.
//   - CEILING: a reader holds at most ONE outstanding increment at a time
//     (Acquire's retry loop rolls its increment back through releaseRef
//     before retrying), and the publisher holds at most one hostage
//     reference, so the count can never exceed readers+1. Exceeding it is a
//     missing Release, never a scheduling artefact.
//   - AT REST: after every reader goroutine has been JOINED and the
//     publisher has stopped, no goroutine can touch any refcount at all, so
//     "every generation ever published has refcount 0" is a total, exact
//     assertion. This — not a poll during the run — is the sound place for
//     the task's "every superseded generation's refcount returns to zero".
//
// # Why the drain-timeout arm is not timing-thresholded
//
// The arm does not race a short duration against a drain. The PUBLISHER
// ITSELF acquires the generation it is about to supersede and holds that
// reference across the whole PublishWithDrain call. Reading
// PublishWithDrain, the wait loop is `for prev.refcount.Load() > 0`, so
// with a reference held for the entire call the loop condition is
// PERMANENTLY true and the only exit is the timeout branch. ErrDrainTimeout
// is therefore guaranteed for ANY positive timeout — 1ns or 1s — and the
// timeout value changes only how long the call takes, never its verdict.
// The arm additionally asserts that the hostage really was the captured
// predecessor, so a spurious timeout cannot be mistaken for the contract.
//
// The paired CONTROL is what makes the arm mean something: the plan forces
// the very next publish to be PublishWithDrain(c, 0) — an unbounded drain —
// which must return nil. Timeout-when-held and drain-when-free are then
// both measured, so neither can pass vacuously.
//
// # Determinism: exactly what is reproducible
//
// [ExecMode.Reproducible] is false for [ModeConcurrent] and this scenario
// does not pretend otherwise. Following the RunConcurrent precedent
// (concurrent.go:242-247), the WHOLE plan and every per-goroutine sub-seed
// are drawn up front on the single calling goroutine, in a fixed order,
// BEFORE any goroutine spawns. Consequences, stated exactly:
//
//   - REPRODUCIBLE from the seed alone: the generation count, every
//     generation's node count / edge count / adjacency / fingerprint, the
//     Publish-vs-drain-vs-drain-timeout op for every publish, the index of
//     the drain-timeout arm, and therefore [genSwapPlan.digest] — which the
//     report pins. Also reproducible: each reader's full-check decision
//     stream, since each reader draws from its own [Seed].
//   - REPRODUCIBLE from (seed, reader count): the reader sub-seed values.
//     They are drawn AFTER the plan precisely so that varying the reader
//     count cannot perturb the plan.
//   - NOT REPRODUCIBLE, and recorded as telemetry rather than asserted
//     against any threshold: how many acquisitions happened, which
//     generation each acquisition landed on, and the observed refcount
//     values. The interleaving is real.
//
// Every non-vacuity clause is therefore STRUCTURAL, never a rate: each
// reader's first acquisition is taken before the publisher is allowed to
// publish (so it must observe sequence 0) and its last is taken after the
// publisher has finished (so it must observe the final sequence). That
// makes "every reader straddled at least one swap" a guaranteed fact of the
// construction rather than a hope about scheduling.
//
// # What this scenario CANNOT detect — read before assuming coverage
//
// USE-AFTER-FREE is not reachable. Go's garbage collector keeps a *csr.CSR
// alive for exactly as long as a reader holds the pointer, so there is no
// freed memory to touch and no observable fault to catch. Detecting a true
// use-after-free would need a poisoned allocator or unsafe reinterpretation
// of released storage, and this scenario has neither.
//
// What IS reachable, and what the sentinel clauses actually cover, is
// USE-AFTER-RECYCLE: the modelled decision to reclaim a generation's
// backing storage. [genSwapSlot.freed] stands for that decision. The
// publisher may only set it once PublishWithDrain(c, 0) has RETURNED nil,
// which the library guarantees means the predecessor's refcount reached
// zero, and no reader can newly acquire it because it is no longer current
// (Acquire increments, re-checks current, and rolls back). A reader that
// ever finds `freed` set on a generation Acquire just handed it has caught
// a premature reclamation. The mirror clause is asserted from the publisher
// side on [genSwapSlot.inUse].
//
// CONCURRENT PUBLISHERS are also out of scope here, deliberately. The
// readers' monotonicity clause is only sound under a single publisher,
// because the plan allocates sequence numbers before the swap rather than
// under the library's publishMu. Concurrent publishers are covered by the
// package's own TestPublisher_ConcurrentPublishWithDrain_NoLostDrain, which
// reaches the unexported onDrain seam this scenario cannot see from here.
//
// # Which clauses were PROVEN able to fire, and how (rmp #2491, measured)
//
// Every clause below was provoked deliberately. The permanent injections
// live in generation_swap_test.go; the library mutations were applied to
// graph/generation/generation.go one at a time, run, and reverted, with the
// file verified byte-identical to its original afterwards.
//
// ELEVEN library mutations are recorded below, and THIS TABLE is the
// authoritative count: any prose elsewhere that disagrees with it is wrong.
// Five of the eleven were reproduced in rmp #2491's validation pass and
// carry the conditions they were measured under; the other six are inherited
// from the implementing session, were NOT re-run, and their sighting counts
// are unattributed. The split is kept because a count with no seed and no
// reader count behind it is not evidence.
//
// REPRODUCED (darwin/arm64, 10 logical cores, -race, catalogue seed
// 0x24915a17; the reader count is stated per row):
//
//	Release decrements twice          -> refcount-below-held-reference (312)
//	                                     + refcount-nonzero-at-rest (35) [8]
//	PublishWithDrain never waits      -> drain-completed-with-reader-inside (9)
//	                                     + held-drain-did-not-time-out (1)
//	                                     + nv-drain-timeout-arm-never-ran (1)
//	                                     + use-after-recycle (4) [8]
//	Publish does not store            -> current-not-the-published-generation
//	                                     (18) [8]; NOT accompanied by
//	                                     last-acquisition-not-final-generation
//	                                     at this seed — see the note below
//	Close does not clear current      -> acquire-after-close-returned-a-generation
//	                                     + current-after-close-is-not-nil
//	                                     + nv-close-arm-was-not-concurrent [2]
//	an unbounded drain gets a deadline-> unbounded-drain-did-not-complete (1)
//	                                     + last-acquisition-not-final-generation
//	                                     (8) + pointer-sequence-disagreement (8)
//	                                     + nv-generation-count-mismatch (1) [8]
//
// One HARNESS mutation was reproduced alongside them, and it is the one that
// shows the identity oracle is live inside the concurrent run rather than
// only inside the injection table: making the MODEL's own fingerprint
// diverge (an extra word folded into genSwapFingerprintModel) fired
// content-mismatch 438 times at 8 readers on the same seed.
//
// INHERITED from the implementing session and NOT re-run in the validation
// pass. The mutation -> clause mapping is recorded as claimed; the counts
// carry no seed and no reader count and are not reproducible as written:
//
//	Acquire's rollback dropped        -> publisher-did-not-finish
//	Release stops decrementing        -> refcount-above-holder-bound (40933
//	                                     sightings, UNATTRIBUTED)
//	                                     + publisher-did-not-finish
//	Acquire skips its re-check        -> use-after-recycle (PROBABILISTIC, see
//	                                     the sensitivity note below)
//	the zero-edge broadcast is lost   -> publisher-did-not-finish
//	a generation's csr is rewritten   -> generation-content-changed
//	                                     (8, UNATTRIBUTED)
//	                                     + pointer-sequence-disagreement
//	                                     (21, UNATTRIBUTED)
//	Publish refuses an open publisher -> publish-errored-on-an-open-publisher
//
// WHY ONE PAIRING IS CONDITIONAL. Dropping Publish's store does not always
// drag last-acquisition-not-final-generation in with it. The readers'
// terminal assertion fires only when the FINAL publish was a plain Publish;
// at the catalogue seed the plan's last op (sequence 36) is a drain, and
// PublishWithDrain still stores, so the final generation is correct and only
// current-not-the-published-generation fires. Whether the pairing appears is
// a property of the seed's op sequence, not of the mutation.
//
// SENSITIVITY NOTE, and the reason the wide-fleet arms exist. Dropping
// Acquire's re-check is the one mutation this oracle catches only
// probabilistically, because the window it opens — a reader incrementing a
// generation's refcount after that generation's drain already completed — is
// a few instructions wide. The figures below are INHERITED and were NOT
// reproduced in the validation pass; 5 seeds per width, one run each:
//
//	without -race, widths 2/8/64/256 : detected in  0 of 20 runs
//	with    -race, width 8           : detected in  0 of  5 runs
//	with    -race, widths 16/32/64/128: detected in 4 of 20 runs
//
// What those figures support, and nothing further: detection needs BOTH the
// race detector's added preemption and a fleet wider than the host's core
// count. They license NO detection rate for any particular width set — the
// only width they share with generation_swap_test.go's 2/8/32/64 fleet is 8,
// and at width 8 they detected nothing in 5 runs — so no rate is claimed
// here. What IS asserted is structural rather than statistical: the wide
// widths are exercised by the DEFAULT gate at all. rmp #2491's validation
// pass measured the wide-fleet arms at 0.46 s under -race (they had been
// gated behind the soak tag on a cost estimate that measurement refuted by
// three orders of magnitude), so they now live in the short layer as
// generation_swap_wide_test.go and every `make ci` run drives 64, 256 and
// 1024 readers plus a 64-seed geometry sweep instead of skipping them.
//
// GUARD CLAUSES PROVEN ONLY BY CONSTRUCTION, stated rather than implied. No
// single-point mutation produces them, because each asserts that a
// precondition the harness itself established still held:
// first-acquisition-not-seed-generation and
// acquire-returned-nil-on-an-open-publisher (the barrier guarantees them),
// timeout-arm-hostage-was-not-predecessor (the arm's own construction),
// publish-succeeded-after-errclosed (would need Close to un-close), and
// close-did-not-return / reader-did-not-join (a wedge the publisher guard
// reaches first). They are kept because dropping them would drop the
// assertion that the construction held at all.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/generation"
)

// ScenarioGenerationSwap is the catalogue key for the generation-swap
// scenario.
const ScenarioGenerationSwap = "generation-swap"

// generationSwapSeedMix decorrelates this scenario's draw stream from every
// other sub-seed in the package, per the convention in seed.go.
const generationSwapSeedMix uint64 = 0x2491_6E45_1701_C5A7

// genSwapCloseArmSeedMix decorrelates the close arm's reader decision
// streams from the swap arm's, so the two arms do not make identical
// full-check choices on identical sub-seeds.
const genSwapCloseArmSeedMix uint64 = 0x2491_C105_E5A7_1701

// generationSwapDefaultSeed is the catalogue default.
const generationSwapDefaultSeed uint64 = 0x2491_5A17

// genSwapMarkerNode is the node whose single out-neighbour encodes the
// generation's publish sequence number, so a generation's content names
// which generation it is. It carries no adjacency of the modelled graph:
// every modelled arc has a source and a destination >= 1.
const genSwapMarkerNode graph.NodeID = 0

// The plan geometry. Node counts are small on purpose: the value of this
// scenario is the number of INTERLEAVINGS it visits, not the size of the
// graphs, and a small graph keeps the O(V+E) full fingerprint cheap enough
// to run on most acquisitions inside the short layer's budget.
const (
	genSwapMinGenerations = 24
	genSwapMaxGenerations = 40
	genSwapMinNodes       = 48
	genSwapMaxNodes       = 96
	genSwapMaxOutDegree   = 3
)

// genSwapFullCheckProb is the per-acquisition probability that a reader
// verifies the WHOLE traversal fingerprint rather than only the O(1)
// identity and shape. Drawn from the reader's own Seed, so the decision
// stream is reproducible per reader.
const genSwapFullCheckProb = 0.25

// Workload defaults. MaxOpsPerReader is a COST CEILING, not a target: a
// reader normally stops as soon as the publisher is done (see
// [genSwapReader.run]).
const (
	genSwapDefaultReaders   = 8
	genSwapDefaultMinOps    = 8
	genSwapDefaultMaxOps    = 2048
	genSwapArm2MaxOps       = 1 << 16
	genSwapArm2Publishes    = 6
	genSwapDefaultDrainWait = time.Millisecond
	genSwapJoinDeadline     = 90 * time.Second
)

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

// GenerationSwapConfig parameterises one generation-swap run.
//
// # Concurrency contract
//
// A GenerationSwapConfig is a plain value read once by
// [RunGenerationSwap] before any goroutine spawns. It is not safe to mutate
// concurrently with a run, and a run mutates nothing in it.
type GenerationSwapConfig struct {
	// Seed is the master seed. The plan is a pure function of it; see the
	// determinism section in this file's header for exactly what that covers.
	Seed uint64
	// DrainTimeout is the deadline handed to PublishWithDrain in the
	// drain-timeout arm. The arm's VERDICT does not depend on this value —
	// the publisher holds a reference for the whole call, so the drain
	// cannot complete at any timeout — only the arm's duration does.
	DrainTimeout time.Duration
	// JoinDeadline bounds every goroutine join. It is a HANG DETECTOR, not a
	// performance threshold: a correct run joins in milliseconds, and a run
	// that reaches this deadline is reported as a wedge rather than being
	// waited on forever.
	JoinDeadline time.Duration
	// Readers is the number of concurrent reader goroutines.
	Readers int
	// MinOpsPerReader is the number of acquisitions a reader performs before
	// it is allowed to notice that the publisher has finished.
	MinOpsPerReader int
	// MaxOpsPerReader caps a reader's acquisitions so the run's cost is
	// bounded regardless of how the scheduler behaves.
	MaxOpsPerReader int
}

// DefaultGenerationSwapConfig returns the short-layer configuration for a
// seed.
func DefaultGenerationSwapConfig(seed uint64) GenerationSwapConfig {
	return GenerationSwapConfig{
		Seed:            seed,
		Readers:         genSwapDefaultReaders,
		MinOpsPerReader: genSwapDefaultMinOps,
		MaxOpsPerReader: genSwapDefaultMaxOps,
		DrainTimeout:    genSwapDefaultDrainWait,
		JoinDeadline:    genSwapJoinDeadline,
	}
}

// normalise clamps a partially-filled config to a runnable one.
func (c *GenerationSwapConfig) normalise() {
	if c.Readers <= 0 {
		c.Readers = genSwapDefaultReaders
	}
	if c.MinOpsPerReader < 0 {
		c.MinOpsPerReader = 0
	}
	if c.MaxOpsPerReader <= 0 {
		c.MaxOpsPerReader = genSwapDefaultMaxOps
	}
	if c.MaxOpsPerReader < c.MinOpsPerReader {
		c.MaxOpsPerReader = c.MinOpsPerReader
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = genSwapDefaultDrainWait
	}
	if c.JoinDeadline <= 0 {
		c.JoinDeadline = genSwapJoinDeadline
	}
}

// -----------------------------------------------------------------------------
// The plan — the model, drawn up front on one goroutine
// -----------------------------------------------------------------------------

// genSwapPublishOp is how one generation is published.
type genSwapPublishOp uint8

// The publish operations.
const (
	// genSwapOpPublish uses Publish: the predecessor is left to drain
	// naturally as readers Release, and is never marked recycled.
	genSwapOpPublish genSwapPublishOp = iota
	// genSwapOpDrain uses PublishWithDrain(c, 0) — an unbounded drain that
	// MUST return nil. It is the control for the timeout arm and the only op
	// permitted to mark its predecessor recycled.
	genSwapOpDrain
	// genSwapOpDrainTimeout uses PublishWithDrain(c, cfg.DrainTimeout) while
	// the publisher holds a reference on the predecessor, so ErrDrainTimeout
	// is structurally guaranteed.
	genSwapOpDrainTimeout
)

// String renders a publish op for the report.
func (o genSwapPublishOp) String() string {
	switch o {
	case genSwapOpPublish:
		return "publish"
	case genSwapOpDrain:
		return "drain"
	case genSwapOpDrainTimeout:
		return "drain-timeout"
	default:
		return fmt.Sprintf("genSwapPublishOp(%d)", uint8(o))
	}
}

// genSwapRow is the model's complete description of one generation: the
// artefact handed to the publisher, and the independently-computed
// expectation a reader adjudicates against.
type genSwapRow struct {
	// snapshot is the immutable artefact published for this sequence number.
	snapshot *csr.CSR[struct{}]
	// adjacency is the model's own adjacency, kept so a failing clause can
	// say what the generation was supposed to contain.
	adjacency map[graph.NodeID][]graph.NodeID
	// fingerprint is the model's digest over (order, size, adjacency),
	// computed from the map above — never from the snapshot.
	fingerprint uint64
	order       uint64
	size        uint64
	// seq is the publish sequence number; 0 is the generation [generation.New]
	// is seeded with.
	seq int
	// op is how seq is published. Unused for seq 0.
	op genSwapPublishOp
}

// genSwapPlan is the whole workload, computed before any goroutine exists.
//
// # Concurrency contract
//
// A genSwapPlan is immutable once [buildGenerationSwapPlan] returns and is
// read concurrently by every reader goroutine without synchronisation.
type genSwapPlan struct {
	rows        []genSwapRow
	readerSeeds []uint64
	// digest is a pure function of the master seed: it covers every row's
	// shape, fingerprint and op, and the drain-timeout index, but NOT the
	// reader sub-seeds (which depend on the reader count, a config value).
	digest uint64
	// drainTimeoutAt is the sequence number published through the
	// drain-timeout arm. The op at drainTimeoutAt+1 is forced to
	// genSwapOpDrain so the arm always has its paired control.
	drainTimeoutAt int
	nodes          int
}

// generations returns the number of PUBLISHES the plan schedules (the seed
// generation is not a publish).
func (p *genSwapPlan) generations() int { return len(p.rows) - 1 }

// row returns the plan row for a sequence number, or nil when seq names no
// generation the plan ever built.
func (p *genSwapPlan) row(seq int) *genSwapRow {
	if seq < 0 || seq >= len(p.rows) {
		return nil
	}
	return &p.rows[seq]
}

// buildGenerationSwapPlan draws the whole workload from the master seed, in
// a fixed order, on the calling goroutine.
//
// The draw order is load-bearing and must not be reordered: the plan is
// drawn FIRST and the reader sub-seeds LAST, so that changing the reader
// count cannot perturb the plan or its digest.
func buildGenerationSwapPlan(cfg GenerationSwapConfig) (*genSwapPlan, error) {
	master := NewSeed(cfg.Seed ^ generationSwapSeedMix)

	gens := genSwapMinGenerations + master.IntN(genSwapMaxGenerations-genSwapMinGenerations+1)
	nodes := genSwapMinNodes + master.IntN(genSwapMaxNodes-genSwapMinNodes+1)
	maxDeg := 1 + master.IntN(genSwapMaxOutDegree)

	// The marker encodes seq as node 1+seq, so the node count must leave room
	// for the highest sequence number. A geometry that cannot satisfy this is
	// a harness defect, not a finding: refuse it rather than publishing
	// generations whose identity cannot be decoded.
	if gens+1 >= nodes {
		return nil, fmt.Errorf(
			"sim: generation-swap plan geometry is unreachable: %d generations need node ids up to %d "+
				"for the sequence marker but the graph has only %d nodes", gens, gens+1, nodes)
	}

	p := &genSwapPlan{rows: make([]genSwapRow, gens+1), nodes: nodes}
	for seq := 0; seq <= gens; seq++ {
		adj := genSwapAdjacency(master, nodes, maxDeg, seq)
		snap, order, size, err := encodeGenSwapCSR(nodes, adj)
		if err != nil {
			return nil, fmt.Errorf("sim: generation-swap plan seq %d: %w", seq, err)
		}
		p.rows[seq] = genSwapRow{
			snapshot:    snap,
			adjacency:   adj,
			fingerprint: genSwapFingerprintModel(nodes, adj, order, size),
			order:       order,
			size:        size,
			seq:         seq,
		}
	}

	// Publish ops for seq 1..gens.
	for seq := 1; seq <= gens; seq++ {
		if master.Bool(0.35) {
			p.rows[seq].op = genSwapOpDrain
		} else {
			p.rows[seq].op = genSwapOpPublish
		}
	}
	// Exactly one drain-timeout arm, placed so its forced control at seq+1
	// still exists. Both are forced AFTER the free draw above, so the arm and
	// its control are guaranteed rather than probable.
	p.drainTimeoutAt = 1 + master.IntN(gens-1)
	p.rows[p.drainTimeoutAt].op = genSwapOpDrainTimeout
	p.rows[p.drainTimeoutAt+1].op = genSwapOpDrain

	p.digest = genSwapPlanDigest(cfg.Seed, p, maxDeg)

	// Reader sub-seeds LAST, so the plan above is a function of the seed
	// alone. Drawn here, on this single goroutine, and never inside a reader:
	// a *Seed is not safe for concurrent use (seed.go:41-45).
	p.readerSeeds = make([]uint64, cfg.Readers)
	for i := range p.readerSeeds {
		p.readerSeeds[i] = master.Uint64N(^uint64(0))
	}
	return p, nil
}

// genSwapAdjacency draws one generation's adjacency. Node
// [genSwapMarkerNode] is reserved for the sequence marker; every modelled
// arc runs between nodes >= 1, so the marker's out-neighbour list is
// unambiguous.
func genSwapAdjacency(rng *Seed, nodes, maxDeg, seq int) map[graph.NodeID][]graph.NodeID {
	adj := make(map[graph.NodeID][]graph.NodeID, nodes)

	// the plan's generation count, which the caller has already checked is
	// strictly below nodes-1.
	adj[genSwapMarkerNode] = []graph.NodeID{graph.NodeID(seq + 1)}
	for src := 1; src < nodes; src++ {
		deg := rng.IntN(maxDeg + 1)
		if deg == 0 {
			continue
		}
		dsts := make([]graph.NodeID, 0, deg)
		for i := 0; i < deg; i++ {

			// the sum is a valid node id in [1, nodes).
			dsts = append(dsts, graph.NodeID(1+rng.IntN(nodes-1)))
		}
		slices.Sort(dsts)
		dsts = slices.Compact(dsts)

		adj[graph.NodeID(src)] = dsts
	}
	return adj
}

// encodeGenSwapCSR is the scenario's OWN independent CSR encoder: it turns
// the model's adjacency into the offsets/edges arrays and hands them to
// [csr.FromArrays]. Writing the encoding here rather than going through
// csr.BuildFromAdjList keeps the expectation entirely inside the model — no
// node-id interning by the engine sits between what the plan intends and
// what the reader must see.
//
// The result is validated once, at plan time, so a malformed encoding is a
// harness error surfaced immediately rather than a panic inside a reader.
func encodeGenSwapCSR(nodes int, adj map[graph.NodeID][]graph.NodeID) (*csr.CSR[struct{}], uint64, uint64, error) {
	vertices := make([]uint64, nodes+1)
	total := 0
	for _, dsts := range adj {
		total += len(dsts)
	}
	edges := make([]graph.NodeID, 0, total)
	for src := 0; src < nodes; src++ {
		vertices[src] = uint64(len(edges))

		edges = append(edges, adj[graph.NodeID(src)]...)
	}
	vertices[nodes] = uint64(len(edges))

	order := uint64(nodes)
	size := uint64(len(edges))
	c := csr.FromArrays[struct{}](vertices, edges, nil, order, size)
	if err := c.Validate(); err != nil {
		return nil, 0, 0, fmt.Errorf("model-encoded snapshot is malformed: %w", err)
	}
	return c, order, size, nil
}

// -----------------------------------------------------------------------------
// Fingerprints — the model's side and the engine's side, computed apart
// -----------------------------------------------------------------------------

// FNV-1a 64-bit parameters. A byte-at-a-time FNV is used rather than
// hash/fnv so the digest allocates nothing on a reader's hot path.
const (
	genSwapFNVOffset uint64 = 14695981039346656037
	genSwapFNVPrime  uint64 = 1099511628211
)

// genSwapMix folds one 64-bit word into an FNV-1a accumulator.
func genSwapMix(h, v uint64) uint64 {
	for i := 0; i < 8; i++ {
		h ^= (v >> (8 * i)) & 0xff
		h *= genSwapFNVPrime
	}
	return h
}

// genSwapFingerprintModel digests a generation from the MODEL's adjacency
// map. It must fold exactly the same words, in exactly the same order, as
// [genSwapFingerprintCSR] reading the engine's snapshot: per source, the
// source id, then every destination, then the degree.
func genSwapFingerprintModel(nodes int, adj map[graph.NodeID][]graph.NodeID, order, size uint64) uint64 {
	h := genSwapMix(genSwapMix(genSwapFNVOffset, order), size)
	for src := 0; src < nodes; src++ {

		dsts := adj[graph.NodeID(src)]
		h = genSwapMix(h, uint64(src))
		for _, d := range dsts {
			h = genSwapMix(h, uint64(d))
		}
		h = genSwapMix(h, uint64(len(dsts)))
	}
	return h
}

// genSwapFingerprintCSR digests a generation by TRAVERSING it through the
// engine's public read path. It is the reader's side of the comparison and
// consults no model state.
//
// The iterator is ranged directly at the call site, which is the
// allocation-free form [csr.CSR.NeighboursByID] documents, and each source
// is visited exactly once.
func genSwapFingerprintCSR(c *csr.CSR[struct{}]) uint64 {
	h := genSwapMix(genSwapMix(genSwapFNVOffset, c.Order()), c.Size())
	maxID := c.MaxNodeID()
	for src := graph.NodeID(0); src < maxID; src++ {
		degree := uint64(0)
		h = genSwapMix(h, uint64(src))
		for dst := range c.NeighboursByID(src) {
			degree++
			h = genSwapMix(h, uint64(dst))
		}
		h = genSwapMix(h, degree)
	}
	return h
}

// genSwapDecodeSeq reads the publish sequence number a snapshot DECLARES,
// from node [genSwapMarkerNode]'s single out-neighbour. It returns false
// when the marker is absent, plural, or names node 0 — none of which any
// generation the plan built can produce.
func genSwapDecodeSeq(c *csr.CSR[struct{}]) (int, bool) {
	n := 0
	var marker graph.NodeID
	for dst := range c.NeighboursByID(genSwapMarkerNode) {
		n++
		if n > 1 {
			return 0, false
		}
		marker = dst
	}
	if n != 1 || marker == 0 {
		return 0, false
	}
	return int(marker) - 1, true
}

// genSwapPlanDigest digests the plan so the report can pin the one thing
// that IS a pure function of the seed.
func genSwapPlanDigest(seed uint64, p *genSwapPlan, maxDeg int) uint64 {
	h := genSwapMix(genSwapFNVOffset, seed)
	h = genSwapMix(h, uint64(len(p.rows)))
	h = genSwapMix(h, uint64(p.nodes))
	h = genSwapMix(h, uint64(maxDeg))
	h = genSwapMix(h, uint64(p.drainTimeoutAt))
	for i := range p.rows {
		r := &p.rows[i]
		h = genSwapMix(h, uint64(r.seq))
		h = genSwapMix(h, r.order)
		h = genSwapMix(h, r.size)
		h = genSwapMix(h, r.fingerprint)
		h = genSwapMix(h, uint64(r.op))
	}
	return h
}

// -----------------------------------------------------------------------------
// Per-generation bookkeeping
// -----------------------------------------------------------------------------

// genSwapSlot is the harness's per-generation shadow record. It models the
// one decision the library does not make for us — whether a generation's
// backing storage has been reclaimed — and brackets each reader's access
// window so the publisher can adjudicate a drain.
//
// # Concurrency contract
//
// Every field is atomic and genSwapSlot is safe for concurrent use by any
// number of readers and one publisher.
type genSwapSlot struct {
	// seq is the sequence number the PUBLISHER recorded for this pointer, or
	// -1 until it has done so. A reader may legitimately see -1: it can
	// Acquire a generation in the window between Publish storing it and the
	// publisher recording it, which is why the pointer/sequence agreement is
	// adjudicated terminally rather than on the hot path.
	seq atomic.Int64
	// inUse counts readers currently inside their access window on this
	// generation. A completed drain must observe zero.
	inUse atomic.Int64
	// freed models the reclamation of this generation's backing storage. The
	// publisher may set it only after an unbounded drain has returned nil.
	freed atomic.Bool
}

// genSwapRegistry maps a generation pointer to its shadow slot with a
// lock-free read path, so neither the readers nor the publisher serialise
// on a mutex to reach one.
//
// # Concurrency contract
//
// genSwapRegistry is safe for concurrent use. [genSwapRegistry.slot] is
// idempotent: whichever goroutine reaches a generation first creates its
// slot, so a reader that sees a generation before the publisher has
// recorded it does not race and does not fabricate a finding.
type genSwapRegistry struct {
	m sync.Map // *generation.Generation[struct{}] -> *genSwapSlot
}

// slot returns g's shadow slot, creating it if this is its first sighting.
func (r *genSwapRegistry) slot(g *generation.Generation[struct{}]) *genSwapSlot {
	if v, ok := r.m.Load(g); ok {
		//nolint:forcetypeassert // the slot map is unexported and only ever stores *genSwapSlot
		return v.(*genSwapSlot)
	}
	fresh := &genSwapSlot{}
	fresh.seq.Store(-1)
	v, _ := r.m.LoadOrStore(g, fresh)
	//nolint:forcetypeassert // LoadOrStore returns the value already in the slot map, which only stores *genSwapSlot
	return v.(*genSwapSlot)
}

// -----------------------------------------------------------------------------
// Findings and evidence
// -----------------------------------------------------------------------------

// genSwapFinding is one clause firing, recorded where it was observed and
// adjudicated later by [checkGenerationSwap].
type genSwapFinding struct {
	Clause  string
	Message string
	Kind    ViolationKind
}

// The clause names. Each names ONE property, so a report says which
// property broke rather than that something did.
const (
	// Identity and content — the torn-swap family.
	genSwapClauseMarker       = "content-declares-no-sequence"
	genSwapClauseUnknownSeq   = "content-declares-unpublished-sequence"
	genSwapClauseShape        = "shape-mismatch"
	genSwapClauseContent      = "content-mismatch"
	genSwapClauseMutated      = "generation-content-changed"
	genSwapClauseNonMonotonic = "acquire-went-backwards"
	genSwapClauseNilCSR       = "acquired-generation-has-nil-csr"
	genSwapClauseFirstSeq     = "first-acquisition-not-seed-generation"
	genSwapClauseLastSeq      = "last-acquisition-not-final-generation"
	genSwapClausePointerSeq   = "pointer-sequence-disagreement"

	// Lifecycle.
	genSwapClauseRefFloor     = "refcount-below-held-reference"
	genSwapClauseRefCeiling   = "refcount-above-holder-bound"
	genSwapClauseRefAtRest    = "refcount-nonzero-at-rest"
	genSwapClauseRecycled     = "use-after-recycle"
	genSwapClauseDrainInUse   = "drain-completed-with-reader-inside"
	genSwapClauseDrainErr     = "unbounded-drain-did-not-complete"
	genSwapClauseNoTimeout    = "held-drain-did-not-time-out"
	genSwapClauseHostage      = "timeout-arm-hostage-was-not-predecessor"
	genSwapClauseCurrent      = "current-not-the-published-generation"
	genSwapClauseNilPublished = "publish-returned-a-nil-generation"
	genSwapClausePublishErr   = "publish-errored-on-an-open-publisher"
	genSwapClauseAcquireNil   = "acquire-returned-nil-on-an-open-publisher"

	// Wedges and Close.
	genSwapClausePubWedged       = "publisher-did-not-finish"
	genSwapClauseCloseWedged     = "close-did-not-return"
	genSwapClauseReaderWedged    = "reader-did-not-join"
	genSwapClauseAcqAfterClose   = "acquire-after-close-returned-a-generation"
	genSwapClauseCurAfterClose   = "current-after-close-is-not-nil"
	genSwapClausePubAfterClose   = "publish-after-close-did-not-return-errclosed"
	genSwapClauseDrainAfterClose = "drain-after-close-did-not-return-errclosed"
	genSwapClauseResurrect       = "publish-succeeded-after-errclosed"

	// Non-vacuity.
	genSwapClauseNVAcquires  = "nv-no-acquisition-happened"
	genSwapClauseNVFull      = "nv-no-full-fingerprint-check-ran"
	genSwapClauseNVDistinct  = "nv-no-reader-straddled-a-swap"
	genSwapClauseNVTimeout   = "nv-drain-timeout-arm-never-ran"
	genSwapClauseNVDrain     = "nv-unbounded-drain-never-completed"
	genSwapClauseNVRecycle   = "nv-no-generation-was-ever-recycled"
	genSwapClauseNVGenCount  = "nv-generation-count-mismatch"
	genSwapClauseNVCloseLoad = "nv-close-arm-was-not-concurrent"
)

// genSwapReaderSpan is one reader's summary. Every field is a
// NON-reproducible observation and is reported as telemetry; the clauses
// that consume it are structural (first/last sequence), never rate-based.
type genSwapReaderSpan struct {
	ID          int
	FirstSeq    int
	LastSeq     int
	Distinct    int
	Acquires    int64
	Full        int64
	Cheap       int64
	NilAcquires int64
}

// GenerationSwapEvidence is what one generation-swap run OBSERVED. It
// separates the seed-reproducible plan facts from the interleaving-dependent
// counters, because conflating them is how a DST report starts asserting
// against a number the scheduler chose.
//
// # Concurrency contract
//
// A GenerationSwapEvidence is populated by [RunGenerationSwap] on its own
// goroutine, after every worker has been joined, and is read-only
// thereafter. It is not safe to mutate concurrently.
type GenerationSwapEvidence struct {
	// Findings are the clause firings observed in flight, unioned after the
	// joins (whose happens-before edge publishes every worker's writes).
	Findings []genSwapFinding
	// PublishOps renders the plan's op for sequence 1..N. Seed-reproducible.
	PublishOps []string
	// ReaderSpans is per-reader telemetry. NOT seed-reproducible.
	ReaderSpans []genSwapReaderSpan
	// RefcountsAtRest is the refcount of every generation ever published,
	// read after every worker was joined — the one point at which the value
	// cannot move.
	RefcountsAtRest []int64

	CloseDuration time.Duration
	DrainTimeout  time.Duration

	// Seed-reproducible plan facts.
	Seed           uint64
	PlanDigest     uint64
	Generations    int
	Nodes          int
	DrainTimeoutAt int

	// Config.
	Readers int

	// Interleaving-dependent counters (telemetry; the clauses that read them
	// use structural minima only).
	Acquires    int64
	NilAcquires int64
	FullChecks  int64
	CheapChecks int64

	// Publisher-side counts. Each is bounded by the plan, so a mismatch
	// against the plan is itself a finding.
	PlainPublishes      int
	DrainsCompleted     int
	DrainTimeouts       int
	HostageWasPrev      int
	Recycled            int
	GenerationsSeen     int
	DistinctGenerations int

	// Terminal facts.
	// Cancelled records that ctx cut the publish sequence short. A cancelled
	// run carries no verdict; RunGenerationSwap returns a harness error for it.
	Cancelled                  bool
	PublisherFinished          bool
	ReadersJoined              bool
	CloseReturned              bool
	AcquireAfterCloseNil       bool
	CurrentAfterCloseNil       bool
	PublishAfterCloseErrClosed bool
	DrainAfterCloseErrClosed   bool

	// Arm 2 — Close under live reader load.
	Arm2Ran              bool
	Arm2ReadersJoined    bool
	Arm2CloseReturned    bool
	Arm2Readers          int
	Arm2PreCloseAcquires int64
	Arm2NilAcquires      int64
	Arm2PublishOK        int
	Arm2PublishClosed    int
	// Arm2PublishBeforeClose is how many publishes ran SYNCHRONOUSLY before
	// the Close goroutine spawned. Every one of them met an open publisher,
	// so it is the structural floor for Arm2PublishOK.
	Arm2PublishBeforeClose int
	Arm2RefcountNonZero    int
	Arm2CapExhausted       int
}

// String renders the evidence for a report or a test log.
func (e *GenerationSwapEvidence) String() string {
	return fmt.Sprintf(
		"generation-swap[seed=%#x digest=%#x]: gens=%d nodes=%d readers=%d drainTimeout=%s ops=%v | "+
			"acquires=%d(nil=%d) full=%d cheap=%d distinct=%d | "+
			"publish=%d drain=%d timeout=%d(at=%d hostage=%d) recycled=%d | "+
			"pubDone=%t joined=%t close=%t(%s) afterClose(acqNil=%t curNil=%t pubClosed=%t drainClosed=%t) | "+
			"arm2(ran=%t readers=%d pre=%d nil=%d pubOK=%d/%d pubClosed=%d joined=%t close=%t rcNZ=%d cap=%d) | "+
			"findings=%d",
		e.Seed, e.PlanDigest, e.Generations, e.Nodes, e.Readers, e.DrainTimeout, e.PublishOps,
		e.Acquires, e.NilAcquires, e.FullChecks, e.CheapChecks, e.DistinctGenerations,
		e.PlainPublishes, e.DrainsCompleted, e.DrainTimeouts, e.DrainTimeoutAt, e.HostageWasPrev, e.Recycled,
		e.PublisherFinished, e.ReadersJoined, e.CloseReturned, e.CloseDuration.Round(time.Microsecond),
		e.AcquireAfterCloseNil, e.CurrentAfterCloseNil, e.PublishAfterCloseErrClosed, e.DrainAfterCloseErrClosed,
		e.Arm2Ran, e.Arm2Readers, e.Arm2PreCloseAcquires, e.Arm2NilAcquires, e.Arm2PublishOK,
		e.Arm2PublishBeforeClose, e.Arm2PublishClosed,
		e.Arm2ReadersJoined, e.Arm2CloseReturned, e.Arm2RefcountNonZero, e.Arm2CapExhausted,
		len(e.Findings))
}

// -----------------------------------------------------------------------------
// The reader
// -----------------------------------------------------------------------------

// genSwapReader is one reader goroutine's PRIVATE state. Nothing in it is
// shared with another goroutine, so the hot path takes no lock of the
// harness's own; the run unions the readers' findings after wg.Wait, whose
// happens-before edge publishes every write. Model: the per-connection
// writer logs in concurrent.go:297-303.
type genSwapReader struct {
	pub  *generation.Publisher[struct{}]
	plan *genSwapPlan
	reg  *genSwapRegistry
	rng  *Seed
	// seen maps a generation pointer to the sequence number its CONTENT
	// declared the first time this reader held it. A later sighting that
	// declares a different sequence number means the artefact changed under a
	// stable pointer, which is the immutability contract broken.
	seen  map[*generation.Generation[struct{}]]int
	found []genSwapFinding

	maxHolders  int64
	id          int
	firstSeq    int
	lastSeq     int
	highWater   int
	acquires    int64
	nilAcquires int64
	full        int64
	cheap       int64
}

// newGenSwapReader builds one reader. The sub-seed comes from the plan,
// drawn before any goroutine spawned; armMix separates the two arms' decision
// streams so they do not make identical choices on identical sub-seeds.
func newGenSwapReader(id int, armMix uint64, pub *generation.Publisher[struct{}], plan *genSwapPlan,
	reg *genSwapRegistry, maxHolders int64) *genSwapReader {
	return &genSwapReader{
		pub:        pub,
		plan:       plan,
		reg:        reg,
		rng:        NewSeed(plan.readerSeeds[id] ^ armMix),
		seen:       make(map[*generation.Generation[struct{}]]int, len(plan.rows)),
		maxHolders: maxHolders,
		id:         id,
		firstSeq:   -1,
		lastSeq:    -1,
		highWater:  -1,
	}
}

// fire records a clause firing.
func (rd *genSwapReader) fire(kind ViolationKind, clause, format string, args ...any) {
	rd.found = append(rd.found, genSwapFinding{
		Clause:  clause,
		Kind:    kind,
		Message: fmt.Sprintf("reader %d: ", rd.id) + fmt.Sprintf(format, args...),
	})
}

// span summarises the reader for the report.
func (rd *genSwapReader) span() genSwapReaderSpan {
	return genSwapReaderSpan{
		ID:          rd.id,
		FirstSeq:    rd.firstSeq,
		LastSeq:     rd.lastSeq,
		Distinct:    len(rd.seen),
		Acquires:    rd.acquires,
		Full:        rd.full,
		Cheap:       rd.cheap,
		NilAcquires: rd.nilAcquires,
	}
}

// probe performs one Acquire → verify → Release cycle.
//
// It returns acquired=false only when Acquire itself returned nil (a closed
// publisher), which is how the close arm detects the shutdown. A generation
// that was acquired but failed a clause still returns acquired=true, with
// seq=-1 when its content declared no usable identity — so a real defect is
// never mistaken for a clean shutdown.
//
// The order of operations inside the access window is load-bearing: inUse is
// raised BEFORE any check and lowered BEFORE Release, so a drain that
// observes refcount zero has necessarily observed inUse zero as well.
func (rd *genSwapReader) probe(full bool) (seq int, acquired bool) {
	g := rd.pub.Acquire()
	if g == nil {
		rd.nilAcquires++
		return -1, false
	}
	rd.acquires++
	slot := rd.reg.slot(g)
	slot.inUse.Add(1)
	defer func() {
		slot.inUse.Add(-1)
		rd.pub.Release(g)
	}()

	// The refcount bounds. Both are structural: this goroutine holds one
	// reference for the whole window, and no goroutine can hold two.
	if rc := g.Refcount(); rc < 1 {
		rd.fire(ViolationOracleDeviation, genSwapClauseRefFloor,
			"refcount is %d while this goroutine holds a reference from Acquire; "+
				"a held generation can never be below 1", rc)
	} else if rc > rd.maxHolders {
		rd.fire(ViolationOracleDeviation, genSwapClauseRefCeiling,
			"refcount is %d but at most %d goroutines can hold a reference "+
				"(%d readers holding one each, plus at most one publisher-held reference): "+
				"a Release was lost",
			rc, rd.maxHolders, rd.maxHolders-1)
	}
	if slot.freed.Load() {
		rd.fire(ViolationOracleDeviation, genSwapClauseRecycled,
			"Acquire returned a generation whose backing storage was already reclaimed "+
				"(its predecessor drain completed while this reference was obtainable)")
	}

	c := g.CSR()
	if c == nil {
		rd.fire(ViolationGraphIntegrity, genSwapClauseNilCSR,
			"Acquire returned a generation with a nil CSR")
		return -1, true
	}

	decoded, ok := genSwapDecodeSeq(c)
	if !ok {
		rd.fire(ViolationGraphIntegrity, genSwapClauseMarker,
			"the acquired snapshot's marker node %d does not carry exactly one out-neighbour, "+
				"so the artefact declares no generation identity (order=%d size=%d)",
			genSwapMarkerNode, c.Order(), c.Size())
		return -1, true
	}

	// Immutability: one pointer, one content, forever.
	if prev, held := rd.seen[g]; held {
		if prev != decoded {
			rd.fire(ViolationGraphIntegrity, genSwapClauseMutated,
				"the same generation pointer declared sequence %d earlier and %d now: "+
					"a published generation's content changed under it", prev, decoded)
		}
	} else {
		rd.seen[g] = decoded
	}

	// Monotonicity. With a single publisher the current pointer only ever
	// advances, so a reader can never be handed an older generation than one
	// it has already been handed.
	if decoded < rd.highWater {
		rd.fire(ViolationOracleDeviation, genSwapClauseNonMonotonic,
			"Acquire returned generation %d after this reader had already been served %d: "+
				"the current pointer went backwards", decoded, rd.highWater)
	} else {
		rd.highWater = decoded
	}
	if rd.firstSeq < 0 {
		rd.firstSeq = decoded
	}
	rd.lastSeq = decoded

	row := rd.plan.row(decoded)
	if row == nil {
		rd.fire(ViolationGraphIntegrity, genSwapClauseUnknownSeq,
			"the acquired snapshot declares sequence %d, which the plan never published "+
				"(it published 0..%d)", decoded, rd.plan.generations())
		return decoded, true
	}
	if c.Order() != row.order || c.Size() != row.size {
		rd.fire(ViolationGraphIntegrity, genSwapClauseShape,
			"generation %d has order=%d size=%d but the model built order=%d size=%d",
			decoded, c.Order(), c.Size(), row.order, row.size)
	}
	if full {
		rd.full++
		if fp := genSwapFingerprintCSR(c); fp != row.fingerprint {
			rd.fire(ViolationGraphIntegrity, genSwapClauseContent,
				"generation %d traverses to fingerprint %#x but the model's independently "+
					"computed adjacency for sequence %d fingerprints to %#x: the reader was served "+
					"content that is not this generation's", decoded, fp, decoded, row.fingerprint)
		}
	} else {
		rd.cheap++
	}
	return decoded, true
}

// run is the reader's lifecycle for the swap arm.
//
// The first and last acquisitions are MANDATORY and barriered, which is what
// makes the non-vacuity structural rather than a hope about scheduling:
//
//   - the first happens before the publisher is allowed to publish anything,
//     so it must observe the seed generation (sequence 0);
//   - the last happens after the publisher has finished every publish, so it
//     must observe the final generation.
//
// Together they guarantee every reader straddled at least one swap, without
// any assertion about how many acquisitions the middle phase managed.
// The abort channel exists for one reason: when the PUBLISHER wedges, the
// readers are not the defect and their observations hold the root cause, so
// they must be released and joined rather than discarded. But the publisher
// never reached the final generation, so the terminal sequence assertion would
// then fire for the publisher's fault rather than the reader's. Closing abort
// suppresses exactly that one assertion and nothing else.
func (rd *genSwapReader) run(ready *sync.WaitGroup, start, done, abort <-chan struct{},
	cfg GenerationSwapConfig) {
	// Mandatory pre-publish acquisition, always fully verified.
	switch seq, acquired := rd.probe(true); {
	case !acquired:
		rd.fire(ViolationOracleDeviation, genSwapClauseAcquireNil,
			"the first acquisition, taken before any publish, returned nil from an open publisher")
	case seq != 0:
		rd.fire(ViolationOracleDeviation, genSwapClauseFirstSeq,
			"the first acquisition, taken before the publisher was released, observed "+
				"generation %d rather than the seed generation 0", seq)
	}
	ready.Done()
	<-start

	// The middle phase: race the publisher. It stops at the op cap (a cost
	// ceiling) or as soon as the publisher is finished, whichever comes first,
	// but never before MinOpsPerReader acquisitions.
	for i := 0; i < cfg.MaxOpsPerReader; i++ {
		if i >= cfg.MinOpsPerReader && genSwapClosed(done) {
			break
		}
		rd.probe(rd.rng.Bool(genSwapFullCheckProb))
		runtime.Gosched()
	}

	// Mandatory post-publish acquisition, always fully verified.
	<-done
	final := rd.plan.generations()
	aborted := genSwapClosed(abort)
	switch seq, acquired := rd.probe(true); {
	case !acquired:
		rd.fire(ViolationOracleDeviation, genSwapClauseAcquireNil,
			"the last acquisition, taken after every publish completed, returned nil from an "+
				"open publisher")
	case aborted:
		// The publisher wedged; there is no final generation to expect.
	case seq != final:
		rd.fire(ViolationOracleDeviation, genSwapClauseLastSeq,
			"the last acquisition, taken after every publish completed, observed generation %d "+
				"rather than the final generation %d: the swap is not visible to this goroutine",
			seq, final)
	}
}

// genSwapClosed reports whether ch is already closed, without blocking.
func genSwapClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// -----------------------------------------------------------------------------
// The publisher
// -----------------------------------------------------------------------------

// genSwapPublisher drives the publish sequence on ONE goroutine. Single
// publisher is deliberate, not incidental: it is what makes the readers'
// monotonicity clause sound, since the plan's sequence numbers are allocated
// before the swap rather than under the library's publishMu.
type genSwapPublisher struct {
	pub  *generation.Publisher[struct{}]
	plan *genSwapPlan
	reg  *genSwapRegistry
	// clock is the logical cadence. It is owned exclusively by this goroutine,
	// which is what its concurrency contract requires, and it advances one
	// tick per publish so the cadence is POSITIONAL — a count, never a wall
	// duration — exactly as the deterministic scenarios' tick moduli are.
	clock *VirtualClock
	found []genSwapFinding
	// generations records every generation that was ever current, seed first,
	// in publish order.
	generations []*generation.Generation[struct{}]

	plainPublishes  int
	drainsCompleted int
	drainTimeouts   int
	hostageWasPrev  int
	recycled        int
	// cancelled records that the publish sequence was cut short by context
	// cancellation rather than completing. A cancelled run is NOT a verdict:
	// the readers' terminal "did you observe the final generation" assertion
	// would fire for the canceller's reason, not the engine's, so the run is
	// reported as a harness error instead of as findings.
	cancelled bool
}

// fire records a clause firing.
func (pw *genSwapPublisher) fire(kind ViolationKind, clause, format string, args ...any) {
	pw.found = append(pw.found, genSwapFinding{
		Clause:  clause,
		Kind:    kind,
		Message: fmt.Sprintf("publisher (tick %d): ", pw.clock.Now()) + fmt.Sprintf(format, args...),
	})
}

// verifyPublished checks that a generation the publisher just installed
// carries the content the plan intended for it. It runs on the publisher's
// goroutine while it holds no reference, so it reads only what Publish
// returned — which is exactly what a reader will be handed next.
// Callers guarantee g is non-nil: the nil case is a distinct property with its
// own clause (genSwapClauseNilPublished), reported where the publish happened.
func (pw *genSwapPublisher) verifyPublished(g *generation.Generation[struct{}], seq int) {
	c := g.CSR()
	if c == nil {
		pw.fire(ViolationGraphIntegrity, genSwapClauseNilCSR,
			"the generation just published for sequence %d has a nil CSR", seq)
		return
	}
	got, ok := genSwapDecodeSeq(c)
	if !ok {
		pw.fire(ViolationGraphIntegrity, genSwapClauseMarker,
			"the generation just published for sequence %d declares no identity", seq)
		return
	}
	if got != seq {
		pw.fire(ViolationGraphIntegrity, genSwapClauseMutated,
			"publishing the model's snapshot for sequence %d produced a generation whose content "+
				"declares sequence %d", seq, got)
		return
	}
	if fp := genSwapFingerprintCSR(c); fp != pw.plan.row(seq).fingerprint {
		pw.fire(ViolationGraphIntegrity, genSwapClauseContent,
			"the generation just published for sequence %d fingerprints to %#x, but the model's "+
				"adjacency for it fingerprints to %#x", seq, fp, pw.plan.row(seq).fingerprint)
	}
}

// record books a generation as having been current and stamps its slot with
// the sequence number, so the terminal pointer/sequence cross-check has
// ground truth.
func (pw *genSwapPublisher) record(g *generation.Generation[struct{}], seq int) {
	pw.generations = append(pw.generations, g)
	pw.reg.slot(g).seq.Store(int64(seq))
}

// run publishes sequence 1..N, one per logical tick.
func (pw *genSwapPublisher) run(ctx context.Context, cfg GenerationSwapConfig) {
	for seq := 1; seq <= pw.plan.generations(); seq++ {
		if ctx.Err() != nil {
			pw.cancelled = true
			return
		}
		row := pw.plan.row(seq)
		var next *generation.Generation[struct{}]
		switch row.op {
		case genSwapOpPublish:
			var err error
			if next, err = pw.pub.Publish(row.snapshot); err != nil {
				pw.fire(ViolationOracleDeviation, genSwapClausePublishErr,
					"Publish of generation %d on an open publisher returned %v", seq, err)
				return
			}
			pw.plainPublishes++

		case genSwapOpDrain:
			// prev is exactly what PublishWithDrain will capture: this is the
			// only publisher, so nothing can swap between this load and the
			// call.
			prev := pw.pub.Current()
			var err error
			if next, err = pw.pub.PublishWithDrain(row.snapshot, 0); err != nil {
				pw.fire(ViolationOracleDeviation, genSwapClauseDrainErr,
					"PublishWithDrain(_, 0) for generation %d returned %v; an unbounded drain "+
						"has no deadline to miss and must block until the predecessor drains", seq, err)
				return
			}
			pw.drainsCompleted++
			// The drain returned nil, so prev's refcount reached zero inside the
			// call. Every reader lowers inUse BEFORE Release, so zero refcount
			// implies zero inUse: this assertion cannot be a scheduling artefact.
			if prev != nil {
				slot := pw.reg.slot(prev)
				if n := slot.inUse.Load(); n != 0 {
					pw.fire(ViolationOracleDeviation, genSwapClauseDrainInUse,
						"the drain of the predecessor of generation %d completed with %d reader(s) "+
							"still inside their access window", seq, n)
				}
				// Model the reclamation. Sound because the predecessor is no
				// longer current, so no Acquire can hand it to a reader again.
				slot.freed.Store(true)
				pw.recycled++
			}

		case genSwapOpDrainTimeout:
			// The structural construction: this goroutine takes a reference on
			// the generation it is about to supersede and holds it across the
			// whole call, so PublishWithDrain's wait condition
			// (prev.refcount > 0) is permanently true and the timeout branch is
			// the only reachable exit — at ANY positive timeout.
			prev := pw.pub.Current()
			hostage := pw.pub.Acquire()
			if hostage == nil {
				pw.fire(ViolationOracleDeviation, genSwapClauseAcquireNil,
					"Acquire returned nil on an open publisher while arming the drain-timeout arm")
				return
			}
			if hostage != prev {
				pw.fire(ViolationOracleDeviation, genSwapClauseHostage,
					"the drain-timeout arm's hostage reference is not the generation the publish "+
						"will supersede, so a timeout would not prove the contract")
			} else {
				pw.hostageWasPrev++
			}
			var err error
			next, err = pw.pub.PublishWithDrain(row.snapshot, cfg.DrainTimeout)
			if !errors.Is(err, generation.ErrDrainTimeout) {
				pw.fire(ViolationOracleDeviation, genSwapClauseNoTimeout,
					"PublishWithDrain(_, %s) returned %v while this goroutine held a reference on "+
						"the predecessor for the whole call: the drain cannot have completed",
					cfg.DrainTimeout, err)
			} else {
				pw.drainTimeouts++
			}
			pw.pub.Release(hostage)
		}

		// A nil generation with a nil error is a contract breach in its own
		// right, and it is the ONLY reason next can be nil here: every branch
		// above either returned on a non-nil error or came back from a call
		// documented to install a generation — PublishWithDrain does so even on
		// the ErrDrainTimeout path, which is the whole point of the
		// "without corrupting Current" clause.
		if next == nil {
			pw.fire(ViolationOracleDeviation, genSwapClauseNilPublished,
				"publishing generation %d installed no generation, though the call reported no "+
					"error it could have failed with", seq)
		} else {
			if cur := pw.pub.Current(); cur != next {
				pw.fire(ViolationOracleDeviation, genSwapClauseCurrent,
					"after publishing generation %d, Current() is not the generation the publish "+
						"returned", seq)
			}
			pw.verifyPublished(next, seq)
			pw.record(next, seq)
		}
		pw.clock.Tick()
	}
}

// -----------------------------------------------------------------------------
// The run
// -----------------------------------------------------------------------------

// RunGenerationSwap performs one generation-swap run and returns what it
// observed, or a harness error.
//
// # Concurrency contract
//
// RunGenerationSwap spawns cfg.Readers reader goroutines plus one publisher
// goroutine per arm, every one with a lifecycle bounded by the plan and by
// ctx, and JOINS them all before returning — so no goroutine outlives the
// call and goleak has nothing to find. A join that reaches
// cfg.JoinDeadline is reported as a wedge; the run then STOPS rather than
// auditing refcounts a live goroutine could still be moving, and the leaked
// goroutine is left for goleak to report, loudly, as the defect it is.
func RunGenerationSwap(ctx context.Context, cfg GenerationSwapConfig) (*GenerationSwapEvidence, error) {
	cfg.normalise()
	plan, err := buildGenerationSwapPlan(cfg)
	if err != nil {
		return nil, err
	}

	ev := &GenerationSwapEvidence{
		Seed:           cfg.Seed,
		PlanDigest:     plan.digest,
		Generations:    plan.generations(),
		Nodes:          plan.nodes,
		Readers:        cfg.Readers,
		DrainTimeoutAt: plan.drainTimeoutAt,
		DrainTimeout:   cfg.DrainTimeout,
	}
	ev.PublishOps = make([]string, 0, plan.generations())
	for seq := 1; seq <= plan.generations(); seq++ {
		ev.PublishOps = append(ev.PublishOps, plan.row(seq).op.String())
	}

	if err := runGenerationSwapArm(ctx, cfg, plan, ev); err != nil {
		return ev, err
	}
	// A cancelled run is a harness outcome, not an engine verdict: reporting
	// its downstream findings would blame the engine for the canceller.
	if ev.Cancelled {
		return ev, fmt.Errorf(
			"sim: generation-swap: the publish sequence was cancelled before completing: %w", ctx.Err())
	}
	// A wedge in arm 1 leaves goroutines running; starting arm 2 on top of
	// that would compound the damage and confuse the attribution.
	if !ev.ReadersJoined || !ev.PublisherFinished {
		return ev, nil
	}
	if err := runGenerationCloseArm(ctx, cfg, plan, ev); err != nil {
		return ev, err
	}
	return ev, nil
}

// runGenerationSwapArm is arm 1: N readers straddling a publisher's whole
// sequence, then a quiescent refcount audit, then Close.
func runGenerationSwapArm(ctx context.Context, cfg GenerationSwapConfig, plan *genSwapPlan,
	ev *GenerationSwapEvidence) error {
	reg := &genSwapRegistry{}
	pub := generation.New(plan.row(0).snapshot)
	seed := pub.Current()
	if seed == nil {
		return fmt.Errorf("sim: generation-swap: New returned a publisher with no current generation")
	}

	pw := &genSwapPublisher{pub: pub, plan: plan, reg: reg, clock: NewVirtualClock(time.Millisecond)}
	pw.record(seed, 0)
	pw.verifyPublished(seed, 0)

	maxHolders := int64(cfg.Readers) + 1
	readers := make([]*genSwapReader, cfg.Readers)
	for i := range readers {
		readers[i] = newGenSwapReader(i, 0, pub, plan, reg, maxHolders)
	}

	var (
		ready sync.WaitGroup
		wg    sync.WaitGroup
		start = make(chan struct{})
		done  = make(chan struct{})
		abort = make(chan struct{})
	)
	ready.Add(cfg.Readers)
	wg.Add(cfg.Readers)
	for i := range readers {
		go func(rd *genSwapReader) {
			defer wg.Done()
			rd.run(&ready, start, done, abort, cfg)
		}(readers[i])
	}

	// Every reader has taken its mandatory pre-publish acquisition, so every
	// one of them has observed the seed generation. Only now may the
	// publisher move.
	ready.Wait()
	close(start)

	// The publisher runs on its own goroutine behind a hang guard. An
	// unbounded PublishWithDrain(_, 0) takes no context, so a genuine drain
	// livelock would otherwise be a silent 30-minute test timeout instead of a
	// named finding.
	pubDone := make(chan struct{})
	go func() { defer close(pubDone); pw.run(ctx, cfg) }()
	select {
	case <-pubDone:
		// A cancelled sequence never reached the final generation, so releasing
		// the readers without aborting would fire every reader's terminal
		// assertion for the canceller's reason rather than the engine's.
		if pw.cancelled {
			ev.Cancelled = true
			close(abort)
			close(done)
			_ = waitWGTimeout(&wg, cfg.JoinDeadline)
			return nil
		}
		ev.PublisherFinished = true
	case <-time.After(cfg.JoinDeadline):
		ev.Findings = append(ev.Findings, genSwapFinding{
			Clause: genSwapClausePubWedged,
			Kind:   ViolationOracleDeviation,
			Message: fmt.Sprintf("the publisher did not finish its %d publishes within %s; an unbounded "+
				"drain is not completing, so no downstream audit is sound", plan.generations(), cfg.JoinDeadline),
		})
		// The readers are not the defect here, and their private findings
		// usually hold the ROOT CAUSE of a stalled drain — a refcount that
		// breached its holder bound. Release and join them so those
		// observations reach the report instead of being discarded with the
		// wedge. Measured: without this, a Release that stopped decrementing
		// reported only "publisher-did-not-finish" and the reader pool's
		// refcount-above-holder-bound sightings were thrown away.
		// ORDER IS LOAD-BEARING: a reader reads abort only after <-done, so
		// abort must be closed FIRST. Reversing these two lines would bury the
		// root cause under one terminal-assertion firing per reader.
		close(abort)
		close(done)
		if waitWGTimeout(&wg, cfg.JoinDeadline) {
			ev.DistinctGenerations = collectGenSwapReaders(readers, ev)
		} else {
			ev.Findings = append(ev.Findings, genSwapFinding{
				Clause:  genSwapClauseReaderWedged,
				Kind:    ViolationOracleDeviation,
				Message: "the reader pool did not join after the publisher wedge either",
			})
		}
		// pw.found is deliberately NOT read: the publisher goroutine is still
		// live inside its drain, so there is no happens-before edge to it and
		// reading its findings would be a data race.
		return nil
	}
	close(done)
	// pubDone has closed, so the publisher's findings are published to this
	// goroutine and are collected BEFORE the join is attempted. Collecting them
	// after the join instead would discard them on the reader-wedge path — the
	// exact mirror of the loss measured on the publisher-wedge path above.
	ev.Findings = append(ev.Findings, pw.found...)

	if !waitWGTimeout(&wg, cfg.JoinDeadline) {
		ev.Findings = append(ev.Findings, genSwapFinding{
			Clause: genSwapClauseReaderWedged,
			Kind:   ViolationOracleDeviation,
			Message: fmt.Sprintf("the reader pool did not join within %s after the publisher finished; "+
				"a reader is wedged, so the quiescent refcount audit cannot run", cfg.JoinDeadline),
		})
		return nil
	}
	ev.ReadersJoined = true

	// Union the workers' findings. The pubDone close and wg.Wait above are the
	// happens-before edges that publish every worker's writes to this
	// goroutine.
	ev.DistinctGenerations = collectGenSwapReaders(readers, ev)
	ev.PlainPublishes = pw.plainPublishes
	ev.DrainsCompleted = pw.drainsCompleted
	ev.DrainTimeouts = pw.drainTimeouts
	ev.HostageWasPrev = pw.hostageWasPrev
	ev.Recycled = pw.recycled
	ev.GenerationsSeen = len(pw.generations)

	rcs, rcFindings := auditGenSwapRefcountsAtRest(pw.generations)
	ev.RefcountsAtRest = rcs
	ev.Findings = append(ev.Findings, rcFindings...)
	ev.Findings = append(ev.Findings, auditGenSwapPointerSequences(readers, reg)...)

	// Close on a quiesced publisher, then the post-close contract.
	closeDone := make(chan struct{})
	closeStart := time.Now()
	go func() { pub.Close(); close(closeDone) }()
	select {
	case <-closeDone:
		ev.CloseReturned = true
		ev.CloseDuration = time.Since(closeStart)
	case <-time.After(cfg.JoinDeadline):
		ev.Findings = append(ev.Findings, genSwapFinding{
			Clause: genSwapClauseCloseWedged,
			Kind:   ViolationOracleDeviation,
			Message: fmt.Sprintf("Close did not return within %s although every reader had already "+
				"been joined and every refcount audited", cfg.JoinDeadline),
		})
		return nil
	}

	if g := pub.Acquire(); g != nil {
		pub.Release(g)
	} else {
		ev.AcquireAfterCloseNil = true
	}
	ev.CurrentAfterCloseNil = pub.Current() == nil
	_, perr := pub.Publish(plan.row(0).snapshot)
	ev.PublishAfterCloseErrClosed = errors.Is(perr, generation.ErrClosed)
	_, derr := pub.PublishWithDrain(plan.row(0).snapshot, 0)
	ev.DrainAfterCloseErrClosed = errors.Is(derr, generation.ErrClosed)
	return nil
}

// runGenerationCloseArm is arm 2: Close called while readers are LIVE and a
// publisher is still publishing. Arm 1 closes a quiesced publisher, which
// cannot show that Close drains — this arm can.
//
// Termination is structural, not timed: Close swaps current to nil
// permanently, so every reader's Acquire eventually returns nil and the
// reader exits on that. The op cap is a hang guard, and exhausting it is
// itself reported.
func runGenerationCloseArm(ctx context.Context, cfg GenerationSwapConfig, plan *genSwapPlan,
	ev *GenerationSwapEvidence) error {
	reg := &genSwapRegistry{}
	pub := generation.New(plan.row(0).snapshot)
	if pub.Current() == nil {
		return fmt.Errorf("sim: generation-swap close arm: New returned a publisher with no current generation")
	}
	ev.Arm2Ran = true
	ev.Arm2Readers = cfg.Readers

	maxHolders := int64(cfg.Readers) + 1
	readers := make([]*genSwapReader, cfg.Readers)
	for i := range readers {
		readers[i] = newGenSwapReader(i, genSwapCloseArmSeedMix, pub, plan, reg, maxHolders)
	}

	var (
		ready sync.WaitGroup
		wg    sync.WaitGroup
		pre   atomic.Int64
		caps  atomic.Int64
	)
	ready.Add(cfg.Readers)
	wg.Add(cfg.Readers)
	for i := range readers {
		go func(rd *genSwapReader) {
			defer wg.Done()
			// One acquisition first, so the barrier proves this reader was
			// genuinely active before Close was called.
			rd.probe(true)
			pre.Add(rd.acquires)
			ready.Done()
			for i := 0; i < genSwapArm2MaxOps; i++ {
				if _, acquired := rd.probe(rd.rng.Bool(genSwapFullCheckProb)); !acquired {
					return
				}
				runtime.Gosched()
			}
			caps.Add(1)
		}(readers[i])
	}
	ready.Wait()

	// The Publish/Close interleaving. Measurement drove the shape here: with
	// the whole publish sequence spawned alongside Close, Close won every
	// single time (measured pubOK=0 across every seed and reader count), so
	// the arm asserted "every publish is either served or refused" against a
	// population in which none was ever served. Half the sequence therefore
	// runs SYNCHRONOUSLY first, on a provably open publisher, which makes a
	// served population structural rather than lucky; the rest races Close.
	var (
		okPublishes     int
		closedPublishes int
		resurrections   int
		sawClosed       bool
		attempts        = min(genSwapArm2Publishes, plan.generations())
		beforeClose     = attempts / 2
	)
	// unexpectedErrs collects any error that is neither nil nor ErrClosed. That
	// is the ONE error class Publish cannot legitimately produce on this path,
	// so it must reach the report rather than quietly ending the loop. It is
	// written only by publishOne, which never runs on two goroutines at once
	// (the synchronous loop below completes before the goroutine is spawned),
	// and is read only after <-pubDone.
	var unexpectedErrs []error
	publishOne := func(seq int) bool {
		_, err := pub.Publish(plan.row(seq).snapshot)
		switch {
		case err == nil:
			okPublishes++
			if sawClosed {
				resurrections++
			}
		case errors.Is(err, generation.ErrClosed):
			closedPublishes++
			sawClosed = true
		default:
			unexpectedErrs = append(unexpectedErrs,
				fmt.Errorf("close arm: Publish of generation %d returned %w, which is neither success "+
					"nor the documented ErrClosed refusal", seq, err))
			return false
		}
		return true
	}
	// synchronousOK counts what the pre-Close loop actually PERFORMED, not what
	// it planned: the loop can end early on cancellation or on an unexpected
	// error, and asserting against the planned count would make the floor a
	// harness claim rather than a measurement.
	synchronousOK := 0
	for seq := 1; seq <= beforeClose; seq++ {
		if ctx.Err() != nil {
			break
		}
		if !publishOne(seq) {
			break
		}
		synchronousOK++
	}

	pubDone := make(chan struct{})
	go func() {
		defer close(pubDone)
		for seq := beforeClose + 1; seq <= attempts; seq++ {
			if ctx.Err() != nil {
				return
			}
			if !publishOne(seq) {
				return
			}
			runtime.Gosched()
		}
	}()

	closeDone := make(chan struct{})
	go func() { pub.Close(); close(closeDone) }()

	// Unlike arm 1, which returns immediately on a Close wedge because its
	// refcount audit needs quiescence, this arm records the finding and CARRIES
	// ON. Its post-close reads stay sound either way: Close nils the current
	// pointer under publishMu before it begins waiting, so Acquire/Current/
	// Publish already answer as closed whether or not the wait completed.
	select {
	case <-closeDone:
		ev.Arm2CloseReturned = true
	case <-time.After(cfg.JoinDeadline):
		ev.Findings = append(ev.Findings, genSwapFinding{
			Clause: genSwapClauseCloseWedged,
			Kind:   ViolationOracleDeviation,
			Message: fmt.Sprintf("Close did not return within %s with %d readers live: a reader is "+
				"wedged inside the drain", cfg.JoinDeadline, cfg.Readers),
		})
	}
	<-pubDone

	if !waitWGTimeout(&wg, cfg.JoinDeadline) {
		ev.Findings = append(ev.Findings, genSwapFinding{
			Clause: genSwapClauseReaderWedged,
			Kind:   ViolationOracleDeviation,
			Message: fmt.Sprintf("the reader pool did not join within %s after Close returned; Close "+
				"reported a completed drain while a reader was still running", cfg.JoinDeadline),
		})
		return nil
	}
	ev.Arm2ReadersJoined = true
	ev.Arm2PreCloseAcquires = pre.Load()
	// Derived after the join rather than counted atomically: each reader leaves
	// this arm on its first nil Acquire, so its own private counter already
	// holds the answer and wg.Wait publishes it.
	for _, rd := range readers {
		ev.Arm2NilAcquires += rd.nilAcquires
	}
	ev.Arm2CapExhausted = int(caps.Load())
	ev.Arm2PublishOK = okPublishes
	ev.Arm2PublishClosed = closedPublishes
	ev.Arm2PublishBeforeClose = synchronousOK
	// Only the FINDINGS are unioned here, deliberately: the non-vacuity floors
	// on Acquires/FullChecks are calibrated on arm 1's barriered two-mandatory-
	// acquisitions-per-reader construction, which this arm does not have. Folding
	// arm 2's counts in would loosen both floors without loosening what they are
	// meant to guarantee.
	for _, rd := range readers {
		ev.Findings = append(ev.Findings, rd.found...)
	}
	for _, uerr := range unexpectedErrs {
		ev.Findings = append(ev.Findings, genSwapFinding{
			Clause:  genSwapClausePublishErr,
			Kind:    ViolationOracleDeviation,
			Message: uerr.Error(),
		})
	}
	if resurrections > 0 {
		ev.Findings = append(ev.Findings, genSwapFinding{
			Clause: genSwapClauseResurrect,
			Kind:   ViolationOracleDeviation,
			Message: fmt.Sprintf("%d Publish call(s) succeeded AFTER one had already been refused with "+
				"ErrClosed: Close is documented as permanent", resurrections),
		})
	}

	// The quiescent audit again: every generation the readers ever held must
	// be back at zero now that they are all joined.
	reg.m.Range(func(k, _ any) bool {
		//nolint:forcetypeassert // every key in this Range is a *generation.Generation[struct{}], the only key type the harness stores in this map
		g := k.(*generation.Generation[struct{}])
		if rc := g.Refcount(); rc != 0 {
			ev.Arm2RefcountNonZero++
			ev.Findings = append(ev.Findings, genSwapFinding{
				Clause: genSwapClauseRefAtRest,
				Kind:   ViolationOracleDeviation,
				Message: fmt.Sprintf("close arm: a generation has refcount %d after Close returned and "+
					"every reader was joined", rc),
			})
		}
		return true
	})

	if g := pub.Acquire(); g != nil {
		pub.Release(g)
		ev.Findings = append(ev.Findings, genSwapFinding{
			Clause:  genSwapClauseAcqAfterClose,
			Kind:    ViolationOracleDeviation,
			Message: "close arm: Acquire returned a generation after Close returned",
		})
	}
	if pub.Current() != nil {
		ev.Findings = append(ev.Findings, genSwapFinding{
			Clause:  genSwapClauseCurAfterClose,
			Kind:    ViolationOracleDeviation,
			Message: "close arm: Current() is non-nil after Close returned",
		})
	}
	return nil
}

// -----------------------------------------------------------------------------
// The terminal audits
// -----------------------------------------------------------------------------

// collectGenSwapReaders unions the readers' PRIVATE findings and telemetry
// into the evidence and returns how many distinct generations they observed
// between them.
//
// It must be called only after the reader pool has been JOINED: wg.Wait is
// the happens-before edge that publishes each reader's writes to the
// collecting goroutine, and without it every field read here would be a data
// race.
func collectGenSwapReaders(readers []*genSwapReader, ev *GenerationSwapEvidence) int {
	distinct := make(map[*generation.Generation[struct{}]]struct{}, len(readers))
	for _, rd := range readers {
		ev.Findings = append(ev.Findings, rd.found...)
		ev.ReaderSpans = append(ev.ReaderSpans, rd.span())
		ev.Acquires += rd.acquires
		ev.NilAcquires += rd.nilAcquires
		ev.FullChecks += rd.full
		ev.CheapChecks += rd.cheap
		for g := range rd.seen {
			distinct[g] = struct{}{}
		}
	}
	return len(distinct)
}

// auditGenSwapRefcountsAtRest is THE QUIESCENT REFCOUNT AUDIT. It must be
// called only once every reader goroutine has been JOINED and the publisher
// has stopped, because that is the one point at which no goroutine in the
// process can move a refcount and "the refcount is zero" is therefore an
// exact statement rather than a sample of a moving target.
//
// It returns the refcount of every generation in publish order plus one
// finding per generation that is not at zero.
func auditGenSwapRefcountsAtRest(gens []*generation.Generation[struct{}]) ([]int64, []genSwapFinding) {
	rcs := make([]int64, 0, len(gens))
	var found []genSwapFinding
	for seq, g := range gens {
		rc := g.Refcount()
		rcs = append(rcs, rc)
		if rc != 0 {
			found = append(found, genSwapFinding{
				Clause: genSwapClauseRefAtRest,
				Kind:   ViolationOracleDeviation,
				Message: fmt.Sprintf("generation %d has refcount %d after every reader was joined and "+
					"the publisher stopped: no goroutine can hold a reference, so this is a leaked "+
					"Acquire", seq, rc),
			})
		}
	}
	return rcs, found
}

// auditGenSwapPointerSequences is the terminal pointer/sequence cross-check:
// every generation a reader was handed must be one the publisher recorded,
// under the same sequence number the content declared. It is sound here and
// only here — DURING the run a reader can legitimately hold a generation in
// the window between Publish storing it and the publisher stamping its slot,
// so checking on the hot path would fabricate findings.
func auditGenSwapPointerSequences(readers []*genSwapReader, reg *genSwapRegistry) []genSwapFinding {
	var found []genSwapFinding
	for _, rd := range readers {
		for g, contentSeq := range rd.seen {
			if recorded := reg.slot(g).seq.Load(); recorded != int64(contentSeq) {
				found = append(found, genSwapFinding{
					Clause: genSwapClausePointerSeq,
					Kind:   ViolationGraphIntegrity,
					Message: fmt.Sprintf("reader %d held a generation whose content declared sequence %d, "+
						"but the publisher recorded sequence %d for that pointer", rd.id, contentSeq, recorded),
				})
			}
		}
	}
	return found
}

// -----------------------------------------------------------------------------
// The contract
// -----------------------------------------------------------------------------

// genSwapOp renders the Op field of a violation for one clause, so a report
// names the clause that fired.
func genSwapOp(clause string) string { return "<generation-swap:" + clause + ">" }

// genSwapViolation builds one violation for a clause.
func genSwapViolation(kind ViolationKind, clause, msg string) Violation {
	return Violation{Kind: kind, Op: genSwapOp(clause), Message: msg}
}

// checkGenerationSwap adjudicates the run's evidence against the contract.
// It returns one violation per clause that fired; an empty result means the
// run satisfied every clause.
func checkGenerationSwap(e *GenerationSwapEvidence) []Violation {
	v := make([]Violation, 0, len(e.Findings)+8)
	for i := range e.Findings {
		f := &e.Findings[i]
		v = append(v, genSwapViolation(f.Kind, f.Clause, f.Message))
	}

	// The post-close contract is only meaningful once the run reached a
	// quiescent Close. A wedge has already been recorded as a finding above;
	// asserting further clauses on a run whose goroutines never stopped would
	// report consequences, not causes.
	if !e.ReadersJoined || !e.PublisherFinished || !e.CloseReturned {
		return v
	}
	if !e.AcquireAfterCloseNil {
		v = append(v, genSwapViolation(ViolationOracleDeviation, genSwapClauseAcqAfterClose,
			"Acquire returned a generation after Close; the documented contract is nil"))
	}
	if !e.CurrentAfterCloseNil {
		v = append(v, genSwapViolation(ViolationOracleDeviation, genSwapClauseCurAfterClose,
			"Current() is non-nil after Close returned"))
	}
	if !e.PublishAfterCloseErrClosed {
		v = append(v, genSwapViolation(ViolationOracleDeviation, genSwapClausePubAfterClose,
			"Publish after Close did not return ErrClosed"))
	}
	if !e.DrainAfterCloseErrClosed {
		v = append(v, genSwapViolation(ViolationOracleDeviation, genSwapClauseDrainAfterClose,
			"PublishWithDrain after Close did not return ErrClosed"))
	}
	// The swap arm's publisher is open for the whole arm and every publish
	// installs a non-nil generation, so a nil Acquire anywhere in it means the
	// current pointer went nil under an open publisher. Only the two barriered
	// acquisitions report it themselves; this catches it in the middle phase.
	if e.NilAcquires != 0 {
		v = append(v, genSwapViolation(ViolationOracleDeviation, genSwapClauseAcquireNil,
			fmt.Sprintf("%d Acquire call(s) returned nil during the swap arm, in which the publisher "+
				"was never closed: the current pointer went nil under an open publisher", e.NilAcquires)))
	}
	return v
}

// checkGenerationSwapNonVacuity proves the run actually exercised what the
// clauses adjudicate. Every gate below is STRUCTURAL — a count the plan or
// the barriers guarantee — never a rate the scheduler chooses, so it cannot
// go red on a slow machine and cannot go green on a run that did nothing.
func checkGenerationSwapNonVacuity(e *GenerationSwapEvidence) []Violation {
	var v []Violation
	add := func(clause, msg string) {
		v = append(v, genSwapViolation(ViolationOracleDeviation, clause, msg))
	}

	if !e.PublisherFinished {
		add(genSwapClauseNVAcquires, "the publisher never finished, so no gate below can be evaluated")
		return v
	}
	if !e.ReadersJoined {
		add(genSwapClauseNVAcquires, "the reader pool never joined, so no gate below can be evaluated")
		return v
	}
	// Each reader takes two mandatory acquisitions (pre-publish and
	// post-publish), so this floor is guaranteed by the construction.
	mandatory := int64(2 * e.Readers)
	if e.Acquires < mandatory {
		add(genSwapClauseNVAcquires, fmt.Sprintf(
			"only %d acquisitions happened, but the two mandatory barriered acquisitions per reader "+
				"guarantee at least %d: the reader lifecycle did not run", e.Acquires, mandatory))
	}
	if e.FullChecks < mandatory {
		add(genSwapClauseNVFull, fmt.Sprintf(
			"only %d full fingerprint checks ran; each reader's first and last acquisition is "+
				"unconditionally full, so at least %d must have", e.FullChecks, mandatory))
	}
	// The mandatory first acquisition observes generation 0 and the mandatory
	// last observes generation N (N >= 1), so every reader necessarily saw at
	// least two distinct generations.
	for i := range e.ReaderSpans {
		s := &e.ReaderSpans[i]
		if s.Distinct < 2 {
			add(genSwapClauseNVDistinct, fmt.Sprintf(
				"reader %d observed %d distinct generation(s) (first=%d last=%d): its barriered first "+
					"and last acquisitions should have straddled every swap",
				s.ID, s.Distinct, s.FirstSeq, s.LastSeq))
		}
	}
	if e.DistinctGenerations < 2 {
		add(genSwapClauseNVDistinct, fmt.Sprintf(
			"the readers between them observed %d distinct generation(s): no swap was ever observed",
			e.DistinctGenerations))
	}
	// The plan schedules EXACTLY one drain-timeout arm, with the hostage
	// proven to be the predecessor, and forces an unbounded drain right after
	// it as the control.
	if e.DrainTimeouts != 1 {
		add(genSwapClauseNVTimeout, fmt.Sprintf(
			"the drain-timeout arm produced ErrDrainTimeout %d time(s); the plan schedules exactly one "+
				"(at sequence %d)", e.DrainTimeouts, e.DrainTimeoutAt))
	}
	if e.HostageWasPrev != 1 {
		add(genSwapClauseNVTimeout, fmt.Sprintf(
			"the drain-timeout arm confirmed its hostage was the superseded generation %d time(s), "+
				"want 1: without that, a timeout proves nothing", e.HostageWasPrev))
	}
	if e.DrainsCompleted < 1 {
		add(genSwapClauseNVDrain, fmt.Sprintf(
			"no unbounded drain completed (%d), so nothing establishes that PublishWithDrain is not "+
				"simply always timing out", e.DrainsCompleted))
	}
	if e.Recycled < 1 {
		add(genSwapClauseNVRecycle, fmt.Sprintf(
			"no generation was ever marked recycled (%d), so the use-after-recycle clause could not "+
				"have fired", e.Recycled))
	}
	// Every generation the plan built must have been current exactly once.
	if want := e.Generations + 1; e.GenerationsSeen != want {
		add(genSwapClauseNVGenCount, fmt.Sprintf(
			"the publisher recorded %d generations but the plan builds %d (the seed plus %d publishes)",
			e.GenerationsSeen, want, e.Generations))
	}
	if len(e.RefcountsAtRest) != e.GenerationsSeen {
		add(genSwapClauseNVGenCount, fmt.Sprintf(
			"the quiescent audit read %d refcounts for %d recorded generations",
			len(e.RefcountsAtRest), e.GenerationsSeen))
	}

	if e.Arm2Ran && e.Arm2ReadersJoined {
		// The close arm's readers exit only on a nil Acquire, and every one of
		// them took an acquisition before the barrier released Close.
		want := int64(e.Arm2Readers)
		if e.Arm2PreCloseAcquires < want {
			add(genSwapClauseNVCloseLoad, fmt.Sprintf(
				"only %d acquisition(s) happened before Close was called with %d readers: Close did "+
					"not race live readers", e.Arm2PreCloseAcquires, e.Arm2Readers))
		}
		if e.Arm2NilAcquires < want {
			add(genSwapClauseNVCloseLoad, fmt.Sprintf(
				"only %d of %d readers left the close arm on a nil Acquire; the rest did not observe "+
					"the closed publisher at all", e.Arm2NilAcquires, e.Arm2Readers))
		}
		if e.Arm2PublishOK < e.Arm2PublishBeforeClose {
			add(genSwapClauseNVCloseLoad, fmt.Sprintf(
				"only %d publish(es) were served although %d ran synchronously before the Close "+
					"goroutine spawned, on a provably open publisher: the arm cannot claim to have "+
					"exercised the Publish/Close interleaving from both sides",
				e.Arm2PublishOK, e.Arm2PublishBeforeClose))
		}
		if e.Arm2CapExhausted > 0 {
			add(genSwapClauseNVCloseLoad, fmt.Sprintf(
				"%d reader(s) exhausted the %d-acquisition hang guard without ever seeing a nil "+
					"Acquire: Close did not take effect", e.Arm2CapExhausted, genSwapArm2MaxOps))
		}
	}
	return v
}

// -----------------------------------------------------------------------------
// Catalogue wiring
// -----------------------------------------------------------------------------

// generationSwapScenario builds the catalogue entry.
//
// Mode is [ModeConcurrent] with a run override, and both halves of that are
// deliberate. ModeConcurrent is the honest label: the scenario spawns real
// goroutines whose interleaving is not seed-controlled, which is exactly what
// that mode documents, and it makes [ExecMode.Reproducible] return false so
// the CLI declines to record, replay or shrink a run it could not reproduce
// (cmd/sim/main.go:499-503) instead of pretending otherwise. The override is
// then mandatory rather than stylistic: a ModeConcurrent scenario without one
// dispatches to runConcurrent, which drives clients over the Bolt wire
// against a SimServer (scenario.go:182-199), and graph/generation has no wire
// surface at all — it is an in-process library API. The same pairing is what
// readtx-isolation uses (durable_scenarios.go:604-613). No new ExecMode is
// warranted: a new mode would need its own String, Reproducible and dispatch
// arm and would behave identically to this pairing.
func generationSwapScenario() Scenario {
	return Scenario{
		Name: ScenarioGenerationSwap,
		Description: "graph/generation's lock-free CSR publisher under concurrent readers: every acquisition's " +
			"traversal must match the model's independently computed adjacency for the generation its own " +
			"content declares (a torn swap is caught by identity, not by well-formedness); every generation's " +
			"refcount is audited at quiescence; a drain with a reference held returns ErrDrainTimeout without " +
			"corrupting Current while the forced unbounded drain beside it completes; and Close drains live " +
			"readers with none left wedged",
		Mode:        ModeConcurrent,
		DefaultSeed: generationSwapDefaultSeed,
		Connections: genSwapDefaultReaders,
		OpsPerConn:  genSwapDefaultMaxOps,
		run:         runGenerationSwapScenario,
	}
}

// runGenerationSwapScenario is the scenario's run override: collect the
// evidence, then adjudicate it against the contract and the non-vacuity
// gate.
func runGenerationSwapScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, err := RunGenerationSwap(ctx, DefaultGenerationSwapConfig(seed))
	if err != nil {
		return nil, err
	}
	v := append(checkGenerationSwap(ev), checkGenerationSwapNonVacuity(ev)...)
	if len(v) == 0 {
		return nil, nil
	}
	return generationSwapReport(seed, ev, v), nil
}

// generationSwapReport wraps violations in a scenario report. It PANICS on an
// empty violation slice: a non-nil report that names nothing is a reporting
// defect, which SimReport.String shouts about and report_render_test pins.
func generationSwapReport(seed uint64, ev *GenerationSwapEvidence, v []Violation) *SimReport {
	if len(v) == 0 {
		panic("sim: generationSwapReport called with no violations; a report must always name what it found")
	}
	// The plan digest rides in the report because it is the one thing a
	// concurrent-mode run CAN promise to reproduce from the seed. The
	// interleaving-dependent counters ride along as context, never as a claim.
	return &SimReport{
		Scenario:   ScenarioGenerationSwap,
		Mode:       ModeConcurrent,
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<generation swap: " + ev.String() + ">"},
		Violations: v,
	}
}
