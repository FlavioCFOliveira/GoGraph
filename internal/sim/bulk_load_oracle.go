package sim

// bulk_load_oracle.go — the offline bulk loader's FULL contract, adjudicated
// against an independent harness model (rmp #2488).
//
// [bulk.Loader] is the module's non-transactional ingest path: it streams
// (src, dst, weight) records into an in-memory adjacency, builds an immutable
// CSR, and publishes it as a Tier 2 csrfile. Until this scenario the DST touched
// three of its methods — New, Add, Finalise — and threw the result away. The
// `bulk-vs-online` scenario (see [runBulkVsOnline]) drives 20 000 Add calls
// beside transactional writes and then compares ONE number, the row count,
// against the constant it just used to generate them; the returned CSR is
// discarded, the csrfile goes to a real OS tempdir that no fault can reach, and
// the loader is only ever configured Directed+Multigraph. That scenario is a
// concurrency/resource-stability watch and remains one; it is deliberately left
// intact.
//
// This scenario occupies the content and fault gap beside it. Every arm below
// adjudicates the LOADED GRAPH — not a row count — against a model this package
// computes for itself, and every publication goes through a [SimDisk] so the
// atomicity of the publish can be attacked.
//
// # What package `bulk` already tests for itself, and what this adds
//
// `store/bulk` is not untested, and this scenario is not a copy of its tests.
// In-package there is already `TestCSRDirect_CsrfileByteIdentical` (directed,
// both multigraph settings), `TestParallel_IdenticalToSequential`,
// `TestParallel_StableAcrossRuns` and `TestLoader_CtxCancelMidDrain`. What the
// DST adds is of a different kind:
//
//   - AN EXTERNAL ORACLE. The in-package identity tests are differentials
//     between two ENGINE code paths — `TestCSRDirect_IdenticalToBuildFromAdjList`
//     builds an [adjlist.AdjList] itself and compares `csr.BuildFromAdjList` of
//     it against `buildCSRDirect`. A differential cannot see a defect the two
//     sides share, and validating the engine with the engine is what the DST's
//     conventions forbid. [bulkOracleModel] is computed outside both, so a fault
//     common to every builder is visible to it.
//
//   - THE OTHER THREE CONFIGURATIONS. In-package identity coverage is directed
//     only, which is correct for its purpose. Here all four
//     Directed x Multigraph configurations are adjudicated, including the
//     undirected ones whose transparent fallback is itself a contract.
//
//   - FAULTS AND A CRASH. The in-package tests publish to real temporary
//     directories, so none of them can fail an fsync, a rename or a
//     parent-directory fsync, and none can crash the host. Every fault arm below
//     is reachable only through the [SimDisk] seam.
//
//   - CONTENT AFTER A PARTIAL INGEST. Crossing MaxRows is tested in-package for
//     its error; here the surviving prefix is compared to the model, which is the
//     half that says the partial ingest kept the RIGHT edges.
//
// # What the model is, and what it does NOT certify
//
// [bulkOracleModel] reimplements the loader's documented ingest contract in
// plain Go: per-edge interning order, simple-graph first-occurrence dedup,
// undirected mirroring with the self-loop exception, and the stable
// order-by-destination each row ends in. It calls into NEITHER
// `graph/adjlist` nor `csr.OrderRuns` — its sort is `slices.SortStableFunc` —
// so an adjacency or ordering defect cannot cancel itself out.
//
// One primitive is NOT independent and must not be read as certified here: the
// NodeID a key receives. [graph.Mapper] assigns ids first-seen-per-shard from a
// hash of the key, which the harness cannot derive without reimplementing the
// hash. The model therefore interns through its OWN [graph.Mapper] instance,
// Src then Dst per edge in input order, which is the assignment rule
// `buildCSRDirect`'s doc comment states. What the model certifies is the edge
// multiset, the dedup, the mirroring, the within-row ordering, the row cap, the
// streaming contracts and the publication's atomicity — all of it INDEXED BY
// whatever ids the Mapper chose. A Mapper that assigned ids differently but
// consistently would pass; a loader that lost, duplicated, reordered or
// mis-weighted an edge would not.
//
// # Which build path each arm actually reaches
//
// [bulk.Loader.Finalise] dispatches on `Parallel && csrDirectEligible()` FIRST,
// and `csrDirectEligible()` is `Directed && adj.Config().MaxShardCapacity == 0`.
// The public [bulk.Options] exposes NO shard-capacity knob, so for any directed
// load configured from outside package `bulk` the predicate is unconditionally
// true. The eight (Directed x Multigraph x Parallel) combinations this scenario
// runs therefore reach exactly three builders:
//
//	Directed,   Parallel=false -> csr.BuildFromAdjList over the Add-time adjacency
//	Directed,   Parallel=true  -> Loader.buildCSRDirect (the counting sort)
//	Undirected, Parallel=false -> csr.BuildFromAdjList over the Add-time adjacency
//	Undirected, Parallel=true  -> Loader.buildBuffered -> buildSequential -> BuildFromAdjList
//
// The undirected+Parallel pair is worth naming: `Parallel && !Directed` is the
// ONLY externally reachable route into the buffered-replay branch, which exists
// for a capacity-capped adjacency no public Options can construct.
//
// # The fault and concurrency regimes this scenario CANNOT reach, and why
//
// Read this before assuming the loader is fully fault- and parallel-covered. It
// is not.
//
//   - THE GOROUTINE FAN-OUT IS UNREACHABLE. `Loader.buildParallel` — the
//     phase-1-intern / phase-2-partition-by-Mapper-shard fan-out across bounded
//     goroutines — is gated by `parallelEligible()`, which requires Directed AND
//     `len(buffered) >= parallelMinEdges` (50 000). But a directed load never
//     gets that far: the CSR-direct case is matched first and wins. The fan-out
//     is reachable only IN-package, with `MaxShardCapacity > 0` injected
//     directly into the adjacency, which is what `store/bulk`'s own
//     parallel_atomicity_test.go does. `parallelMinEdges` is an unexported var
//     the simulator cannot lower.
//
//     Consequently the byte-identity arm below certifies the PRODUCTION parallel
//     path — `buildCSRDirect` against `BuildFromAdjList` — and nothing about the
//     multi-goroutine build. That is the comparison that matters for a caller
//     setting `Parallel: true`, because it is the code such a caller runs. It is
//     not a statement about goroutine determinism.
//     [TestBulkLoadOracle_ParallelFanOutStillUnreachable] fails if a future
//     revision adds a shard-capacity knob to [bulk.Options], because that would
//     silently make this paragraph stale.
//
//   - THE LOADER'S OWN PUBLICATION CANNOT BE FAULTED. `Finalise` publishes
//     through [csrfile.WriteToFile], which binds the OS backend at its entry
//     point. The fault arms below therefore call
//     [csrfile.WriteToFileWith] over a [SimDisk] instead. That is the SAME
//     writer core — `WriteToFile` and `WriteToFileWith` both tail-call
//     `writeToFileWith`, differing only in the `fs` value — so the write /
//     fsync / rename / parent-fsync protocol under test is byte-for-byte the one
//     `Finalise` runs. What is NOT covered is the `Finalise` -> `WriteToFile`
//     call edge itself under a fault; the closest reachable substitute, an
//     unwritable OutputPath, is exercised by [bulkOracleArmRealFS] and pins the
//     error wrapping and the fact that the built CSR is still returned.
//
//   - A CRASH CANNOT LAND INSIDE THE BUILD. The build completes wholly in
//     memory before the single publication, so there is no partial-CSR state to
//     crash into. The crash window this scenario does attack is the one that
//     exists on disk: between the publish rename and the parent-directory fsync.
//
//   - `CrashProcess` CANNOT FALSIFY THE PARENT FSYNC. A SIGKILL discards
//     nothing (see [SimDisk.CrashProcess]), so it can only ever confirm "never
//     torn". [bulkOracleArmCrashWindow] therefore uses [SimDisk.CrashHost] —
//     power-loss semantics — which is the only crash that can revoke a dirent.
//
//   - THE IMPOSSIBLE OUTCOME IS NEVER CHARGED TO THE ENGINE.
//     [SimDisk.ArmRenameRevokeBothForPath] deliberately produces the physically
//     impossible neither-name result as a harness self-test. This scenario never
//     arms it. The both-names-absent state it DOES assert is the legal one: a
//     rolled-back rename whose source (`<path>.tmp`) never had a durable dirent
//     of its own.
//
// # Why the post-fault oracle admits THREE outcomes, not two
//
// A reader of the published path must observe exactly one of: the path is
// ABSENT; the path is present and reconstructs the expected graph EXACTLY; or
// the path is present and is REJECTED by the reader. The third is legal and
// reachable: with `faultRate > 0` a written sector is silently corrupted, so
// [csrfile.WriteToFileWith] can return nil over an image whose stored CRC no
// longer matches its bytes. Media corruption after a correct publication is not
// an atomicity failure — it is what the checksum exists to catch. The state that
// is FORBIDDEN, and that [bulkOracleAdjudicateImage] exists to catch, is the
// fourth: present, accepted, and DIFFERENT.
//
// A rejection is not required to be a CRC failure. [csrfile.DecodeHeader]
// validates magic, version, the byte-order byte and the weight kind BEFORE the
// tail CRC is ever computed, so corruption inside bytes 0..7 or at byte 24
// surfaces as [csrfile.ErrBadMagic], [csrfile.ErrUnsupportedVersion],
// [csrfile.ErrUnsupportedByteOrder] or [csrfile.ErrUnknownWeightKind] instead.
// A TRUNCATED image is rejected by neither: the mechanism that stops it is the
// exact `total == fileLen` equality in `Header.validate`, which yields
// [csrfile.ErrHeaderInconsistent] deterministically rather than probabilistically
// — a torn publication is caught by size, not by checksum. The oracle accepts
// the whole set (see [bulkOracleRejection]); pinning it to
// [csrfile.ErrFileCorrupted] alone would report false failures.
//
// # Why the published path is a SUBDIRECTORY key
//
// [SimDisk] treats any path whose parent is "." or "/" as having a DURABLE
// dirent from the instant of creation (see `isRootLevel`), because the
// root-level WAL is governed by the data-durability model rather than the
// dirent model. Publishing to a root-level key would therefore make the rename
// un-rollbackable and the parent-directory fsync unobservable, and every "the
// name survived the crash" assertion below would pass while proving nothing.
// [bulkOraclePath] is a subdirectory key for exactly that reason, and
// [bulkOracleArmCrashWindow] asserts the non-vacuity observables
// ([SimDisk.PendingRenameCount], [SimDisk.LastCrashRenameOutcome]) that make the
// window provably entered rather than assumed.
//
// # A leftover temp file is an observation, not a violation
//
// Nothing in the module enumerates the publish directory — the only non-test
// `os.ReadDir` under `store/` is store/bulkimport/publish.go's, on an unrelated
// path — so a stranded `<path>.tmp` is invisible to every reader and is never
// reclaimed. (Measured on the default seed: zero of the five armed publish
// faults strands one, because every failure path in `writeToFileWith` removes
// the temp file before returning.) It
// is counted into the evidence ([bulkOracleEvidence.strandedTemps]) and reported,
// never raised as a [Violation].

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/store/bulk"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// ScenarioBulkLoadOracle is the catalogue key of the bulk-loader full-contract
// scenario.
const ScenarioBulkLoadOracle = "bulk-load-oracle"

// bulkOraclePath is the SimDisk key every publication in this scenario targets.
// It is deliberately a SUBDIRECTORY key: a root-level key would be treated as
// durably linked on creation and would make the whole crash-window arm vacuous
// (see the file header).
const bulkOraclePath = "bulk/oracle.csr"

// The fixture's shape. It is sized so every arm drives a genuinely non-trivial
// multigraph while the whole scenario stays a small fraction of the short-layer
// budget.
const (
	// bulkOracleKeys is the size of the distinct node-key universe the drawn
	// edges sample from. It is well below the Mapper's 256 shards, so the NodeID
	// space stays sparse and the offsets array spans the gaps — the shape a
	// closed-form model of the vertices array has to get right.
	bulkOracleKeys = 48
	// bulkOracleDrawnEdges is the number of seed-drawn edge records appended
	// after the guaranteed skeleton.
	bulkOracleDrawnEdges = 900
	// bulkOracleRowCap is the MaxRows cap the cap-crossing arms configure. It is
	// strictly between 1 and the fixture's length by construction, so the cap is
	// always crossed and the partial-ingest contract is always observed.
	bulkOracleRowCap = 400
	// bulkOracleCancelAt is the number of edges Drain consumes before the
	// mid-stream context cancellation. Strictly between 1 and the fixture length.
	bulkOracleCancelAt = 250
	// bulkOracleBatchSize is the AddBatch chunk length.
	bulkOracleBatchSize = 64
	// bulkOracleFaultRate is the per-sector / per-Sync fault probability of the
	// media-fault arm's disk. No clause is conditioned on the draw: the arm
	// asserts only the three-way legal set, so it holds for every seed.
	bulkOracleFaultRate = 0.30
	// bulkOracleFaultAttempts is how many publications the media-fault arm makes
	// onto its faulty disk.
	bulkOracleFaultAttempts = 8
)

// Non-vacuity floors. They sit deliberately BELOW the fixture's exact counts —
// which are themselves asserted against the model and against a closed form —
// so that a fixture which silently degenerated to a handful of records cannot
// pass as coverage.
const (
	bulkOracleMinEdgeRecords  = 800
	bulkOracleMinPublishBytes = 4096
	bulkOracleMinDistinctKeys = 24
)

// -----------------------------------------------------------------------------
// The independent model
// -----------------------------------------------------------------------------

// bulkOracleEntry is one stored adjacency entry: a destination NodeID and the
// weight that travelled with it.
type bulkOracleEntry struct {
	dst graph.NodeID
	w   int64
}

// bulkOracleShape is the CSR the model says the loader must produce: the
// offsets array over the whole NodeID space, the destination and weight columns
// in row-major NodeID order, and the order/size scalars.
type bulkOracleShape struct {
	vertices []uint64
	edges    []graph.NodeID
	weights  []int64
	order    uint64
	size     uint64
}

// bulkOracleModel is the harness's independent reimplementation of the bulk
// loader's documented ingest contract.
//
// It reproduces four rules, each read off the contract rather than off the
// implementation:
//
//  1. Size counts stored adjacency ENTRIES, not logical edges, so an undirected
//     non-self-loop edge contributes two.
//  2. Simple-graph mode keeps the FIRST occurrence of an endpoint pair and its
//     weight, and drops every later one ([adjlist.Config].Multigraph: "repeated
//     AddEdge calls on the same endpoint pair are idempotent").
//  3. Undirected mode mirrors each edge onto (dst, src), EXCEPT a self-loop,
//     which is stored once.
//  4. Each row ends stably ordered by destination NodeID, with equal
//     destinations left in ingest order (rmp #2141).
//
// Rule 2 is implemented as "row s already holds an entry for d", which is the
// literal reading of the adjacency's idempotence clause. For an undirected graph
// that coincides with unordered-pair dedup, because mirroring keeps the rows
// symmetric — but the per-row form is the one the contract states, so it is the
// one modelled.
//
// # Concurrency contract
//
// A bulkOracleModel is driven from a single goroutine and is not safe for
// concurrent use.
type bulkOracleModel struct {
	mapper *graph.Mapper[string]
	rows   map[graph.NodeID][]bulkOracleEntry
	stored map[[2]graph.NodeID]struct{}

	entries     int
	dupsDropped int
	mirrored    int
	selfLoops   int

	directed   bool
	multigraph bool
}

// newBulkOracleModel returns an empty model for one adjacency configuration.
func newBulkOracleModel(directed, multigraph bool) *bulkOracleModel {
	return &bulkOracleModel{
		mapper:     graph.NewMapper[string](),
		rows:       make(map[graph.NodeID][]bulkOracleEntry, bulkOracleKeys),
		stored:     make(map[[2]graph.NodeID]struct{}, bulkOracleDrawnEdges),
		directed:   directed,
		multigraph: multigraph,
	}
}

// add ingests one edge record, interning Src then Dst in that order so the model
// fixes the same NodeIDs the loader does.
func (m *bulkOracleModel) add(e bulk.Edge) {
	s := m.mapper.Intern(e.Src)
	d := m.mapper.Intern(e.Dst)
	if s == d {
		m.selfLoops++
	}
	m.store(s, d, e.Weight, false)
	if !m.directed && s != d {
		m.store(d, s, e.Weight, true)
	}
}

// store appends one adjacency entry, applying simple-graph dedup. mirror marks
// the entry as the reverse half of an undirected edge, for the coverage counter.
func (m *bulkOracleModel) store(s, d graph.NodeID, w int64, mirror bool) {
	if !m.multigraph {
		key := [2]graph.NodeID{s, d}
		if _, dup := m.stored[key]; dup {
			m.dupsDropped++
			return
		}
		m.stored[key] = struct{}{}
	}
	m.rows[s] = append(m.rows[s], bulkOracleEntry{dst: d, w: w})
	m.entries++
	if mirror {
		m.mirrored++
	}
}

// expect renders the model as the CSR shape the loader must have produced.
//
// The offsets array spans [0, MaxNodeID] so that absent ("ghost") NodeIDs get
// their natural zero-width slot, which is the layout csr.BuildFromAdjList
// produces and the one the csrfile header's NVertices counts.
func (m *bulkOracleModel) expect() *bulkOracleShape {
	order := uint64(m.mapper.Len())
	maxID := uint64(m.mapper.MaxNodeID())
	if maxID == 0 {
		// The empty-graph shape: a single zero offset, no columns.
		return &bulkOracleShape{vertices: []uint64{0}, order: order}
	}

	vertices := make([]uint64, maxID+1)
	var total uint64
	for id := uint64(0); id < maxID; id++ {
		vertices[id] = total
		total += uint64(len(m.rows[graph.NodeID(id)]))
	}
	vertices[maxID] = total

	edges := make([]graph.NodeID, 0, total)
	weights := make([]int64, 0, total)
	for id := uint64(0); id < maxID; id++ {
		row := m.rows[graph.NodeID(id)]
		if len(row) == 0 {
			continue
		}
		ordered := make([]bulkOracleEntry, len(row))
		copy(ordered, row)
		// The harness's OWN stable sort. Deliberately not csr.OrderRuns: the
		// within-row ordering is one of the properties under test, so borrowing
		// the engine's sort would let an ordering defect cancel itself out.
		slices.SortStableFunc(ordered, func(a, b bulkOracleEntry) int {
			switch {
			case a.dst < b.dst:
				return -1
			case a.dst > b.dst:
				return 1
			default:
				return 0
			}
		})
		for k := range ordered {
			edges = append(edges, ordered[k].dst)
			weights = append(weights, ordered[k].w)
		}
	}
	return &bulkOracleShape{vertices: vertices, edges: edges, weights: weights, order: order, size: total}
}

// buildBulkOracleModel folds an edge stream into a fresh model.
func buildBulkOracleModel(edges []bulk.Edge, directed, multigraph bool) *bulkOracleModel {
	m := newBulkOracleModel(directed, multigraph)
	for k := range edges {
		m.add(edges[k])
	}
	return m
}

// bulkOracleClosedFormSize derives the number of stored adjacency entries a
// SECOND way, from the edge stream alone, without building rows or sorting.
//
// It exists so the model's own count cannot be the only witness to itself: a
// model whose two views of its edge set disagree fails the cross-check rather
// than passing on whichever view happens to be wrong. The four cases are the
// documented rules read as arithmetic — every record for a directed multigraph,
// every record plus a mirror except self-loops for an undirected one, and the
// distinct-pair cardinalities for the simple variants.
func bulkOracleClosedFormSize(edges []bulk.Edge, directed, multigraph bool) uint64 {
	if multigraph {
		loops := 0
		for k := range edges {
			if edges[k].Src == edges[k].Dst {
				loops++
			}
		}
		if directed {
			return uint64(len(edges))
		}
		return uint64(2*len(edges) - loops)
	}

	// Simple graph: count DISTINCT keys over the string endpoints. Interning is
	// injective, so string identity and NodeID identity coincide.
	type pair struct{ a, b string }
	seen := make(map[pair]struct{}, len(edges))
	var stored uint64
	note := func(a, b string) {
		p := pair{a: a, b: b}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		stored++
	}
	for k := range edges {
		e := edges[k]
		note(e.Src, e.Dst)
		if !directed && e.Src != e.Dst {
			note(e.Dst, e.Src)
		}
	}
	return stored
}

// -----------------------------------------------------------------------------
// The fixture
// -----------------------------------------------------------------------------

// bulkOracleFixture is one run's edge stream together with the structural
// features it GUARANTEES for every seed, so that no coverage gate below can be
// satisfied by luck.
type bulkOracleFixture struct {
	edges []bulk.Edge
	// duplicatePairs, selfLoops, reversedPairs and distinctKeys are the
	// guaranteed-by-construction features, counted rather than assumed.
	duplicatePairs int
	selfLoops      int
	reversedPairs  int
	distinctKeys   int
}

// bulkOracleKey renders the i-th node key.
func bulkOracleKey(i int) string { return fmt.Sprintf("bk%03d", i) }

// buildBulkOracleFixture draws one run's edge stream.
//
// The stream is a guaranteed SKELETON followed by seed-drawn records and then a
// repeat of the skeleton's head. The skeleton pins every structural feature the
// coverage gates require — a duplicate pair, a self-loop, a reversed pair,
// negative and extremal weights — so those gates are structural rather than
// probabilistic; the drawn body makes the graph non-trivial; the repeated tail
// puts a duplicate at the END of the stream as well as in its head, so a dedup
// that only inspected a prefix would be caught.
//
// tame drops every structural feature (strictly increasing distinct ordered
// pairs, no duplicate, no loop, no reverse). It is the falsifiability seam for
// the coverage gates themselves: with it, they must go red.
func buildBulkOracleFixture(s *Seed, tame bool) bulkOracleFixture {
	k0, k1, k2 := bulkOracleKey(0), bulkOracleKey(1), bulkOracleKey(2)

	var edges []bulk.Edge
	if tame {
		// Enumerate strictly-increasing pairs (a < b) once each, in order. There
		// are bulkOracleKeys*(bulkOracleKeys-1)/2 = 1128 of them for 48 keys, more
		// than bulkOracleDrawnEdges, so the walk never wraps: no pair repeats, no
		// self-loop occurs, and (b,a) never follows (a,b).
		edges = make([]bulk.Edge, 0, bulkOracleDrawnEdges)
		lo, hi := 0, 1
		for i := 0; i < bulkOracleDrawnEdges; i++ {
			edges = append(edges, bulk.Edge{Src: bulkOracleKey(lo), Dst: bulkOracleKey(hi), Weight: int64(i)})
			hi++
			if hi >= bulkOracleKeys {
				lo++
				hi = lo + 1
			}
		}
	} else {
		skeleton := []bulk.Edge{
			{Src: k0, Dst: k1, Weight: 1},
			{Src: k1, Dst: k2, Weight: -2},
			{Src: k0, Dst: k1, Weight: 3},  // duplicate of the first record
			{Src: k2, Dst: k2, Weight: -4}, // self-loop: mirrored ONCE
			{Src: k1, Dst: k0, Weight: 5},  // reverse of the first record
			// Extremal weights: the csrfile carries int64 weights in the
			// 8-byte section the reader hands back as uint64, so a sign or
			// reinterpretation defect shows up here and nowhere else.
			{Src: k0, Dst: k2, Weight: -9223372036854775808},
			{Src: k2, Dst: k0, Weight: 9223372036854775807},
		}
		edges = make([]bulk.Edge, 0, len(skeleton)+bulkOracleDrawnEdges+2)
		edges = append(edges, skeleton...)
		for i := 0; i < bulkOracleDrawnEdges; i++ {
			src := bulkOracleKey(int(s.Uint64N(bulkOracleKeys)))
			dst := bulkOracleKey(int(s.Uint64N(bulkOracleKeys)))
			// Weights straddle zero so the weight column is not monotone with
			// the row order and cannot be reproduced by accident.
			edges = append(edges, bulk.Edge{Src: src, Dst: dst, Weight: int64(s.Uint64N(1<<20)) - (1 << 19)})
		}
		edges = append(edges, skeleton[0], skeleton[1])
	}

	f := bulkOracleFixture{edges: edges}
	type pair struct{ a, b string }
	seen := make(map[pair]int, len(edges))
	keys := make(map[string]struct{}, bulkOracleKeys)
	for k := range edges {
		e := edges[k]
		keys[e.Src] = struct{}{}
		keys[e.Dst] = struct{}{}
		if e.Src == e.Dst {
			f.selfLoops++
		}
		if seen[pair{a: e.Src, b: e.Dst}] > 0 {
			f.duplicatePairs++
		}
		if seen[pair{a: e.Dst, b: e.Src}] > 0 && e.Src != e.Dst {
			f.reversedPairs++
		}
		seen[pair{a: e.Src, b: e.Dst}]++
	}
	f.distinctKeys = len(keys)
	return f
}

// -----------------------------------------------------------------------------
// Adjudication
// -----------------------------------------------------------------------------

// bulkOracleRejection reports whether err is one of the reader's structural
// refusals — the FULL set, not just the tail-CRC failure.
//
// [csrfile.DecodeHeader] validates magic, version, byte order and weight kind
// before the CRC is computed, and `Header.validate` rejects a wrong FILE SIZE
// with [csrfile.ErrHeaderInconsistent], so a damaged or truncated image can
// surface under any of these sentinels. Testing only for
// [csrfile.ErrFileCorrupted] would report a legal rejection as a violation.
func bulkOracleRejection(err error) bool {
	switch {
	case errors.Is(err, csrfile.ErrFileCorrupted), // also covers ErrHeaderInconsistent, which wraps it
		errors.Is(err, csrfile.ErrHeaderTooShort),
		errors.Is(err, csrfile.ErrBadMagic),
		errors.Is(err, csrfile.ErrUnsupportedVersion),
		errors.Is(err, csrfile.ErrUnsupportedByteOrder),
		errors.Is(err, csrfile.ErrUnknownWeightKind):
		return true
	default:
		return false
	}
}

// bulkOracleOutcome is one of the three legal post-fault states of a published
// path.
type bulkOracleOutcome uint8

// The three legal outcomes. Any fourth state — present, accepted, and different
// from the model — is a violation.
const (
	// bulkOracleAbsent: the path does not exist.
	bulkOracleAbsent bulkOracleOutcome = iota
	// bulkOracleComplete: the path exists and reconstructs the expected graph.
	bulkOracleComplete
	// bulkOracleRejected: the path exists and the real reader refuses it.
	bulkOracleRejected
)

// String renders an outcome for a report or a log line.
func (o bulkOracleOutcome) String() string {
	switch o {
	case bulkOracleAbsent:
		return "absent"
	case bulkOracleComplete:
		return "complete"
	case bulkOracleRejected:
		return "rejected"
	default:
		return fmt.Sprintf("bulkOracleOutcome(%d)", uint8(o))
	}
}

// bulkOracleImageFS serves ONE fixed byte image to [csrfile.OpenWith] and
// refuses every other operation.
//
// It exists so an arm can adjudicate the DURABLE image — what a host crash would
// leave behind — through the real reader, rather than the live image
// [simCSRFS.ReadFile] returns. Asserting on the durable bytes is the rule for
// any crash arm: a publisher's own return value describes a process that real
// power loss would have ended.
type bulkOracleImageFS struct{ img []byte }

// errBulkOracleReadOnly is returned by every write method of
// [bulkOracleImageFS]: the backend exists to serve one image to the reader and
// must never be mistaken for a writable filesystem.
var errBulkOracleReadOnly = errors.New("sim: bulk-load-oracle image backend is read-only")

func (b bulkOracleImageFS) Create(string) (csrfile.File, error) { return nil, errBulkOracleReadOnly }

func (b bulkOracleImageFS) Rename(string, string) error { return errBulkOracleReadOnly }

func (b bulkOracleImageFS) Remove(string) error { return errBulkOracleReadOnly }

func (b bulkOracleImageFS) ReadFile(string) ([]byte, error) { return b.img, nil }

func (b bulkOracleImageFS) ParentDirSync(string) error { return nil }

// crc32Castagnoli returns the CRC32C the csrfile format stores in its tail. It
// is used ONLY by [bulkOracleRepairCRC], to manufacture the self-consistent
// wrong image that proves the content oracle is not resting on the checksum.
func crc32Castagnoli(b []byte) uint32 {
	return crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))
}

// bulkOracleVerdict is the adjudication of one image of the published path: which
// of the three legal outcomes it landed in, the reader's refusal when it was
// refused, and any violation the adjudication found.
type bulkOracleVerdict struct {
	rejectErr error
	v         []Violation
	outcome   bulkOracleOutcome
}

// bulkOracleCheckCSR compares an in-memory CSR against the model's shape.
func bulkOracleCheckCSR(tag string, want *bulkOracleShape, c *csr.CSR[int64]) []Violation {
	op := "<bulk-load-oracle:" + tag + ">"
	if c == nil {
		return []Violation{{
			Kind: ViolationOracleDeviation, Op: op,
			Message: fmt.Sprintf("[%s] Finalise returned a nil CSR", tag),
		}}
	}
	var v []Violation
	if got := c.Order(); got != want.order {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: op,
			Message: fmt.Sprintf("[%s] CSR order = %d, model says %d", tag, got, want.order),
		})
	}
	if got := c.Size(); got != want.size {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: op,
			Message: fmt.Sprintf("[%s] CSR size = %d, model says %d (stored adjacency entries)", tag, got, want.size),
		})
	}
	if !slices.Equal(c.VerticesSlice(), want.vertices) {
		v = append(v, Violation{
			Kind: ViolationGraphIntegrity, Op: op,
			Message: fmt.Sprintf("[%s] CSR offsets array differs from the model: len %d vs %d",
				tag, len(c.VerticesSlice()), len(want.vertices)),
		})
	}
	if !slices.Equal(c.EdgesSlice(), want.edges) {
		v = append(v, Violation{
			Kind: ViolationGraphIntegrity, Op: op,
			Message: fmt.Sprintf("[%s] CSR edge multiset differs from the model: len %d vs %d",
				tag, len(c.EdgesSlice()), len(want.edges)),
		})
	}
	if !slices.Equal(c.WeightsSlice(), want.weights) {
		v = append(v, Violation{
			Kind: ViolationGraphIntegrity, Op: op,
			Message: fmt.Sprintf("[%s] CSR weight column differs from the model: len %d vs %d",
				tag, len(c.WeightsSlice()), len(want.weights)),
		})
	}
	if !c.RunsOrdered() {
		v = append(v, Violation{
			Kind: ViolationGraphIntegrity, Op: op,
			Message: fmt.Sprintf("[%s] CSR rows are not ordered by destination", tag),
		})
	}
	return v
}

// bulkOracleCheckReader compares a reopened csrfile against the model's shape,
// header fields included.
func bulkOracleCheckReader(tag string, want *bulkOracleShape, r *csrfile.Reader) []Violation {
	op := "<bulk-load-oracle:" + tag + ">"
	var v []Violation
	h := r.Header()
	if h.NVertices != uint64(len(want.vertices)) {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: op,
			Message: fmt.Sprintf("[%s] header NVertices = %d, model offsets array is %d long",
				tag, h.NVertices, len(want.vertices)),
		})
	}
	if h.NEdges != want.size {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: op,
			Message: fmt.Sprintf("[%s] header NEdges = %d, model says %d", tag, h.NEdges, want.size),
		})
	}
	if h.Weight != csrfile.WeightUint64 {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: op,
			Message: fmt.Sprintf("[%s] header weight kind = %d, an int64 CSR must publish as the 8-byte kind %d",
				tag, h.Weight, csrfile.WeightUint64),
		})
	}
	if !slices.Equal(r.Vertices(), want.vertices) {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: op,
			Message: fmt.Sprintf("[%s] reopened offsets array differs from the model", tag),
		})
	}
	if !slices.Equal(r.Edges(), want.edges) {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: op,
			Message: fmt.Sprintf("[%s] reopened edge multiset differs from the model", tag),
		})
	}
	raw, ok := r.WeightsUint64()
	if !ok {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: op,
			Message: fmt.Sprintf("[%s] reopened csrfile exposes no 8-byte weight column", tag),
		})
		return v
	}
	if len(raw) != len(want.weights) {
		return append(v, Violation{
			Kind: ViolationACIDConsistency, Op: op,
			Message: fmt.Sprintf("[%s] reopened weight column is %d long, model says %d",
				tag, len(raw), len(want.weights)),
		})
	}
	for i := range raw {
		// The 8-byte section carries the int64 two's-complement image, which the
		// reader hands back as uint64; reinterpreting is the round trip.
		if int64(raw[i]) != want.weights[i] {
			v = append(v, Violation{
				Kind: ViolationACIDConsistency, Op: op,
				Message: fmt.Sprintf("[%s] reopened weight[%d] = %d, model says %d",
					tag, i, int64(raw[i]), want.weights[i]),
			})
			break
		}
	}
	return v
}

// bulkOracleAdjudicateImage classifies one byte image of the published path into
// the three legal outcomes, and returns a violation for the forbidden fourth:
// present, accepted by the real reader, and DIFFERENT from the model.
//
// img == nil means the path was absent. The image is opened through
// [bulkOracleImageFS] so the caller chooses which image is adjudicated — the
// live one or, for a crash arm, the durable one.
func bulkOracleAdjudicateImage(tag string, want *bulkOracleShape, img []byte) bulkOracleVerdict {
	if img == nil {
		return bulkOracleVerdict{outcome: bulkOracleAbsent}
	}
	r, err := csrfile.OpenWith(bulkOracleImageFS{img: img}, bulkOraclePath)
	if err != nil {
		if bulkOracleRejection(err) {
			return bulkOracleVerdict{outcome: bulkOracleRejected, rejectErr: err}
		}
		return bulkOracleVerdict{outcome: bulkOracleRejected, rejectErr: err, v: []Violation{{
			Kind: ViolationACIDConsistency, Op: "<bulk-load-oracle:" + tag + ">",
			Message: fmt.Sprintf("[%s] the reader refused the published image with an error outside the"+
				" documented rejection set: %v", tag, err),
		}}}
	}
	defer func() { _ = r.Close() }()
	return bulkOracleVerdict{outcome: bulkOracleComplete, v: bulkOracleCheckReader(tag, want, r)}
}

// bulkOracleReadLive returns the live image of path, or nil when absent.
func bulkOracleReadLive(disk *SimDisk, path string) ([]byte, []Violation) {
	img, err := disk.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []Violation{{
			Kind: ViolationACIDDurability, Op: "<bulk-load-oracle:read>",
			Message: "reading the published path failed for a reason other than absence: " + err.Error(),
		}}
	}
	return img, nil
}

// bulkOracleReadDurable returns the DURABLE image of path — the bytes a
// [SimDisk.CrashHost] would leave — or nil when the path is absent.
func bulkOracleReadDurable(disk *SimDisk, path string) ([]byte, []Violation) {
	img, err := disk.DurableImage(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []Violation{{
			Kind: ViolationACIDDurability, Op: "<bulk-load-oracle:durable-read>",
			Message: "reading the durable image of the published path failed for a reason other than absence: " + err.Error(),
		}}
	}
	return img, nil
}

// bulkOraclePublish publishes c onto disk through the csrfile filesystem seam.
// It is the same writer core [bulk.Loader.Finalise] runs; only the backend
// differs (see the file header).
func bulkOraclePublish(disk *SimDisk, path string, c *csr.CSR[int64]) error {
	_, err := csrfile.WriteToFileWith[int64](simCSRFS{disk: disk}, path, c)
	return err
}

// bulkOracleLoad streams edges through a freshly configured loader and finalises
// it WITHOUT an OutputPath, so the arm owns the publication and can fault it.
func bulkOracleLoad(edges []bulk.Edge, directed, multigraph, parallel bool) (int, *csr.CSR[int64], error) {
	l := bulk.New(bulk.Options{
		Directed: directed, Multigraph: multigraph, Parallel: parallel,
		ExpectNodes: bulkOracleKeys,
	})
	for k := range edges {
		if err := l.Add(edges[k]); err != nil {
			return l.Rows(), nil, fmt.Errorf("sim: bulk-load-oracle add: %w", err)
		}
	}
	return l.Finalise()
}

// -----------------------------------------------------------------------------
// Evidence and options
// -----------------------------------------------------------------------------

// bulkOracleConfigEvidence is what one (Directed x Multigraph) configuration
// measured.
type bulkOracleConfigEvidence struct {
	name string
	// seqBytes / parBytes are the published csrfile image lengths for
	// Parallel=false and Parallel=true.
	seqBytes, parBytes int
	// byteIdentical records whether the two published images are equal
	// byte-for-byte.
	byteIdentical bool
	// sliceIdentical records whether the two in-memory CSRs are equal on all
	// three columns plus order/size.
	sliceIdentical bool
	entries        int
	dupsDropped    int
	mirrored       int
	selfLoops      int
	closedFormSize uint64
	modelSize      uint64
	rowsIngested   int
}

// bulkOracleEvidence is what one run measured, for the non-vacuity gates. It
// carries no filesystem path, so two runs of the same seed produce the same
// evidence even though each ran in a different temporary directory.
type bulkOracleEvidence struct {
	configs []bulkOracleConfigEvidence
	fixture bulkOracleFixture

	// Streaming arm.
	drainClean          int
	drainCancelled      int
	drainCancelRows     int
	drainCancelErrIsCtx bool
	batchRows           int

	// Cap arm.
	batchCapErrIsTooManyRows bool
	batchCapRows             int
	addAfterCapErrIsTooMany  bool
	drainCapErrIsTooManyRows bool
	drainCapRows             int
	drainCapDrained          int
	// drainCapDiscarded is how many sends the producer still had in flight when
	// the capped Drain walked away, counted while releasing it.
	drainCapDiscarded int

	// Real-filesystem arm.
	realFSBytes        int
	realFSPublished    bool
	badPathErrWrapped  bool
	badPathCSRReturned bool
	badPathRows        int

	// Publish-fault arm: one outcome per named fault.
	faultOutcomes map[string]bulkOracleOutcome
	faultErrors   map[string]bool
	// strandedTemps counts the faults that left a <path>.tmp behind. Tolerated
	// garbage, never a violation (see the file header).
	strandedTemps int
	// parentFsyncFaultPresentAndComplete records the documented asymmetry: a
	// failed parent-directory fsync returns an error over a path that IS present
	// and complete.
	parentFsyncFaultPresentAndComplete bool

	// Corruption arm.
	corruptionRejected bool
	corruptionErr      string
	forgedCRCDetected  bool

	// Media-fault arm.
	mediaAttempts      int
	mediaOutcomes      map[bulkOracleOutcome]int
	mediaErrors        int
	mediaBytesDiffered int
	// mediaFailedWithPriorImage counts the failed attempts adjudicated against a
	// target that already held something, which is what makes the
	// unchanged-on-failure clause live rather than dormant.
	mediaFailedWithPriorImage int
	mediaFailedReplacements   int

	// Crash-window arm (the parent-fsync differential).
	crashControlOutcome      bulkOracleOutcome
	crashControlGeneration   int
	crashTreatOutcome        bulkOracleOutcome
	crashTreatGeneration     int
	crashWritebackOutcome    bulkOracleOutcome
	crashWritebackGeneration int
	crashPendingRenames      int
	crashRolledBack          int
	crashDiscardedBytes      int64
	crashDurableSizeBefore   int64
	renameRollbacksFired     int64
	renameWritebacksFired    int64
}

// bulkOracleOptions parameterises one run. The zero value is NOT the scenario's
// configuration; use [defaultBulkOracleOptions].
//
// Every field is a FALSIFIABILITY SEAM: it makes one clause's precondition
// false, so the clause must go red. A guard that cannot fail proves nothing, and
// these are how each guard below is shown to be able to fail
// ([TestBulkLoadOracle_Sensitivity] drives them all).
type bulkOracleOptions struct {
	// perturb mutates the MODEL of the first adjudicated configuration, after the
	// closed-form cross-check and before adjudication. Perturbing the model
	// rather than the engine's output leaves the durable image exactly as a
	// passing run publishes it, so what is measured is the checker's power to see
	// a difference.
	perturb func(*bulkOracleModel)
	// tameFixture drops every guaranteed structural feature, so the coverage
	// gates on dedup / mirroring / self-loops must fire.
	tameFixture bool
	// extraParallelEdge feeds the Parallel=true loader ONE more edge than the
	// sequential one, so the byte-identity clause has a real difference to find.
	extraParallelEdge bool
	// liftRowCap raises MaxRows above the fixture length, so no cap is crossed
	// and the ErrTooManyRows clauses must fire.
	liftRowCap bool
	// skipDrainCancel lets the Drain stream close normally, so the
	// cancellation clauses must fire.
	skipDrainCancel bool
	// disarmSyncFault leaves the first-publish Sync fault unarmed, so that
	// publish succeeds and the "absent after a failed first publish" clause must
	// fire.
	disarmSyncFault bool
	// tornRepublish writes a TRUNCATED image over the published path after the
	// failed republish, so the "unchanged after a failed republish" clause must
	// fire.
	tornRepublish bool
	// disarmRenameFault leaves the rename fault unarmed.
	disarmRenameFault bool
	// liftDiskCapacity removes the ENOSPC bound.
	liftDiskCapacity bool
	// disarmParentDirFault leaves the parent-directory fsync fault unarmed, so
	// the publish returns nil and the "errored yet present and complete" clause
	// must fire.
	disarmParentDirFault bool
	// skipCorruption leaves the published image intact, so the fail-stop clause
	// must fire.
	skipCorruption bool
	// repairForgedCRC recomputes the tail CRC after corrupting the payload, so
	// the reader ACCEPTS a wrong graph. It is the constructed control for the
	// forbidden fourth outcome: the silent-divergence clause must fire.
	repairForgedCRC bool
	// writableBadPath points the "unwritable OutputPath" case at a writable
	// path, so Finalise succeeds and the publish-error clause must fire.
	writableBadPath bool
	// keepCrashDurability leaves the treatment arm's parent-directory fsync
	// intact, so the rename is pruned, the crash keeps the new generation, and
	// the differential's "the lost fsync can cost the publication" clause must
	// fire.
	keepCrashDurability bool
}

// defaultBulkOracleOptions is the scenario's own configuration: no seam engaged.
func defaultBulkOracleOptions() bulkOracleOptions { return bulkOracleOptions{} }

// -----------------------------------------------------------------------------
// Registration
// -----------------------------------------------------------------------------

// bulkLoadOracleScenario registers the bulk-loader full-contract scenario.
//
// It is bit-reproducible: the fixture is drawn from the seed, every build is
// single-goroutine, every fault is ARMED rather than sampled (so no clause is
// conditioned on a draw), and nothing in the run consults a clock or lets a map
// iteration order reach a result. The one goroutine it starts — the Drain
// producer — draws nothing and is joined before adjudication.
func bulkLoadOracleScenario() Scenario {
	return Scenario{
		Name: ScenarioBulkLoadOracle,
		Description: "offline bulk loader's full contract: Drain/ctx-cancel, AddBatch partial ingest, MaxRows, " +
			"all four adjacency configs and parallel/sequential byte-identity, adjudicated against an " +
			"independent model, published onto SimDisk under armed faults and a host crash " +
			"(goroutine fan-out is unreachable externally — see the file header)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xB0142488,
		run:         runBulkLoadOracle,
	}
}

// runBulkLoadOracle performs one scenario run in its own configuration.
func runBulkLoadOracle(ctx context.Context, seed uint64) (*SimReport, error) {
	_, report, err := runBulkLoadOracleWith(ctx, seed, defaultBulkOracleOptions())
	return report, err
}

// bulkOracleReport builds the scenario's failure report. It carries no
// filesystem path, so two runs of the same seed produce the same report text.
func bulkOracleReport(seed uint64, v []Violation) *SimReport {
	return &SimReport{
		Seed:       seed,
		Scenario:   ScenarioBulkLoadOracle,
		Mode:       ModeDeterministic,
		FailedOp:   Op{Kind: OpCreate, Cypher: "<bulk load>"},
		Violations: v,
	}
}

// runBulkLoadOracleWith performs one run under opts and returns what it measured
// alongside the report (nil == passed).
//
// The arms, in order. Each adjudicates the loaded content against the model
// before the next begins, so a defect is attributed to the arm that produced it
// rather than to the end of the run:
//
//  1. CONFIGURATION MATRIX. All four Directed x Multigraph configurations, each
//     built twice (Parallel false/true), each adjudicated in memory and again
//     after a round trip through a csrfile on a clean [SimDisk], plus the
//     parallel/sequential byte-identity comparison.
//  2. STREAMING. Drain to completion, Drain cancelled mid-stream, and AddBatch.
//  3. CAPS. MaxRows crossed by AddBatch, by a following Add, and by Drain, with
//     the partial ingest adjudicated against the model of the accepted prefix.
//  4. REAL FILESYSTEM. Finalise's own publication to a real directory, reopened
//     through [csrfile.Open], plus its refusal of an unwritable OutputPath.
//  5. PUBLISH FAULTS. An armed Sync fault on a first publish and on a
//     republish, an armed rename fault, an ENOSPC bound, and an armed
//     parent-directory fsync fault.
//  6. CORRUPTION. A published image damaged in place must be REFUSED, and a
//     damaged image whose CRC was repaired must be DETECTED as divergent.
//  7. MEDIA FAULTS. A [SimDisk] with a non-zero fault rate, asserting only the
//     three-way legal set so no clause depends on the draw.
//  8. CRASH WINDOW. The parent-directory-fsync differential across
//     [SimDisk.CrashHost], adjudicated on the DURABLE image.
func runBulkLoadOracleWith(
	ctx context.Context, seed uint64, opts bulkOracleOptions,
) (*bulkOracleEvidence, *SimReport, error) {
	s := NewSeed(seed)
	ev := &bulkOracleEvidence{
		faultOutcomes: make(map[string]bulkOracleOutcome),
		faultErrors:   make(map[string]bool),
		mediaOutcomes: make(map[bulkOracleOutcome]int),
	}
	ev.fixture = buildBulkOracleFixture(s, opts.tameFixture)

	var v []Violation
	arms := []func(context.Context, *bulkOracleEvidence, bulkOracleOptions) ([]Violation, error){
		bulkOracleArmConfigMatrix,
		bulkOracleArmStreaming,
		bulkOracleArmCaps,
		bulkOracleArmRealFS,
		bulkOracleArmPublishFaults,
		bulkOracleArmCorruption,
		bulkOracleArmMediaFaults,
		bulkOracleArmCrashWindow,
	}
	for _, arm := range arms {
		av, err := arm(ctx, ev, opts)
		if err != nil {
			return ev, nil, err
		}
		v = append(v, av...)
	}
	v = append(v, bulkOracleCheckCoverage(ev)...)

	if len(v) > 0 {
		return ev, bulkOracleReport(seed, v), nil
	}
	return ev, nil, nil
}

// -----------------------------------------------------------------------------
// Arm 1 — the configuration matrix and parallel/sequential identity
// -----------------------------------------------------------------------------

// bulkOracleConfig names one adjacency configuration.
type bulkOracleConfig struct {
	name       string
	directed   bool
	multigraph bool
}

// bulkOracleConfigs enumerates the four Directed x Multigraph configurations.
// The `bulk-vs-online` scenario only ever built the first.
func bulkOracleConfigs() []bulkOracleConfig {
	return []bulkOracleConfig{
		{name: "directed-multi", directed: true, multigraph: true},
		{name: "directed-simple", directed: true, multigraph: false},
		{name: "undirected-multi", directed: false, multigraph: true},
		{name: "undirected-simple", directed: false, multigraph: false},
	}
}

// bulkOracleArmConfigMatrix builds every configuration twice — sequentially and
// with Parallel set — adjudicates each build against the model in memory and
// again through a published csrfile, and requires the two builds to be identical
// both as bytes on the disk and as CSR columns in memory.
func bulkOracleArmConfigMatrix(
	_ context.Context, ev *bulkOracleEvidence, opts bulkOracleOptions,
) ([]Violation, error) {
	var v []Violation
	perturbed := false

	for _, cfg := range bulkOracleConfigs() {
		model := buildBulkOracleModel(ev.fixture.edges, cfg.directed, cfg.multigraph)
		cev := bulkOracleConfigEvidence{
			name:           cfg.name,
			entries:        model.entries,
			dupsDropped:    model.dupsDropped,
			mirrored:       model.mirrored,
			selfLoops:      model.selfLoops,
			modelSize:      uint64(model.entries),
			closedFormSize: bulkOracleClosedFormSize(ev.fixture.edges, cfg.directed, cfg.multigraph),
		}
		// Cross-check the model against a SECOND, closed-form derivation before
		// anything is compared to it: a model whose two views of its own edge set
		// disagree must fail here rather than pass on whichever view is wrong.
		if cev.modelSize != cev.closedFormSize {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:model>",
				Message: fmt.Sprintf("[%s] the model's stored-entry count (%d) disagrees with the"+
					" closed form derived from the edge stream (%d) — the oracle is inconsistent with itself",
					cfg.name, cev.modelSize, cev.closedFormSize),
			})
		}

		// The perturbation seam applies to the FIRST configuration only, so a
		// sensitivity case names one dimension rather than every arm at once.
		if opts.perturb != nil && !perturbed {
			opts.perturb(model)
			perturbed = true
		}
		want := model.expect()

		var images [2][]byte
		var built [2]*csr.CSR[int64]
		for i, parallel := range [2]bool{false, true} {
			edges := ev.fixture.edges
			if parallel && opts.extraParallelEdge {
				edges = append(slices.Clone(edges), bulk.Edge{
					Src: bulkOracleKey(0), Dst: bulkOracleKey(1), Weight: 77,
				})
			}
			rows, c, err := bulkOracleLoad(edges, cfg.directed, cfg.multigraph, parallel)
			if err != nil {
				return v, fmt.Errorf("sim: bulk-load-oracle %s parallel=%t: %w", cfg.name, parallel, err)
			}
			cev.rowsIngested = rows
			built[i] = c
			tag := cfg.name + "/mem/" + bulkOracleParallelTag(parallel)
			v = append(v, bulkOracleCheckCSR(tag, want, c)...)
			if rows != len(edges) {
				v = append(v, Violation{
					Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:rows>",
					Message: fmt.Sprintf("[%s] Finalise reported %d rows for %d Add calls", tag, rows, len(edges)),
				})
			}

			// Publish onto a clean SimDisk and adjudicate the round trip.
			disk := NewSimDisk(NewSeed(seed64For(cfg.name, parallel)), 0)
			if err := bulkOraclePublish(disk, bulkOraclePath, c); err != nil {
				return v, fmt.Errorf("sim: bulk-load-oracle clean publish %s: %w", tag, err)
			}
			img, rv := bulkOracleReadLive(disk, bulkOraclePath)
			v = append(v, rv...)
			if img == nil {
				v = append(v, Violation{
					Kind: ViolationACIDDurability, Op: "<bulk-load-oracle:publish>",
					Message: fmt.Sprintf("[%s] a publish that returned nil left no file at the path", tag),
				})
				continue
			}
			images[i] = img
			ver := bulkOracleAdjudicateImage(cfg.name+"/disk/"+bulkOracleParallelTag(parallel), want, img)
			v = append(v, ver.v...)
			if ver.outcome != bulkOracleComplete {
				v = append(v, Violation{
					Kind: ViolationACIDDurability, Op: "<bulk-load-oracle:publish>",
					Message: fmt.Sprintf("[%s] a clean publish onto a fault-free disk read back as %s,"+
						" not complete", tag, ver.outcome),
				})
			}
			// A clean publish must also be crash-durable: WriteToFile's contract
			// is that on a nil return the file survives power loss, which means
			// the durable image already equals the live one.
			durable, dv := bulkOracleReadDurable(disk, bulkOraclePath)
			v = append(v, dv...)
			if !bytes.Equal(durable, img) {
				v = append(v, Violation{
					Kind: ViolationACIDDurability, Op: "<bulk-load-oracle:publish>",
					Message: fmt.Sprintf("[%s] a publish that returned nil is not crash-durable:"+
						" durable image is %d bytes, live image is %d", tag, len(durable), len(img)),
				})
			}
			if parallel {
				cev.parBytes = len(img)
			} else {
				cev.seqBytes = len(img)
			}
		}

		cev.byteIdentical = images[0] != nil && bytes.Equal(images[0], images[1])
		cev.sliceIdentical = bulkOracleCSREqual(built[0], built[1])
		if !cev.byteIdentical {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:parallel-identity>",
				Message: fmt.Sprintf("[%s] the Parallel build's published csrfile is NOT byte-identical to the"+
					" sequential one (%d vs %d bytes) — the documented byte-for-byte contract is broken",
					cfg.name, cev.seqBytes, cev.parBytes),
			})
		}
		if !cev.sliceIdentical {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:parallel-identity>",
				Message: fmt.Sprintf("[%s] the Parallel build's in-memory CSR differs from the sequential one",
					cfg.name),
			})
		}
		ev.configs = append(ev.configs, cev)
	}
	return v, nil
}

// bulkOracleParallelTag renders the Parallel option for a violation tag.
func bulkOracleParallelTag(parallel bool) string {
	if parallel {
		return "parallel"
	}
	return "sequential"
}

// seed64For derives a stable per-arm disk seed from an arm name, so each arm's
// disk has its own reproducible fault stream without any of them consuming the
// run's master draw sequence.
func seed64For(name string, flag bool) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(name); i++ {
		h ^= uint64(name[i])
		h *= 1099511628211
	}
	if flag {
		h ^= 0x9E3779B97F4A7C15
	}
	return h
}

// bulkOracleCSREqual reports whether two CSRs agree on every column and scalar.
func bulkOracleCSREqual(a, b *csr.CSR[int64]) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Order() == b.Order() && a.Size() == b.Size() &&
		slices.Equal(a.VerticesSlice(), b.VerticesSlice()) &&
		slices.Equal(a.EdgesSlice(), b.EdgesSlice()) &&
		slices.Equal(a.WeightsSlice(), b.WeightsSlice())
}

// -----------------------------------------------------------------------------
// Arm 2 — streaming: Drain, cancelled Drain, AddBatch
// -----------------------------------------------------------------------------

// bulkOracleArmStreaming drives the two ingest entry points the DST had never
// called. It asserts the clean cases against the model, and for the cancelled
// Drain it asserts the ERROR IDENTITY, the accounting, and the content of the
// partial load.
//
// The cancellation point is exact rather than racy by construction: the producer
// sends into an UNBUFFERED channel and then cancels without sending again, so
// Drain has necessarily completed the Add for every send that returned and can
// only take the ctx.Done branch on its next select.
func bulkOracleArmStreaming(
	ctx context.Context, ev *bulkOracleEvidence, opts bulkOracleOptions,
) ([]Violation, error) {
	var v []Violation
	edges := ev.fixture.edges

	// --- Drain to completion. ---
	l := bulk.New(bulk.Options{Directed: true, Multigraph: true, ExpectNodes: bulkOracleKeys})
	ch := make(chan bulk.Edge)
	go func() {
		defer close(ch)
		for k := range edges {
			ch <- edges[k]
		}
	}()
	drained, err := l.Drain(ctx, ch)
	if err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle clean drain: %w", err)
	}
	ev.drainClean = drained
	if drained != len(edges) || l.Rows() != drained {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:drain>",
			Message: fmt.Sprintf("a Drain of a closed channel drained %d of %d edges and reports Rows()=%d",
				drained, len(edges), l.Rows()),
		})
	}
	_, c, err := l.Finalise()
	if err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle clean drain finalise: %w", err)
	}
	want := buildBulkOracleModel(edges, true, true).expect()
	v = append(v, bulkOracleCheckCSR("drain/clean", want, c)...)

	// --- Drain cancelled mid-stream. ---
	cancelAt := bulkOracleCancelAt
	if opts.skipDrainCancel {
		cancelAt = len(edges) // never cancels: the stream simply closes.
	}
	cl := bulk.New(bulk.Options{Directed: true, Multigraph: true, ExpectNodes: bulkOracleKeys})
	cctx, cancel := context.WithCancel(ctx)
	cch := make(chan bulk.Edge)
	go func() {
		for k := 0; k < cancelAt; k++ {
			cch <- edges[k]
		}
		if opts.skipDrainCancel {
			close(cch)
			return
		}
		// Every send above has been received AND ingested, and no further send
		// will ever arrive, so the next select can only observe the cancellation.
		cancel()
	}()
	cdrained, cerr := cl.Drain(cctx, cch)
	cancel()
	ev.drainCancelled = cdrained
	ev.drainCancelRows = cl.Rows()
	ev.drainCancelErrIsCtx = cerr != nil && errors.Is(cerr, context.Canceled)

	if !ev.drainCancelErrIsCtx {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:drain-cancel>",
			Message: fmt.Sprintf("a Drain cancelled mid-stream returned err=%v, want the context's own"+
				" cancellation error", cerr),
		})
	}
	if cdrained <= 0 || cdrained >= len(edges) {
		v = append(v, Violation{
			Kind: ViolationVacuousRun, Op: "<bulk-load-oracle:drain-cancel>",
			Message: fmt.Sprintf("a Drain cancelled mid-stream drained %d of %d edges — the cancellation was"+
				" not observed part-way through, so the partial-ingest contract was never exercised",
				cdrained, len(edges)),
		})
	}
	if cl.Rows() != cdrained {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:drain-cancel>",
			Message: fmt.Sprintf("a cancelled Drain returned %d drained but the loader reports Rows()=%d",
				cdrained, cl.Rows()),
		})
	}
	if _, cc, ferr := cl.Finalise(); ferr != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle cancelled drain finalise: %w", ferr)
	} else if cdrained > 0 && cdrained <= len(edges) {
		// The partial load must be exactly the model of the ACCEPTED PREFIX: a
		// cancellation may not lose, duplicate or reorder what was already
		// ingested.
		pw := buildBulkOracleModel(edges[:cdrained], true, true).expect()
		v = append(v, bulkOracleCheckCSR("drain/cancelled-prefix", pw, cc)...)
	}

	// --- AddBatch, uncapped. ---
	bl := bulk.New(bulk.Options{Directed: true, Multigraph: true, ExpectNodes: bulkOracleKeys})
	for lo := 0; lo < len(edges); lo += bulkOracleBatchSize {
		hi := min(lo+bulkOracleBatchSize, len(edges))
		if err := bl.AddBatch(edges[lo:hi]); err != nil {
			return v, fmt.Errorf("sim: bulk-load-oracle addbatch: %w", err)
		}
	}
	ev.batchRows = bl.Rows()
	if bl.Rows() != len(edges) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:addbatch>",
			Message: fmt.Sprintf("AddBatch ingested %d of %d edges", bl.Rows(), len(edges)),
		})
	}
	_, bc, err := bl.Finalise()
	if err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle addbatch finalise: %w", err)
	}
	v = append(v, bulkOracleCheckCSR("addbatch/clean", want, bc)...)
	return v, nil
}

// -----------------------------------------------------------------------------
// Arm 3 — the MaxRows cap
// -----------------------------------------------------------------------------

// bulkOracleArmCaps drives the row cap through all three ingest entry points.
//
// The contract [bulk.Loader.AddBatch] documents is a PARTIAL one: it returns on
// the first edge that would cross the cap and "edges accepted before that point
// remain ingested". This arm asserts that literally — the error identity, the
// exact surviving row count, and the CONTENT of the accepted prefix against the
// model — rather than merely that an error appeared.
func bulkOracleArmCaps(
	ctx context.Context, ev *bulkOracleEvidence, opts bulkOracleOptions,
) ([]Violation, error) {
	var v []Violation
	edges := ev.fixture.edges
	cap0 := bulkOracleRowCap
	if opts.liftRowCap {
		cap0 = len(edges) + 1 // never crossed.
	}
	optsFor := func() bulk.Options {
		return bulk.Options{Directed: true, Multigraph: true, MaxRows: cap0, ExpectNodes: bulkOracleKeys}
	}

	// --- AddBatch crosses the cap. ---
	l := bulk.New(optsFor())
	err := l.AddBatch(edges)
	ev.batchCapErrIsTooManyRows = errors.Is(err, bulk.ErrTooManyRows)
	ev.batchCapRows = l.Rows()
	if !ev.batchCapErrIsTooManyRows {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:cap>",
			Message: fmt.Sprintf("AddBatch of %d edges under MaxRows=%d returned err=%v, want ErrTooManyRows",
				len(edges), cap0, err),
		})
	}
	if ev.batchCapErrIsTooManyRows && l.Rows() != cap0 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:cap>",
			Message: fmt.Sprintf("AddBatch stopped at the cap but kept %d rows, want exactly MaxRows=%d"+
				" (the documented partial-ingest contract)", l.Rows(), cap0),
		})
	}

	// --- A further Add still refuses, and changes nothing. ---
	rowsBefore := l.Rows()
	addErr := l.Add(edges[0])
	ev.addAfterCapErrIsTooMany = errors.Is(addErr, bulk.ErrTooManyRows)
	if !ev.addAfterCapErrIsTooMany {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:cap>",
			Message: fmt.Sprintf("an Add after the cap was reached returned err=%v, want ErrTooManyRows", addErr),
		})
	}
	if l.Rows() != rowsBefore {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:cap>",
			Message: fmt.Sprintf("a refused Add changed the row count from %d to %d", rowsBefore, l.Rows()),
		})
	}

	// --- The accepted prefix's CONTENT, not just its count. ---
	_, c, err := l.Finalise()
	if err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle capped finalise: %w", err)
	}
	kept := min(l.Rows(), len(edges))
	pw := buildBulkOracleModel(edges[:kept], true, true).expect()
	v = append(v, bulkOracleCheckCSR("cap/accepted-prefix", pw, c)...)

	// --- Drain crosses the cap. ---
	dl := bulk.New(optsFor())
	dch := make(chan bulk.Edge)
	go func() {
		defer close(dch)
		for k := range edges {
			select {
			case dch <- edges[k]:
			case <-ctx.Done():
				return
			}
		}
	}()
	drained, derr := dl.Drain(ctx, dch)
	ev.drainCapErrIsTooManyRows = errors.Is(derr, bulk.ErrTooManyRows)
	ev.drainCapRows = dl.Rows()
	ev.drainCapDrained = drained
	if !opts.liftRowCap {
		if !ev.drainCapErrIsTooManyRows {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:cap>",
				Message: fmt.Sprintf("a Drain past MaxRows=%d returned err=%v, want ErrTooManyRows", cap0, derr),
			})
		}
		if dl.Rows() != cap0 || drained != cap0 {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:cap>",
				Message: fmt.Sprintf("a Drain stopped by the cap reports drained=%d Rows()=%d, want both = %d",
					drained, dl.Rows(), cap0),
			})
		}
	}
	// Release the producer: Drain returned early, so nothing is receiving and the
	// goroutine is parked on a send. Counting what is discarded here turns the
	// release into an accounting check rather than a bare drain.
	//
	// Drain RECEIVES the edge that trips the cap and then drops it — Add is called
	// after the receive — so the producer's sends split as
	// drained + 1 refused + discarded. Asserting the identity pins that detail: a
	// Drain that ever peeked without consuming, or consumed two, breaks it here.
	discarded := 0
	for range dch {
		discarded++
	}
	ev.drainCapDiscarded = discarded
	if !opts.liftRowCap && drained+1+discarded != len(edges) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:cap>",
			Message: fmt.Sprintf("Drain's edge accounting does not close: drained=%d + 1 refused +"+
				" discarded=%d != %d sent", drained, discarded, len(edges)),
		})
	}
	return v, nil
}

// -----------------------------------------------------------------------------
// Arm 4 — the loader's own publication, on a real filesystem
// -----------------------------------------------------------------------------

// bulkOracleArmRealFS exercises the ONE publication path the SimDisk seam cannot
// reach: [bulk.Loader.Finalise] writing through [csrfile.WriteToFile] to a real
// directory, reopened with the real mmap-backed [csrfile.Open].
//
// It also pins Finalise's failure contract: given an unwritable OutputPath it
// must wrap the writer's error AND still hand back the row count and the built
// CSR, so a caller can salvage the in-memory result.
func bulkOracleArmRealFS(
	_ context.Context, ev *bulkOracleEvidence, opts bulkOracleOptions,
) ([]Violation, error) {
	var v []Violation
	root, err := os.MkdirTemp("", "sim-bulkoracle-*")
	if err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle tempdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	edges := ev.fixture.edges
	want := buildBulkOracleModel(edges, true, true).expect()

	out := filepath.Join(root, "oracle.csr")
	l := bulk.New(bulk.Options{
		OutputPath: out, Directed: true, Multigraph: true, ExpectNodes: bulkOracleKeys,
	})
	for k := range edges {
		if err := l.Add(edges[k]); err != nil {
			return v, fmt.Errorf("sim: bulk-load-oracle realfs add: %w", err)
		}
	}
	rows, c, err := l.Finalise()
	if err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle realfs finalise: %w", err)
	}
	if rows != len(edges) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:realfs>",
			Message: fmt.Sprintf("Finalise with an OutputPath reported %d rows for %d edges", rows, len(edges)),
		})
	}
	v = append(v, bulkOracleCheckCSR("realfs/mem", want, c)...)

	if st, serr := os.Stat(out); serr != nil {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<bulk-load-oracle:realfs>",
			Message: "Finalise returned nil but published no file: " + serr.Error(),
		})
	} else {
		ev.realFSBytes = int(st.Size())
		ev.realFSPublished = true
	}
	if ev.realFSPublished {
		r, oerr := csrfile.Open(out)
		if oerr != nil {
			v = append(v, Violation{
				Kind: ViolationACIDConsistency, Op: "<bulk-load-oracle:realfs>",
				Message: "the real mmap-backed reader refused Finalise's own published file: " + oerr.Error(),
			})
		} else {
			v = append(v, bulkOracleCheckReader("realfs/reopen", want, r)...)
			_ = r.Close()
		}
	}

	// --- Finalise over an unwritable OutputPath. ---
	bad := filepath.Join(root, "no", "such", "dir", "oracle.csr")
	if opts.writableBadPath {
		bad = filepath.Join(root, "second.csr")
	}
	bl := bulk.New(bulk.Options{
		OutputPath: bad, Directed: true, Multigraph: true, ExpectNodes: bulkOracleKeys,
	})
	for k := range edges {
		if err := bl.Add(edges[k]); err != nil {
			return v, fmt.Errorf("sim: bulk-load-oracle badpath add: %w", err)
		}
	}
	brows, bc, berr := bl.Finalise()
	ev.badPathRows = brows
	ev.badPathErrWrapped = berr != nil && errors.Is(berr, fs.ErrNotExist)
	ev.badPathCSRReturned = bc != nil
	if !ev.badPathErrWrapped {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:realfs>",
			Message: fmt.Sprintf("Finalise over an unwritable OutputPath returned err=%v, want the writer's"+
				" own not-exist error wrapped through", berr),
		})
	}
	if !ev.badPathCSRReturned || brows != len(edges) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:realfs>",
			Message: fmt.Sprintf("a failed publication discarded the built result: csr!=nil is %t, rows=%d"+
				" (want %d) — the documented contract returns both alongside the error",
				ev.badPathCSRReturned, brows, len(edges)),
		})
	} else {
		v = append(v, bulkOracleCheckCSR("realfs/salvaged", want, bc)...)
	}
	return v, nil
}

// -----------------------------------------------------------------------------
// Arm 5 — armed publish faults
// -----------------------------------------------------------------------------

// bulkOracleArmPublishFaults attacks the publish protocol at each step a fault
// can be ARMED at — deterministically, so no clause depends on a draw.
//
// Each case asserts the three-way legal set on the published path, and the
// specific state its fault implies. The parent-directory-fsync case is the one
// with a documented ASYMMETRY: the rename has already happened when it fires, so
// the publish returns an error over a path that is present and complete. What
// that error means is that the name's DURABILITY is unproven, which is what
// [bulkOracleArmCrashWindow] then measures.
func bulkOracleArmPublishFaults(
	_ context.Context, ev *bulkOracleEvidence, opts bulkOracleOptions,
) ([]Violation, error) {
	var v []Violation
	edges := ev.fixture.edges
	model := buildBulkOracleModel(edges, true, true)
	want := model.expect()
	_, first, err := bulkOracleLoad(edges, true, true, false)
	if err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle fault fixture: %w", err)
	}
	// A SECOND, different generation, so a republish that must not take effect
	// can be told apart from one that did.
	secondEdges := append(slices.Clone(edges), bulk.Edge{
		Src: bulkOracleKey(3), Dst: bulkOracleKey(4), Weight: 424242,
	})
	_, second, err := bulkOracleLoad(secondEdges, true, true, false)
	if err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle fault fixture 2: %w", err)
	}

	// --- Case A: a Sync fault on a FIRST publish. Nothing was ever published,
	// so the path must not exist.
	{
		disk := NewSimDisk(NewSeed(seed64For("sync-first", false)), 0)
		if !opts.disarmSyncFault {
			disk.ArmSyncFaultAt(1)
		}
		perr := bulkOraclePublish(disk, bulkOraclePath, first)
		ev.faultErrors["sync-first"] = perr != nil
		img, rv := bulkOracleReadLive(disk, bulkOraclePath)
		v = append(v, rv...)
		ver := bulkOracleAdjudicateImage("fault/sync-first", want, img)
		v = append(v, ver.v...)
		ev.faultOutcomes["sync-first"] = ver.outcome
		if perr == nil {
			v = append(v, Violation{
				Kind: ViolationVacuousRun, Op: "<bulk-load-oracle:fault>",
				Message: "[fault/sync-first] the armed fsync fault did not make the publish fail, so every" +
					" assertion about a failed first publish below is vacuous",
			})
		} else if ver.outcome != bulkOracleAbsent {
			v = append(v, Violation{
				Kind: ViolationACIDAtomicity, Op: "<bulk-load-oracle:fault>",
				Message: fmt.Sprintf("[fault/sync-first] a first publish that failed at the fsync left the"+
					" path %s; a publication that never completed must leave nothing", ver.outcome),
			})
		}
		ev.strandedTemps += bulkOracleCountStrandedTemp(disk)
	}

	// --- Case B: a Sync fault on a REPUBLISH over a good generation. The prior
	// publication must survive intact.
	{
		disk := NewSimDisk(NewSeed(seed64For("sync-republish", false)), 0)
		if err := bulkOraclePublish(disk, bulkOraclePath, first); err != nil {
			return v, fmt.Errorf("sim: bulk-load-oracle republish fixture: %w", err)
		}
		before, rv := bulkOracleReadLive(disk, bulkOraclePath)
		v = append(v, rv...)
		disk.ArmSyncFaultAt(1)
		perr := bulkOraclePublish(disk, bulkOraclePath, second)
		ev.faultErrors["sync-republish"] = perr != nil
		if opts.tornRepublish && len(before) > 64 {
			// Constructed control: plant a TRUNCATED image at the path, which is
			// exactly the torn state the atomicity clause claims cannot occur.
			if terr := disk.TruncatePath(bulkOraclePath, int64(len(before)/2)); terr != nil {
				return v, fmt.Errorf("sim: bulk-load-oracle torn control: %w", terr)
			}
		}
		after, rv2 := bulkOracleReadLive(disk, bulkOraclePath)
		v = append(v, rv2...)
		ver := bulkOracleAdjudicateImage("fault/sync-republish", want, after)
		v = append(v, ver.v...)
		ev.faultOutcomes["sync-republish"] = ver.outcome
		if perr == nil {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:fault>",
				Message: "[fault/sync-republish] the armed fsync fault did not make the republish fail",
			})
		}
		if !bytes.Equal(before, after) {
			v = append(v, Violation{
				Kind: ViolationACIDAtomicity, Op: "<bulk-load-oracle:fault>",
				Message: fmt.Sprintf("[fault/sync-republish] a failed republish changed the published image"+
					" (%d bytes before, %d after): a torn or partial write reached the live path",
					len(before), len(after)),
			})
		}
		if ver.outcome != bulkOracleComplete {
			v = append(v, Violation{
				Kind: ViolationACIDDurability, Op: "<bulk-load-oracle:fault>",
				Message: fmt.Sprintf("[fault/sync-republish] after a failed republish the path reads as %s;"+
					" the previously published generation must still be complete", ver.outcome),
			})
		}
		ev.strandedTemps += bulkOracleCountStrandedTemp(disk)
	}

	// --- Case C: the publish rename itself fails. ---
	{
		disk := NewSimDisk(NewSeed(seed64For("rename", false)), 0)
		if err := bulkOraclePublish(disk, bulkOraclePath, first); err != nil {
			return v, fmt.Errorf("sim: bulk-load-oracle rename fixture: %w", err)
		}
		before, rv := bulkOracleReadLive(disk, bulkOraclePath)
		v = append(v, rv...)
		if !opts.disarmRenameFault {
			disk.ArmRenameFaultForPath(bulkOraclePath)
		}
		perr := bulkOraclePublish(disk, bulkOraclePath, second)
		ev.faultErrors["rename"] = perr != nil
		after, rv2 := bulkOracleReadLive(disk, bulkOraclePath)
		v = append(v, rv2...)
		ver := bulkOracleAdjudicateImage("fault/rename", want, after)
		v = append(v, ver.v...)
		ev.faultOutcomes["rename"] = ver.outcome
		if disk.RenameFaultCount() != 1 && !opts.disarmRenameFault {
			v = append(v, Violation{
				Kind: ViolationVacuousRun, Op: "<bulk-load-oracle:fault>",
				Message: fmt.Sprintf("[fault/rename] the armed rename fault fired %d times, want exactly 1 —"+
					" the destination never matched, so this case is vacuous", disk.RenameFaultCount()),
			})
		}
		if perr == nil {
			v = append(v, Violation{
				Kind: ViolationVacuousRun, Op: "<bulk-load-oracle:fault>",
				Message: "[fault/rename] the armed rename fault did not make the publish fail",
			})
		}
		if !bytes.Equal(before, after) || ver.outcome != bulkOracleComplete {
			v = append(v, Violation{
				Kind: ViolationACIDAtomicity, Op: "<bulk-load-oracle:fault>",
				Message: fmt.Sprintf("[fault/rename] a publish whose rename failed disturbed the live path"+
					" (%d bytes before, %d after, outcome %s)", len(before), len(after), ver.outcome),
			})
		}
		ev.strandedTemps += bulkOracleCountStrandedTemp(disk)
	}

	// --- Case D: ENOSPC while pre-allocating the temp file. ---
	{
		disk := NewSimDisk(NewSeed(seed64For("enospc", false)), 0)
		if err := bulkOraclePublish(disk, bulkOraclePath, first); err != nil {
			return v, fmt.Errorf("sim: bulk-load-oracle enospc fixture: %w", err)
		}
		before, rv := bulkOracleReadLive(disk, bulkOraclePath)
		v = append(v, rv...)
		if !opts.liftDiskCapacity {
			// Cap the disk at exactly what it already holds, so the republish's
			// temp-file pre-allocation cannot fit.
			disk.SetCapacity(int64(len(before)), false)
		}
		perr := bulkOraclePublish(disk, bulkOraclePath, second)
		disk.SetCapacity(0, false)
		ev.faultErrors["enospc"] = perr != nil
		after, rv2 := bulkOracleReadLive(disk, bulkOraclePath)
		v = append(v, rv2...)
		ver := bulkOracleAdjudicateImage("fault/enospc", want, after)
		v = append(v, ver.v...)
		ev.faultOutcomes["enospc"] = ver.outcome
		if perr == nil {
			v = append(v, Violation{
				Kind: ViolationVacuousRun, Op: "<bulk-load-oracle:fault>",
				Message: "[fault/enospc] the republish under an exact-capacity bound succeeded, so the" +
					" atomicity assertion for this case is vacuous",
			})
		}
		if !bytes.Equal(before, after) || ver.outcome != bulkOracleComplete {
			v = append(v, Violation{
				Kind: ViolationACIDAtomicity, Op: "<bulk-load-oracle:fault>",
				Message: fmt.Sprintf("[fault/enospc] an out-of-space republish disturbed the live path"+
					" (%d bytes before, %d after, outcome %s)", len(before), len(after), ver.outcome),
			})
		}
		ev.strandedTemps += bulkOracleCountStrandedTemp(disk)
	}

	// --- Case E: the post-rename parent-directory fsync fails. ---
	{
		disk := NewSimDisk(NewSeed(seed64For("parent-fsync", false)), 0)
		if !opts.disarmParentDirFault {
			disk.ArmParentDirSyncFaultForPath(bulkOraclePath)
		}
		perr := bulkOraclePublish(disk, bulkOraclePath, first)
		ev.faultErrors["parent-fsync"] = perr != nil
		img, rv := bulkOracleReadLive(disk, bulkOraclePath)
		v = append(v, rv...)
		ver := bulkOracleAdjudicateImage("fault/parent-fsync", want, img)
		v = append(v, ver.v...)
		ev.faultOutcomes["parent-fsync"] = ver.outcome
		ev.parentFsyncFaultPresentAndComplete = perr != nil && ver.outcome == bulkOracleComplete
		if !ev.parentFsyncFaultPresentAndComplete {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:fault>",
				Message: fmt.Sprintf("[fault/parent-fsync] a failed parent-directory fsync must report an"+
					" error over a path that is nonetheless PRESENT and COMPLETE (the rename already"+
					" happened); got err!=nil=%t outcome=%s", perr != nil, ver.outcome),
			})
		}
		if disk.DirSyncFaultCount() != 1 && !opts.disarmParentDirFault {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:fault>",
				Message: fmt.Sprintf("[fault/parent-fsync] the armed directory-fsync fault fired %d times,"+
					" want exactly 1", disk.DirSyncFaultCount()),
			})
		}
		ev.strandedTemps += bulkOracleCountStrandedTemp(disk)
	}
	return v, nil
}

// bulkOracleCountStrandedTemp returns 1 when a <path>.tmp survived the arm.
//
// It is an OBSERVATION, never a violation: nothing in the module enumerates the
// publish directory, so a stranded temp is invisible to every reader (see the
// file header).
func bulkOracleCountStrandedTemp(disk *SimDisk) int {
	if disk.Exists(bulkOraclePath + ".tmp") {
		return 1
	}
	return 0
}

// -----------------------------------------------------------------------------
// Arm 6 — corruption fail-stop, and the forged-checksum control
// -----------------------------------------------------------------------------

// bulkOracleArmCorruption damages a correctly published image and requires the
// reader to REFUSE it, then repairs the checksum over the damaged payload and
// requires the oracle to notice that the accepted graph is the wrong one.
//
// The second half is the constructed control for the forbidden fourth outcome.
// Without it, "present and accepted implies equal to the model" would rest on
// the checksum alone; with it, the clause is shown to fire when a damaged image
// really is accepted.
func bulkOracleArmCorruption(
	_ context.Context, ev *bulkOracleEvidence, opts bulkOracleOptions,
) ([]Violation, error) {
	var v []Violation
	edges := ev.fixture.edges
	want := buildBulkOracleModel(edges, true, true).expect()
	_, c, err := bulkOracleLoad(edges, true, true, false)
	if err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle corruption fixture: %w", err)
	}

	disk := NewSimDisk(NewSeed(seed64For("corrupt", false)), 0)
	if err := bulkOraclePublish(disk, bulkOraclePath, c); err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle corruption publish: %w", err)
	}
	img, rv := bulkOracleReadLive(disk, bulkOraclePath)
	v = append(v, rv...)
	if img == nil {
		return v, errors.New("sim: bulk-load-oracle corruption arm published nothing")
	}

	// Damage a byte inside the WEIGHTS section. That choice is deliberate: the
	// weights are the one payload region `Reader.validateCSRSemantics` places no
	// constraint on, so the ONLY thing that can reject the damaged image is the
	// tail CRC — which is precisely the gate this case is about. Damaging an
	// offset or a destination instead would be caught by the semantic check and
	// would prove nothing about the checksum.
	hdr, herr := csrfile.DecodeHeader(img)
	if herr != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle decode published header: %w", herr)
	}
	off := int64(hdr.WeightsOffset) + 8
	if !opts.skipCorruption {
		if err := disk.CorruptRange(bulkOraclePath, off, 1); err != nil {
			return v, fmt.Errorf("sim: bulk-load-oracle corrupt: %w", err)
		}
	}
	damaged, rv2 := bulkOracleReadLive(disk, bulkOraclePath)
	v = append(v, rv2...)
	ver := bulkOracleAdjudicateImage("corrupt/detect", want, damaged)
	v = append(v, ver.v...)
	ev.corruptionRejected = ver.rejectErr != nil && bulkOracleRejection(ver.rejectErr)
	if ver.rejectErr != nil {
		ev.corruptionErr = ver.rejectErr.Error()
	}
	if !ev.corruptionRejected {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: "<bulk-load-oracle:corruption>",
			Message: fmt.Sprintf("a published csrfile with a flipped weight byte at offset %d was NOT"+
				" refused by the reader (err=%v): silent data corruption is a fail-stop violation",
				off, ver.rejectErr),
		})
	}

	// The forged-checksum control: repair the tail CRC over the damaged payload
	// so the image is self-consistent but describes a DIFFERENT graph.
	if opts.repairForgedCRC && !opts.skipCorruption && damaged != nil {
		forged, ferr := bulkOracleRepairCRC(damaged)
		if ferr != nil {
			return v, fmt.Errorf("sim: bulk-load-oracle forge crc: %w", ferr)
		}
		fver := bulkOracleAdjudicateImage("corrupt/forged", want, forged)
		ev.forgedCRCDetected = len(fver.v) > 0
		if fver.outcome == bulkOracleComplete && len(fver.v) == 0 {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:corruption>",
				Message: "an image whose payload was damaged and whose checksum was then repaired was" +
					" accepted as EQUAL to the model: the content oracle cannot see a silent divergence," +
					" so every 'present and complete' verdict above is unfalsifiable",
			})
		}
	}
	return v, nil
}

// bulkOracleRepairCRC returns a copy of img whose trailing CRC32C is recomputed
// over its (possibly damaged) payload, so the reader accepts it.
//
// It is a harness-only construction: it manufactures the one state the module's
// checksum exists to make unreachable, purely so the content oracle can be shown
// to detect it.
func bulkOracleRepairCRC(img []byte) ([]byte, error) {
	h, err := csrfile.DecodeHeader(img)
	if err != nil {
		return nil, err
	}
	if h.TailCRCOffset+4 > uint64(len(img)) {
		return nil, fmt.Errorf("sim: forged crc: tail offset %d past image length %d", h.TailCRCOffset, len(img))
	}
	out := bytes.Clone(img)
	sum := crc32Castagnoli(out[:h.TailCRCOffset])
	out[h.TailCRCOffset] = byte(sum)
	out[h.TailCRCOffset+1] = byte(sum >> 8)
	out[h.TailCRCOffset+2] = byte(sum >> 16)
	out[h.TailCRCOffset+3] = byte(sum >> 24)
	return out, nil
}

// -----------------------------------------------------------------------------
// Arm 7 — media faults (a non-zero FaultRate disk)
// -----------------------------------------------------------------------------

// bulkOracleArmMediaFaults publishes repeatedly onto a disk whose sectors and
// fsyncs fail with probability [bulkOracleFaultRate].
//
// This is the arm where the THREE-way disjunction earns its keep. A faulted
// sector is corrupted silently at write time, so the publish can return nil over
// an image the reader will refuse — a legal outcome that a two-way
// "absent or complete" oracle would report as a defect.
//
// No clause here is conditioned on the draw: the arm asserts only that every
// attempt landed in the legal set, which holds for every seed. Which outcomes
// actually occurred is recorded in the evidence and gated by the test at the
// pinned default seed, where it is a measurement rather than a lottery.
func bulkOracleArmMediaFaults(
	_ context.Context, ev *bulkOracleEvidence, _ bulkOracleOptions,
) ([]Violation, error) {
	var v []Violation
	edges := ev.fixture.edges
	want := buildBulkOracleModel(edges, true, true).expect()
	_, c, err := bulkOracleLoad(edges, true, true, false)
	if err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle media fixture: %w", err)
	}
	intended, err := bulkOracleIntendedImage(c)
	if err != nil {
		return v, err
	}

	disk := NewSimDisk(NewSeed(seed64For("media", false)), bulkOracleFaultRate)
	var prev []byte
	for attempt := 0; attempt < bulkOracleFaultAttempts; attempt++ {
		perr := bulkOraclePublish(disk, bulkOraclePath, c)
		ev.mediaAttempts++
		if perr != nil {
			ev.mediaErrors++
		}
		img, rv := bulkOracleReadLive(disk, bulkOraclePath)
		v = append(v, rv...)
		if img != nil && !bytes.Equal(img, intended) {
			ev.mediaBytesDiffered++
		}
		ver := bulkOracleAdjudicateImage("media", want, img)
		v = append(v, ver.v...)
		ev.mediaOutcomes[ver.outcome]++

		// A publish that FAILED must not have changed the target's bytes. This is
		// phrased over the RAW BYTES rather than over the outcome so it stays live
		// on every failed attempt, including one whose target already held a
		// corrupted (reader-rejected) image. Phrasing it over "the previously
		// COMPLETE image" instead would leave the clause dormant on any seed that
		// never produced a clean publication onto the faulty disk — which is
		// exactly what the default seed does (measured: 0 complete outcomes).
		//
		// The escape is the one failure mode that legitimately DOES replace the
		// target: a fault after the rename but before the parent-directory fsync,
		// which arm 5 covers explicitly. It cannot arise here — no directory-fsync
		// fault is armed, and the fault rate reaches only sectors and Syncs, both
		// of which precede the rename — so on this arm the strict form is what
		// holds.
		if perr != nil {
			if prev != nil {
				ev.mediaFailedWithPriorImage++
			}
			if !bytes.Equal(img, prev) && ver.outcome != bulkOracleComplete {
				ev.mediaFailedReplacements++
				v = append(v, Violation{
					Kind: ViolationACIDAtomicity, Op: "<bulk-load-oracle:media>",
					Message: fmt.Sprintf("attempt %d FAILED yet replaced the target with %d bytes that are"+
						" neither the previous image (%d bytes) nor a complete new generation: a partial"+
						" write reached the published path", attempt, len(img), len(prev)),
				})
			}
		}
		if perr == nil && ver.outcome == bulkOracleAbsent {
			v = append(v, Violation{
				Kind: ViolationACIDDurability, Op: "<bulk-load-oracle:media>",
				Message: fmt.Sprintf("attempt %d returned nil but left nothing at the path", attempt),
			})
		}
		prev = img
	}
	return v, nil
}

// bulkOracleIntendedImage renders the exact bytes a fault-free publication of c
// produces, so a media arm can tell "the image was corrupted on the way down"
// from "the image is what the writer meant".
func bulkOracleIntendedImage(c *csr.CSR[int64]) ([]byte, error) {
	clean := NewSimDisk(NewSeed(seed64For("intended", false)), 0)
	if err := bulkOraclePublish(clean, bulkOraclePath, c); err != nil {
		return nil, fmt.Errorf("sim: bulk-load-oracle intended image: %w", err)
	}
	img, err := clean.ReadFile(bulkOraclePath)
	if err != nil {
		return nil, fmt.Errorf("sim: bulk-load-oracle intended image read: %w", err)
	}
	return img, nil
}

// -----------------------------------------------------------------------------
// Arm 8 — the rename <-> parent-fsync crash window
// -----------------------------------------------------------------------------

// bulkOracleArmCrashWindow is the differential that proves the publish
// protocol's parent-directory fsync is LOAD-BEARING, with no mutation of the
// module.
//
// Before this arm, nothing in the DST crashed a csrfile publication, which is
// worth stating precisely because a bare grep disagrees.
// storage_fault_scenarios.go DOES call Crash three times — but all three are
// `st.Crash()` in ST6, whose armed fault targets the WAL's own post-rename
// directory fsync (`walPathFor(cfg.dir)`), not a csrfile path; its csrfile arm
// (ST4) never crashes. csrfile_access_matrix.go calls no crash primitive at all,
// and internal/crashinject has no csrfile coverage. The window between the
// publish rename and the parent-directory fsync was therefore untested, and this
// arm is the first crash taken against it.
//
// Three sub-arms publish generation 1, then generation 2, then crash the HOST:
//
//	control   — generation 2 published cleanly. The parent fsync succeeded, so
//	            the rename is durable and pruned from the undo log; the crash
//	            MUST leave generation 2.
//	treatment — the parent fsync is faulted and the rename pinned to its
//	            rolled-back branch. Nothing made the new name durable, so the
//	            crash MUST restore generation 1 — the publication is lost, whole,
//	            never torn.
//	writeback — the parent fsync is faulted but the rename pinned to its
//	            written-back branch, the OTHER legal outcome of the same window.
//	            The crash MUST leave generation 2.
//
// Both branches of the window are PINNED rather than sampled, so each side is
// deterministic for every seed; leaving it to the draw would make one side a
// lottery. Every verdict is read off the DURABLE image, never off the
// publisher's return value: real power loss ends the process, so what a
// still-running publisher would have returned is a harness artefact.
func bulkOracleArmCrashWindow(
	_ context.Context, ev *bulkOracleEvidence, opts bulkOracleOptions,
) ([]Violation, error) {
	var v []Violation
	edges := ev.fixture.edges
	gen1Model := buildBulkOracleModel(edges, true, true)
	want1 := gen1Model.expect()
	_, gen1, err := bulkOracleLoad(edges, true, true, false)
	if err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle crash fixture 1: %w", err)
	}
	gen2Edges := append(slices.Clone(edges), bulk.Edge{
		Src: bulkOracleKey(5), Dst: bulkOracleKey(6), Weight: -987654321,
	})
	want2 := buildBulkOracleModel(gen2Edges, true, true).expect()
	_, gen2, err := bulkOracleLoad(gen2Edges, true, true, false)
	if err != nil {
		return v, fmt.Errorf("sim: bulk-load-oracle crash fixture 2: %w", err)
	}

	// generation identifies which of the two published graphs a post-crash image
	// holds: 1, 2, or 0 for absent / unrecognisable.
	generation := func(tag string, img []byte) (int, []Violation) {
		if img == nil {
			return 0, nil
		}
		if g2 := bulkOracleAdjudicateImage(tag+"/gen2", want2, img); g2.outcome == bulkOracleComplete && len(g2.v) == 0 {
			return 2, nil
		}
		g1 := bulkOracleAdjudicateImage(tag+"/gen1", want1, img)
		if g1.outcome == bulkOracleComplete && len(g1.v) == 0 {
			return 1, nil
		}
		// Neither generation: legal only if the reader refuses the image
		// outright. An image the reader ACCEPTS that is neither generation is the
		// forbidden fourth outcome.
		if g1.rejectErr != nil && bulkOracleRejection(g1.rejectErr) {
			return 0, nil
		}
		return 0, []Violation{{
			Kind: ViolationACIDAtomicity, Op: "<bulk-load-oracle:crash>",
			Message: fmt.Sprintf("[%s] the durable image after the crash is %d bytes and is neither"+
				" published generation, yet the reader accepts it: a torn publication reached the path",
				tag, len(img)),
		}}
	}

	// --- Control: a clean second publish, then a host crash. ---
	{
		disk := NewSimDisk(NewSeed(seed64For("crash-control", false)), 0)
		if err := bulkOraclePublish(disk, bulkOraclePath, gen1); err != nil {
			return v, fmt.Errorf("sim: bulk-load-oracle crash control gen1: %w", err)
		}
		if err := bulkOraclePublish(disk, bulkOraclePath, gen2); err != nil {
			return v, fmt.Errorf("sim: bulk-load-oracle crash control gen2: %w", err)
		}
		if sz, ok := disk.DurableSize(bulkOraclePath); ok {
			ev.crashDurableSizeBefore = sz
		}
		pendingBefore := disk.PendingRenameCount()
		disk.CrashHost()
		img, rv := bulkOracleReadDurable(disk, bulkOraclePath)
		v = append(v, rv...)
		gen, gv := generation("crash/control", img)
		v = append(v, gv...)
		ev.crashControlGeneration = gen
		ev.crashControlOutcome = bulkOracleOutcomeOf(img != nil, gen != 0)
		if gen != 2 {
			v = append(v, Violation{
				Kind: ViolationACIDDurability, Op: "<bulk-load-oracle:crash>",
				Message: fmt.Sprintf("[crash/control] a publication that completed its parent-directory"+
					" fsync did not survive a host crash: the durable image holds generation %d, want 2", gen),
			})
		}
		if pendingBefore != 0 {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:crash>",
				Message: fmt.Sprintf("[crash/control] %d rename(s) were still pending after a publish whose"+
					" parent-directory fsync succeeded; the control is not the durable case it claims to be",
					pendingBefore),
			})
		}
	}

	// --- Treatment: the parent fsync fails and the rename is pinned to its
	// rolled-back branch, then a host crash. ---
	{
		disk := NewSimDisk(NewSeed(seed64For("crash-treatment", false)), 0)
		if err := bulkOraclePublish(disk, bulkOraclePath, gen1); err != nil {
			return v, fmt.Errorf("sim: bulk-load-oracle crash treatment gen1: %w", err)
		}
		if !opts.keepCrashDurability {
			disk.ArmParentDirSyncFaultForPath(bulkOraclePath)
			disk.ArmRenameRollbackForPath(bulkOraclePath)
		}
		perr := bulkOraclePublish(disk, bulkOraclePath, gen2)
		ev.crashPendingRenames = disk.PendingRenameCount()
		disk.CrashHost()
		pending, rolledBack := disk.LastCrashRenameOutcome()
		ev.crashRolledBack = rolledBack
		// Recorded, not gated, and EXPECTED to be zero: the publish fsync'd its
		// data before the rename, so what this crash destroys is a directory
		// ENTRY, not a byte. Reading a zero here as "the crash did nothing" would
		// be wrong, which is why the rename-window observables above
		// (PendingRenameCount / LastCrashRenameOutcome) are this arm's
		// non-vacuity evidence and the byte counter is not.
		ev.crashDiscardedBytes = disk.LastCrashDiscardedBytes()
		ev.renameRollbacksFired = disk.RenameRollbackCount()
		img, rv := bulkOracleReadDurable(disk, bulkOraclePath)
		v = append(v, rv...)
		gen, gv := generation("crash/treatment", img)
		v = append(v, gv...)
		ev.crashTreatGeneration = gen
		ev.crashTreatOutcome = bulkOracleOutcomeOf(img != nil, gen != 0)

		if !opts.keepCrashDurability {
			if perr == nil {
				v = append(v, Violation{
					Kind: ViolationVacuousRun, Op: "<bulk-load-oracle:crash>",
					Message: "[crash/treatment] the armed parent-directory fsync fault did not make the" +
						" publish fail, so the window this arm claims to enter was never entered",
				})
			}
			if pending == 0 || rolledBack == 0 {
				v = append(v, Violation{
					Kind: ViolationVacuousRun, Op: "<bulk-load-oracle:crash>",
					Message: fmt.Sprintf("[crash/treatment] the crash adjudicated pending=%d rolledBack=%d;"+
						" the rename window was not entered, so the verdict below is vacuous",
						pending, rolledBack),
				})
			}
			// THE DIFFERENTIAL. Without the parent fsync the publication is
			// rollback-eligible, so the crash must leave the PREVIOUS generation
			// — whole, and never a mixture of the two.
			if gen != 1 {
				v = append(v, Violation{
					Kind: ViolationACIDAtomicity, Op: "<bulk-load-oracle:crash>",
					Message: fmt.Sprintf("[crash/treatment] a rolled-back publish rename left generation %d;"+
						" a crash in the rename window must leave the previous generation intact, never a"+
						" partial or absent one", gen),
				})
			}
		}
	}

	// --- Writeback: the other legal branch of the same window. ---
	{
		disk := NewSimDisk(NewSeed(seed64For("crash-writeback", false)), 0)
		if err := bulkOraclePublish(disk, bulkOraclePath, gen1); err != nil {
			return v, fmt.Errorf("sim: bulk-load-oracle crash writeback gen1: %w", err)
		}
		disk.ArmParentDirSyncFaultForPath(bulkOraclePath)
		disk.ArmRenameWritebackForPath(bulkOraclePath)
		_ = bulkOraclePublish(disk, bulkOraclePath, gen2) // fails at the parent fsync by design
		disk.CrashHost()
		ev.renameWritebacksFired = disk.RenameWritebackCount()
		img, rv := bulkOracleReadDurable(disk, bulkOraclePath)
		v = append(v, rv...)
		gen, gv := generation("crash/writeback", img)
		v = append(v, gv...)
		ev.crashWritebackGeneration = gen
		ev.crashWritebackOutcome = bulkOracleOutcomeOf(img != nil, gen != 0)
		if disk.RenameWritebackCount() != 1 {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: "<bulk-load-oracle:crash>",
				Message: fmt.Sprintf("[crash/writeback] the armed rename write-back fired %d times, want 1",
					disk.RenameWritebackCount()),
			})
		}
		if gen != 2 {
			v = append(v, Violation{
				Kind: ViolationACIDDurability, Op: "<bulk-load-oracle:crash>",
				Message: fmt.Sprintf("[crash/writeback] a rename whose dirent reached stable storage left"+
					" generation %d after the crash, want 2 — the other legal branch of the window is"+
					" not being produced", gen),
			})
		}
	}
	return v, nil
}

// bulkOracleOutcomeOf maps (present, recognised) onto the three-way outcome, for
// the evidence record.
func bulkOracleOutcomeOf(present, recognised bool) bulkOracleOutcome {
	switch {
	case !present:
		return bulkOracleAbsent
	case recognised:
		return bulkOracleComplete
	default:
		return bulkOracleRejected
	}
}

// -----------------------------------------------------------------------------
// Coverage gates
// -----------------------------------------------------------------------------

// bulkOracleCheckCoverage asserts that the run really entered the branches its
// verdicts are about.
//
// These are STRUCTURAL, not probabilistic: the fixture guarantees a duplicate
// pair, a self-loop and a reversed pair for every seed, and every fault is armed
// rather than drawn. A gate here firing therefore means the fixture or an arm
// degenerated, not that a seed was unlucky.
func bulkOracleCheckCoverage(ev *bulkOracleEvidence) []Violation {
	var v []Violation
	op := "<bulk-load-oracle:coverage>"
	note := func(format string, args ...any) {
		v = append(v, Violation{Kind: ViolationOracleDeviation, Op: op, Message: fmt.Sprintf(format, args...)})
	}

	if len(ev.fixture.edges) < bulkOracleMinEdgeRecords {
		note("the fixture holds %d edge records, want at least %d", len(ev.fixture.edges), bulkOracleMinEdgeRecords)
	}
	if ev.fixture.distinctKeys < bulkOracleMinDistinctKeys {
		note("the fixture touches %d distinct keys, want at least %d",
			ev.fixture.distinctKeys, bulkOracleMinDistinctKeys)
	}
	if ev.fixture.duplicatePairs == 0 {
		note("the fixture repeats no endpoint pair, so simple-graph dedup was never exercised")
	}
	if ev.fixture.selfLoops == 0 {
		note("the fixture holds no self-loop, so the undirected mirror exception was never exercised")
	}
	if ev.fixture.reversedPairs == 0 {
		note("the fixture holds no reversed pair, so undirected simple-graph dedup was never exercised")
	}

	if len(ev.configs) != len(bulkOracleConfigs()) {
		note("%d of %d adjacency configurations were adjudicated", len(ev.configs), len(bulkOracleConfigs()))
	}
	for i := range ev.configs {
		c := ev.configs[i]
		if c.seqBytes < bulkOracleMinPublishBytes || c.parBytes < bulkOracleMinPublishBytes {
			note("[%s] published %d/%d bytes, want at least %d on both builds — a comparison of two"+
				" trivial images would pass without testing anything",
				c.name, c.seqBytes, c.parBytes, bulkOracleMinPublishBytes)
		}
		simple := c.name == "directed-simple" || c.name == "undirected-simple"
		undirected := c.name == "undirected-multi" || c.name == "undirected-simple"
		if simple && c.dupsDropped == 0 {
			note("[%s] dropped no duplicate entry", c.name)
		}
		if !simple && c.dupsDropped != 0 {
			note("[%s] dropped %d entries although a multigraph keeps every parallel edge",
				c.name, c.dupsDropped)
		}
		if undirected && c.mirrored == 0 {
			note("[%s] stored no mirror entry", c.name)
		}
		if !undirected && c.mirrored != 0 {
			note("[%s] stored %d mirror entries although a directed load mirrors nothing",
				c.name, c.mirrored)
		}
	}

	// Streaming and cap arms really entered their branches.
	if !ev.drainCancelErrIsCtx {
		note("no Drain cancellation was observed")
	}
	if ev.drainCancelled == 0 || ev.drainCancelled >= len(ev.fixture.edges) {
		note("the cancelled Drain ingested %d of %d edges: the cancellation was not mid-stream",
			ev.drainCancelled, len(ev.fixture.edges))
	}
	if !ev.batchCapErrIsTooManyRows || !ev.addAfterCapErrIsTooMany || !ev.drainCapErrIsTooManyRows {
		note("the row cap was not crossed on all three ingest entry points"+
			" (AddBatch=%t Add=%t Drain=%t)",
			ev.batchCapErrIsTooManyRows, ev.addAfterCapErrIsTooMany, ev.drainCapErrIsTooManyRows)
	}
	if ev.batchCapRows == 0 || ev.batchCapRows >= len(ev.fixture.edges) {
		note("AddBatch kept %d of %d rows under the cap: the partial ingest was not observed",
			ev.batchCapRows, len(ev.fixture.edges))
	}

	// Fault arms really failed.
	for _, name := range []string{"sync-first", "sync-republish", "rename", "enospc", "parent-fsync"} {
		if !ev.faultErrors[name] {
			note("the %q publish fault did not produce an error, so its verdict is vacuous", name)
		}
	}
	if !ev.corruptionRejected {
		note("no corruption rejection was observed")
	}
	if ev.mediaAttempts != bulkOracleFaultAttempts {
		note("the media-fault arm made %d of %d attempts", ev.mediaAttempts, bulkOracleFaultAttempts)
	}

	// The crash window really was a differential.
	if ev.crashControlGeneration == ev.crashTreatGeneration {
		note("the crash-window control and treatment both left generation %d: the parent-directory fsync"+
			" made no difference, so this arm proves nothing about it", ev.crashControlGeneration)
	}
	if ev.renameRollbacksFired == 0 || ev.renameWritebacksFired == 0 {
		note("the crash window's branches did not both fire (rollbacks=%d writebacks=%d)",
			ev.renameRollbacksFired, ev.renameWritebacksFired)
	}
	return v
}
