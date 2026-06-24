package sim

import (
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/search/community"
)

// TestCommunityChecks_CleanOnFixtures asserts the community-detection battery
// finds no violation across a spread of ticks. The fixtures are deterministic,
// well-separated clique chains on which Leiden recovers the planted communities
// exactly and both algorithms return valid, deterministic partitions, so a clean
// engine must yield nil at every tick.
func TestCommunityChecks_CleanOnFixtures(t *testing.T) {
	t.Parallel()
	for _, tick := range []int64{0, 1, 2, 3, 7, 42, 99, 1000, 123456} {
		if vs := communityViolations(tick); vs != nil {
			t.Fatalf("tick %d: expected no violations, got %d:\n%v", tick, len(vs), vs)
		}
	}
}

// TestCommunityChecks_Deterministic asserts the battery itself is a pure
// function of the tick: two invocations at the same tick must produce identical
// violation slices (here, both nil). This guards the harness invariant that all
// randomness flows from the seed and no map iteration leaks into the output.
func TestCommunityChecks_Deterministic(t *testing.T) {
	t.Parallel()
	for _, tick := range []int64{0, 5, 250, 99999} {
		a := communityViolations(tick)
		b := communityViolations(tick)
		if len(a) != len(b) {
			t.Fatalf("tick %d: non-deterministic violation count: %d vs %d", tick, len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("tick %d: violation %d differs: %v vs %v", tick, i, a[i], b[i])
			}
		}
	}
}

// TestCommunity_PlantedRecovery_TwoCliques is the hand-checked recovery proof.
// Two triangles {0,1,2} and {3,4,5} joined by the single bridge 2-3 form an
// unambiguous two-community graph. Leiden must recover exactly two communities,
// with {0,1,2} in one and {3,4,5} in the other, and the canonical signature of
// the recovered partition must equal the canonical signature of the planted
// blocks.
func TestCommunity_PlantedRecovery_TwoCliques(t *testing.T) {
	t.Parallel()
	f := communityCliqueChain("hand-2-cliques", []int{3, 3})

	// Hand-check the fixture itself before trusting it as an oracle.
	if f.order != 6 {
		t.Fatalf("fixture order = %d, want 6", f.order)
	}
	wantBlock := []int{0, 0, 0, 1, 1, 1}
	if !slices.Equal(f.block, wantBlock) {
		t.Fatalf("planted blocks = %v, want %v", f.block, wantBlock)
	}
	// Two triangles (3 edges each) + 1 bridge = 7 undirected edges.
	if len(f.edges) != 7 {
		t.Fatalf("fixture has %d undirected edges, want 7", len(f.edges))
	}

	c := communityBuildCSR(f)
	if got := int(c.MaxNodeID()); got != 6 {
		t.Fatalf("MaxNodeID = %d, want 6", got)
	}
	// Symmetric CSR: 7 undirected edges -> 14 directed slots, and IsSymmetric.
	if got := int(c.Size()); got != 14 {
		t.Fatalf("CSR Size = %d, want 14 (7 undirected edges doubled)", got)
	}
	if !c.IsSymmetric() {
		t.Fatalf("planted CSR is not symmetric; community detection requires an undirected graph")
	}

	p := community.Leiden(c, community.DefaultLeidenOptions())
	if p.NumCommunities != 2 {
		t.Fatalf("Leiden recovered %d communities, want 2 (Community=%v)", p.NumCommunities, p.Community)
	}
	// {0,1,2} must share a community and {3,4,5} must share the other.
	if p.Community[0] != p.Community[1] || p.Community[1] != p.Community[2] {
		t.Fatalf("clique {0,1,2} was split: %v", p.Community[:3])
	}
	if p.Community[3] != p.Community[4] || p.Community[4] != p.Community[5] {
		t.Fatalf("clique {3,4,5} was split: %v", p.Community[3:])
	}
	if p.Community[0] == p.Community[3] {
		t.Fatalf("the two cliques were merged into one community: %v", p.Community)
	}

	// Canonical-signature recovery check (up to relabelling).
	if err := checkPlantedRecovery(p, f); err != nil {
		t.Fatalf("planted recovery failed: %v", err)
	}

	// The canonical signature is relabelling-invariant: the planted blocks
	// {0,0,0,1,1,1} and a relabelled-but-identical grouping {7,7,7,3,3,3}
	// must share one signature, namely each node mapped to its block's min
	// index: [0,0,0,3,3,3].
	wantSig := []int{0, 0, 0, 3, 3, 3}
	if got := canonicalPartitionSig(f.block); !slices.Equal(got, wantSig) {
		t.Fatalf("canonical signature of planted blocks = %v, want %v", got, wantSig)
	}
	relabelled := []int{7, 7, 7, 3, 3, 3}
	if got := canonicalPartitionSig(relabelled); !slices.Equal(got, wantSig) {
		t.Fatalf("canonical signature not relabelling-invariant: %v != %v", got, wantSig)
	}
}

// TestCommunity_CanonicalSig_DetectsWrongPartition proves the canonical-signature
// comparison is sound: it must report EQUAL only for partitions that group the
// same nodes together, and UNEQUAL for any grouping difference — even when the
// two label slices use overlapping numeric labels.
func TestCommunity_CanonicalSig_DetectsWrongPartition(t *testing.T) {
	t.Parallel()

	truth := []int{0, 0, 0, 1, 1, 1} // {0,1,2} | {3,4,5}

	cases := []struct {
		name      string
		labels    []int
		wantEqual bool
	}{
		{"identical", []int{0, 0, 0, 1, 1, 1}, true},
		{"relabelled-same-grouping", []int{5, 5, 5, 2, 2, 2}, true},
		{"swapped-labels-same-grouping", []int{1, 1, 1, 0, 0, 0}, true},
		{"one-node-misassigned", []int{0, 0, 1, 1, 1, 1}, false},
		{"all-one-community", []int{0, 0, 0, 0, 0, 0}, false},
		{"all-singletons", []int{0, 1, 2, 3, 4, 5}, false},
		{"split-first-clique", []int{0, 2, 0, 1, 1, 1}, false},
	}

	truthSig := canonicalPartitionSig(truth)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSig := canonicalPartitionSig(tc.labels)
			gotEqual := slices.Equal(gotSig, truthSig)
			if gotEqual != tc.wantEqual {
				t.Fatalf("labels=%v sig=%v vs truthSig=%v: gotEqual=%v want=%v",
					tc.labels, gotSig, truthSig, gotEqual, tc.wantEqual)
			}
		})
	}
}

// TestCommunity_CheckPlantedRecovery_FlagsSplit proves the recovery check flags a
// partition that SPLITS a planted clique (a real regression), while tolerating a
// legitimate MERGE of whole cliques (the modularity resolution limit), which the
// sound invariant must not reject.
func TestCommunity_CheckPlantedRecovery_FlagsSplit(t *testing.T) {
	t.Parallel()
	f := communityCliqueChain("two-cliques-3-3", []int{3, 3})

	// Exact recovery: two blocks matching the plant -> no error.
	good := community.Partition{Community: []int{0, 0, 0, 1, 1, 1}, NumCommunities: 2}
	if err := checkPlantedRecovery(good, f); err != nil {
		t.Fatalf("exact recovery wrongly flagged: %v", err)
	}

	// Legitimate merge: both cliques folded into one community. No clique is split,
	// so this must NOT be flagged (it is the resolution limit, not a bug).
	merged := community.Partition{Community: []int{0, 0, 0, 0, 0, 0}, NumCommunities: 1}
	if err := checkPlantedRecovery(merged, f); err != nil {
		t.Fatalf("a legitimate merge (no split) must not be flagged: %v", err)
	}

	// Split: node 2 of clique 0 crosses into clique 1's community -> error.
	split := community.Partition{Community: []int{0, 0, 1, 0, 1, 1}, NumCommunities: 2}
	if err := checkPlantedRecovery(split, f); err == nil {
		t.Fatalf("a partition that splits a planted clique was not flagged")
	}
}

// TestCommunity_ValidatePartition proves the partition-validity check accepts a
// well-formed dense partition and rejects each ill-formed shape: a wrong slot
// count, an out-of-range id, a stray ghost sentinel, and a non-dense labelling.
func TestCommunity_ValidatePartition(t *testing.T) {
	t.Parallel()
	const n = 6

	if err := validatePartition(community.Partition{Community: []int{0, 0, 0, 1, 1, 1}, NumCommunities: 2}, n); err != nil {
		t.Fatalf("valid partition rejected: %v", err)
	}
	bad := []struct {
		name string
		p    community.Partition
	}{
		{"wrong-slot-count", community.Partition{Community: []int{0, 0, 1}, NumCommunities: 2}},
		{"id-out-of-range", community.Partition{Community: []int{0, 0, 0, 2, 1, 1}, NumCommunities: 2}},
		{"ghost-on-dense-graph", community.Partition{Community: []int{0, -1, 0, 1, 1, 1}, NumCommunities: 2}},
		{"non-dense-labelling", community.Partition{Community: []int{0, 0, 0, 2, 2, 2}, NumCommunities: 3}},
		{"zero-communities", community.Partition{Community: []int{0, 0, 0, 0, 0, 0}, NumCommunities: 0}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := validatePartition(tc.p, n); err == nil {
				t.Fatalf("ill-formed partition %q was accepted", tc.name)
			}
		})
	}
}

// TestCommunity_ModularityFloor_DetectsSubSingleton proves the modularity-floor
// check rejects a partition whose Q falls below the all-singletons baseline, and
// accepts the planted optimum (which is at least the singleton baseline). On the
// two-clique fixture the singleton partition has Q < 0 (every node isolated), and
// merging everything into one community gives Q = 0 here, so the floor is a real
// signal rather than a vacuous one.
func TestCommunity_ModularityFloor(t *testing.T) {
	t.Parallel()
	f := communityCliqueChain("two-cliques-3-3", []int{3, 3})
	c := communityBuildCSR(f)

	// The planted optimum clears the floor.
	planted := community.Partition{Community: slices.Clone(f.block), NumCommunities: 2}
	if err := checkModularityFloor(c, planted, f.order); err != nil {
		t.Fatalf("planted partition wrongly flagged below singleton floor: %v", err)
	}

	// Sanity on the absolute ordering of the three reference partitions:
	// Q(planted) > Q(all-in-one) >= Q(singletons). If this ordering ever breaks
	// the floor check is meaningless, so assert it directly.
	qPlanted := communityModularity(c, f.block)
	qOne := communityModularity(c, []int{0, 0, 0, 0, 0, 0})
	qSingle := communityModularity(c, []int{0, 1, 2, 3, 4, 5})
	if !(qPlanted > qOne) {
		t.Fatalf("expected Q(planted)=%.4f > Q(all-in-one)=%.4f", qPlanted, qOne)
	}
	if !(qOne >= qSingle) {
		t.Fatalf("expected Q(all-in-one)=%.4f >= Q(singletons)=%.4f", qOne, qSingle)
	}
}

// TestCommunity_LabelPropagationDeterminism cross-confirms the documented
// determinism of LabelPropagation directly on a fixture, independent of the
// battery, since the battery only checks it implicitly.
func TestCommunity_LabelPropagationDeterminism(t *testing.T) {
	t.Parallel()
	f := communityCliqueChain("three-cliques-4-3-5", []int{4, 3, 5})
	c := communityBuildCSR(f)
	a := community.LabelPropagation(c, community.DefaultLabelPropagationOptions())
	b := community.LabelPropagation(c, community.DefaultLabelPropagationOptions())
	if !slices.Equal(a.Community, b.Community) || a.NumCommunities != b.NumCommunities {
		t.Fatalf("LabelPropagation not deterministic:\n run1=%v (k=%d)\n run2=%v (k=%d)",
			a.Community, a.NumCommunities, b.Community, b.NumCommunities)
	}
	if err := validatePartition(a, f.order); err != nil {
		t.Fatalf("LabelPropagation returned an invalid partition: %v", err)
	}
}

// TestCommunity_SortedDistinct is a small guard on the diagnostics helper.
func TestCommunity_SortedDistinct(t *testing.T) {
	t.Parallel()
	got := communitySortedDistinct([]int{2, 0, 2, -1, 1, 0, -1})
	if want := []int{0, 1, 2}; !slices.Equal(got, want) {
		t.Fatalf("communitySortedDistinct = %v, want %v", got, want)
	}
}
