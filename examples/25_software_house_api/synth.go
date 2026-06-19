package main

import (
	"fmt"
	"math/rand"

	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// This file augments the hand-authored fixture (see seed.go) with a seeded,
// reproducible SYNTHETIC layer that scales the same multi-layer model up to a
// size where the module's behaviour — query latency, live heap, bytes per
// element — is actually observable. The base fixture is the deterministic
// anchor that keeps every maintenance query in SPEC.md §9 meaningful; the
// synthetic layer grows the Code/Work/People layers (and the inter-layer
// spine) around it without ever touching a hand-authored key.
//
// The topology follows a recipe vetted by the project's graph-theory
// specialist; the design intent is recorded inline so the shape is faithful
// to what the maintenance queries assume:
//
//   - DEPENDS_ON is a Price-model power-law DAG over a topological component
//     order (a few "foundational" components carry a heavy in-degree), into
//     which a bounded number of short-span back-edges inject small, countable
//     dependency cycles — never one giant strongly-connected component. So the
//     most-depended-upon query (Q6) keeps a heavy tail and the cycle query
//     (Q7) keeps returning a controllable handful of cycles.
//   - ASSIGNED_TO / TOUCHES are coupled by developer home-module affinity, so
//     ownership clusters by module: most components are touched by one or two
//     developers (bus-factor risk, Q3) and a few by many (Q2).
//   - BLOCKS is a sparse, depth-capped strict backward DAG, so the transitive
//     blocked-work query (Q5) returns shallow chains rather than a tangle.
//   - CONTAINS is a strict Repository -> Module -> Component forest.
//
// Every draw comes from a single seeded *rand.Rand in a fixed build order, so
// a given (counts, seed) reproduces an identical edge multiset.

// synthScale captures the opt-in synthetic scale knobs. The zero value adds
// nothing — seedFixture then loads exactly the hand-authored fixture, which is
// what the regression tests pin. A scaled run sets one or more *Extra fields
// and a seed.
type synthScale struct {
	components int   // extra :Component nodes to synthesise (0 = none)
	tasks      int   // extra :Task nodes to synthesise (0 = none)
	developers int   // extra :Developer nodes to synthesise (0 = none)
	seed       int64 // RNG seed; fixes the synthetic data shape
}

// active reports whether any synthetic dimension is requested. When false the
// synthetic layer is skipped entirely and the fixture is the bare hand-authored
// graph.
func (s synthScale) active() bool {
	return s.components > 0 || s.tasks > 0 || s.developers > 0
}

// validate rejects an impossible synthetic configuration once, before any
// work. Negative counts are the only way to misconfigure it; every other
// combination produces a well-defined (possibly empty) shape.
func (s synthScale) validate() error {
	switch {
	case s.components < 0:
		return fmt.Errorf("scale-components must be >= 0, got %d", s.components)
	case s.tasks < 0:
		return fmt.Errorf("scale-tasks must be >= 0, got %d", s.tasks)
	case s.developers < 0:
		return fmt.Errorf("scale-developers must be >= 0, got %d", s.developers)
	}
	return nil
}

// Synthetic-graph shape parameters. Fixed so the dataset is reproducible and
// the realism properties hold by construction rather than by luck.
const (
	synModuleSize    = 25 // mean components per synthetic module
	synDepOutMin     = 2  // minimum DEPENDS_ON out-degree per component
	synDepOutMax     = 6  // maximum DEPENDS_ON out-degree per component
	synDepAttachBias = 1  // preferential-attachment additive bias (a in in_deg+a)

	synCycleSpan   = 20  // back-edge index span: an injected cycle stays within ~W indices
	synCycleEveryN = 200 // inject ~one dependency cycle per this many components

	synTouchMin   = 1    // minimum components a synthetic task touches
	synTouchMax   = 4    // maximum components a synthetic task touches
	synTouchLocal = 0.80 // fraction of touches drawn from the task's anchor module

	synBlocksProb  = 0.15 // probability a task blocks an earlier task
	synBlocksReach = 30   // a BLOCKS edge reaches back at most this many task indices
	synBlocksDepth = 4    // maximum BLOCKS* chain depth (enforced, not hoped for)

	synDevHomeMin = 1    // minimum home modules per developer
	synDevHomeMax = 3    // maximum home modules per developer
	synCrossProb  = 0.10 // probability a task assignment crosses developer affinity

	synTasksPerSprint = 40 // synthetic tasks grouped into sprints of this size
)

// synKeys holds the generated keys for one synthetic layer so the edge phase
// can reference nodes created in the node phase.
type synKeys struct {
	repos      []string
	modules    []string
	moduleOf   []int // moduleOf[c] = index into modules for component c
	components []string
	tasks      []string
	taskStatus []string // taskStatus[t] = the status property of task t
	sprints    []string
	developers []string
	teams      []string
}

// applySynthetic grows the graph with cfg.components/tasks/developers extra
// nodes plus their edges, all drawn from a seeded RNG. It runs against the same
// transaction as the base fixture, so the whole seed (base + synthetic) commits
// atomically. Build order is fixed: Code nodes, Work nodes, People nodes, then
// every edge phase, so a given (cfg, seed) reproduces an identical graph.
func applySynthetic(tx *txn.Tx[string, float64], cfg synthScale) error {
	//nolint:gosec // G404: a seeded math/rand is intentional — the synthetic
	// dataset must reproduce a fixed shape for a given seed; crypto/rand would
	// defeat reproducibility, which is the whole point of a benchmark fixture.
	rng := rand.New(rand.NewSource(cfg.seed))
	s := &seeder{tx: tx}
	keys := &synKeys{}

	synthCodeNodes(s, rng, cfg, keys)
	synthWorkNodes(s, rng, cfg, keys)
	synthPeopleNodes(s, cfg, keys)

	synthContainsEdges(s, keys)
	synthDependsOnEdges(s, rng, keys)
	synthWorkEdges(s, rng, keys)
	synthSpineEdges(s, rng, keys)

	return s.err
}

// synthCodeNodes creates the synthetic Repository / Module / Component forest.
// Repositories hold modules; each module holds a run of components, so the
// component index order is also the CONTAINS traversal order — which the
// DEPENDS_ON phase reuses as its topological order.
func synthCodeNodes(s *seeder, rng *rand.Rand, cfg synthScale, keys *synKeys) {
	if cfg.components <= 0 {
		return
	}
	nModules := cfg.components/synModuleSize + 1
	nRepos := nModules/8 + 1

	keys.repos = make([]string, nRepos)
	for r := range keys.repos {
		key := fmt.Sprintf("syn:repo:%d", r)
		keys.repos[r] = key
		s.node(key, layerCode, typeRepository,
			ps("name", fmt.Sprintf("synthetic-repo-%d", r)))
	}

	keys.modules = make([]string, nModules)
	for m := range keys.modules {
		key := fmt.Sprintf("syn:mod:%d", m)
		keys.modules[m] = key
		s.node(key, layerCode, typeModule,
			ps("name", fmt.Sprintf("module-%d", m)),
			ps("path", fmt.Sprintf("internal/syn/m%d", m)))
	}

	keys.components = make([]string, cfg.components)
	keys.moduleOf = make([]int, cfg.components)
	for c := 0; c < cfg.components; c++ {
		// Round-robin components across modules so every module is non-empty
		// and module sizes are even; the modulo keeps the assignment O(1) and
		// deterministic.
		mod := c % nModules
		keys.moduleOf[c] = mod
		key := fmt.Sprintf("syn:comp:%d", c)
		keys.components[c] = key
		s.node(key, layerCode, typeComponent,
			ps("name", fmt.Sprintf("file%d.go", c)),
			ps("path", fmt.Sprintf("internal/syn/m%d/file%d.go", mod, c)),
			ps("kind", "file"), ps("language", "go"),
			pi("loc", int64(80+rng.Intn(600))))
	}
}

// synthWorkNodes creates the synthetic Sprint and Task nodes. WorkflowStates
// are shared with the base fixture (state:todo .. state:done), so no synthetic
// states are created.
func synthWorkNodes(s *seeder, rng *rand.Rand, cfg synthScale, keys *synKeys) {
	if cfg.tasks <= 0 {
		return
	}
	nSprints := cfg.tasks/synTasksPerSprint + 1
	keys.sprints = make([]string, nSprints)
	for sp := range keys.sprints {
		key := fmt.Sprintf("syn:sprint:%d", sp)
		keys.sprints[sp] = key
		s.node(key, layerWork, typeSprint,
			ps("name", fmt.Sprintf("Synthetic Sprint %d", sp)))
	}

	keys.tasks = make([]string, cfg.tasks)
	keys.taskStatus = make([]string, cfg.tasks)
	statusChoices := []string{"todo", "in_progress", "in_review", "done"}
	for t := 0; t < cfg.tasks; t++ {
		key := fmt.Sprintf("syn:task:%d", t)
		keys.tasks[t] = key
		status := statusChoices[rng.Intn(len(statusChoices))]
		keys.taskStatus[t] = status
		s.node(key, layerWork, typeTask,
			ps("title", fmt.Sprintf("Synthetic task %d", t)),
			ps("status", status), ps("kind", "feature"),
			pi("priority", int64(1+rng.Intn(9))))
	}
}

// synthPeopleNodes creates the synthetic Team and Developer nodes and records,
// per developer, a small home-module set used later to couple ASSIGNED_TO and
// TOUCHES into realistic ownership clusters.
func synthPeopleNodes(s *seeder, cfg synthScale, keys *synKeys) {
	if cfg.developers <= 0 {
		return
	}
	nTeams := cfg.developers/12 + 1
	keys.teams = make([]string, nTeams)
	for tm := range keys.teams {
		key := fmt.Sprintf("syn:team:%d", tm)
		keys.teams[tm] = key
		s.node(key, layerPeople, typeTeam, ps("name", fmt.Sprintf("Team %d", tm)))
	}

	keys.developers = make([]string, cfg.developers)
	for d := 0; d < cfg.developers; d++ {
		key := fmt.Sprintf("syn:dev:%d", d)
		keys.developers[d] = key
		s.node(key, layerPeople, typeDeveloper,
			ps("name", fmt.Sprintf("Dev %d", d)),
			ps("handle", fmt.Sprintf("dev%d", d)),
			ps("seniority", "mid"), pb("active", true))
	}
}

// synthContainsEdges wires the strict Repository -> Module -> Component forest:
// every module is contained by a repository (round-robin) and every component
// by its assigned module.
func synthContainsEdges(s *seeder, keys *synKeys) {
	if len(keys.modules) == 0 {
		return
	}
	nRepos := len(keys.repos)
	for m, modKey := range keys.modules {
		s.edge(keys.repos[m%nRepos], modKey, relContains)
	}
	for c, compKey := range keys.components {
		s.edge(keys.modules[keys.moduleOf[c]], compKey, relContains)
	}
}

// synthDependsOnEdges builds the DEPENDS_ON dependency graph: a Price-model
// power-law DAG (out-edges always point to a lower index, so the backbone is
// acyclic and a few foundational components attract a heavy in-degree), then a
// bounded number of short-span back-edges that inject small dependency cycles
// without coalescing the graph into one giant strongly-connected component.
func synthDependsOnEdges(s *seeder, rng *rand.Rand, keys *synKeys) {
	n := len(keys.components)
	if n < 2 {
		return
	}

	// target is an append-only multiset of component indices, each appearing
	// once per in-edge it has received. Sampling a uniform element of target
	// realises preferential attachment (in_deg + bias) in O(1) per draw,
	// because every index also enters synDepAttachBias times up front.
	target := make([]int, 0, n*synDepOutMax)
	for i := 0; i < n; i++ {
		for b := 0; b < synDepAttachBias; b++ {
			target = append(target, i)
		}
	}

	// nearTarget[i] records, for each source i, the closest lower-index target
	// it depends on (i.e. an existing forward edge i -> nearTarget[i] with
	// i - nearTarget[i] <= synCycleSpan). Reversing one such edge later
	// deterministically closes a short 2-cycle, so the cycle query always
	// returns results — without relying on a path happening to exist.
	nearTarget := make([]int, n)
	for i := range nearTarget {
		nearTarget[i] = -1
	}

	// chosen accumulates the distinct lower-index targets for source i in
	// INSERTION order (a slice, not a map): every downstream draw and edge is
	// emitted in that fixed order, so the build is deterministic. A map range
	// here would iterate in random order and perturb both the edge multiset
	// and the preferential-attachment target array across runs.
	chosen := make([]int, 0, synDepOutMax)
	inChosen := make(map[int]struct{}, synDepOutMax)
	for i := 1; i < n; i++ {
		out := synDepOutMin + rng.Intn(synDepOutMax-synDepOutMin+1)
		if out > i {
			out = i // cannot point to more distinct lower indices than exist
		}
		chosen = chosen[:0]
		clear(inChosen)
		for len(chosen) < out {
			j := target[rng.Intn(len(target))]
			if j >= i { // only attach to a strictly-lower index (keeps the backbone acyclic)
				j = rng.Intn(i)
			}
			if _, dup := inChosen[j]; dup {
				continue
			}
			inChosen[j] = struct{}{}
			chosen = append(chosen, j)
		}
		for _, j := range chosen {
			s.edge(keys.components[i], keys.components[j], relDependsOn,
				ps("kind", "call"), pi("strength", int64(1+rng.Intn(9))))
			target = append(target, j) // j gained one in-edge
			if i-j <= synCycleSpan && (nearTarget[i] < 0 || j > nearTarget[i]) {
				nearTarget[i] = j // keep the closest in-window target
			}
		}
	}

	synthInjectCycles(s, keys, nearTarget, n)
}

// synthInjectCycles closes a small, countable number of short dependency
// cycles by reversing a few existing in-window forward edges. For a chosen
// source i with forward edge i -> nearTarget[i], it adds the back-edge
// nearTarget[i] -> i, which makes {nearTarget[i], i} a 2-cycle local to a
// synCycleSpan-index window. Picking sources from distinct windows keeps the
// cycles separate so they never coalesce into one giant SCC. Deterministic: it
// walks indices in order and draws no randomness, so the same DAG yields the
// same cycles.
func synthInjectCycles(s *seeder, keys *synKeys, nearTarget []int, n int) {
	want := n / synCycleEveryN
	if want < 1 && n > synCycleSpan {
		want = 1 // guarantee at least one cycle once the graph is large enough
	}
	usedWindow := make(map[int]struct{}, want)
	made := 0
	for i := 0; i < n && made < want; i++ {
		j := nearTarget[i]
		if j < 0 {
			continue // no in-window forward edge to reverse
		}
		window := i / synCycleSpan
		if _, busy := usedWindow[window]; busy {
			continue // keep injected cycles in distinct windows
		}
		// Reverse the existing forward edge i -> j by adding j -> i, closing
		// the 2-cycle {j, i}.
		s.edge(keys.components[j], keys.components[i], relDependsOn,
			ps("kind", "cycle"), pi("strength", 1))
		usedWindow[window] = struct{}{}
		made++
	}
}

// synthWorkEdges wires the synthetic work layer: each task to a workflow state
// and a sprint, plus a sparse, depth-capped BLOCKS DAG so transitive
// blocked-work chains stay shallow (depth <= synBlocksDepth).
func synthWorkEdges(s *seeder, rng *rand.Rand, keys *synKeys) {
	n := len(keys.tasks)
	if n == 0 {
		return
	}
	nSprints := len(keys.sprints)

	// HAS_STATE and IN_SPRINT: every task gets exactly one of each. The state
	// is derived from the task's recorded status so the two stay coherent.
	stateKey := map[string]string{
		"todo": "state:todo", "in_progress": "state:in_progress",
		"in_review": "state:in_review", "done": "state:done",
	}
	for t, taskKey := range keys.tasks {
		s.edge(taskKey, stateKey[keys.taskStatus[t]], relHasState)
		s.edge(taskKey, keys.sprints[t/synTasksPerSprint%nSprints], relInSprint)
	}

	// BLOCKS: blocker (lower index) -> blocked (higher index). Sparse, with a
	// bounded backward reach and a depth cap enforced via dynamic programming
	// over the chosen edges, so BLOCKS* chains never exceed synBlocksDepth.
	depth := make([]int, n)
	for i := 1; i < n; i++ {
		if rng.Float64() >= synBlocksProb {
			continue
		}
		lo := i - synBlocksReach
		if lo < 0 {
			lo = 0
		}
		blocker := lo + rng.Intn(i-lo)
		d := depth[blocker] + 1
		if d > synBlocksDepth {
			continue // would exceed the chain-depth cap; skip this edge
		}
		s.edge(keys.tasks[blocker], keys.tasks[i], relBlocks,
			ps("reason", "synthetic dependency"))
		if d > depth[i] {
			depth[i] = d
		}
	}
}

// synthSpineEdges wires the inter-layer spine for the synthetic layer.
// MEMBER_OF assigns each developer to a team; ASSIGNED_TO and TOUCHES are
// coupled through developer home-module affinity so that ownership clusters by
// module — yielding the bus-factor distribution the maintenance queries assume.
func synthSpineEdges(s *seeder, rng *rand.Rand, keys *synKeys) {
	if len(keys.developers) > 0 && len(keys.teams) > 0 {
		nTeams := len(keys.teams)
		for d, devKey := range keys.developers {
			s.edge(devKey, keys.teams[d%nTeams], relMemberOf,
				ps("since", "2025-11-03T00:00:00Z"))
		}
	}

	// Developer home-module sets: each developer "owns" a few modules, drawn
	// once. Assignments prefer developers whose home set contains the task's
	// anchor module, which is what concentrates ownership and creates
	// bus-factor risk.
	home := keys.developerHomes(rng)

	nTasks := len(keys.tasks)
	nComps := len(keys.components)
	nDevs := len(keys.developers)
	if nTasks == 0 || nDevs == 0 {
		return
	}
	nModules := len(keys.modules)

	for t, taskKey := range keys.tasks {
		anchor := 0
		if nModules > 0 {
			anchor = rng.Intn(nModules)
		}
		synthAssign(s, rng, keys, home, taskKey, anchor)
		if nComps > 0 {
			synthTouch(s, rng, keys, taskKey, anchor, t)
		}
	}
}

// developerHomes draws, once, the home-module set of every synthetic developer
// (synDevHomeMin..synDevHomeMax modules each). Returns module index -> set of
// developer indices that call it home, the lookup the assignment phase uses to
// find affinity candidates in O(1).
func (keys *synKeys) developerHomes(rng *rand.Rand) map[int][]int {
	home := make(map[int][]int, len(keys.modules))
	nModules := len(keys.modules)
	if nModules == 0 {
		return home
	}
	for d := range keys.developers {
		k := synDevHomeMin + rng.Intn(synDevHomeMax-synDevHomeMin+1)
		seen := make(map[int]struct{}, k)
		for len(seen) < k {
			m := rng.Intn(nModules)
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			home[m] = append(home[m], d)
		}
	}
	return home
}

// synthAssign adds one or two ASSIGNED_TO edges for a task, preferring
// developers whose home set contains the task's anchor module. A small cross
// probability lets work spill across affinity (cross-team help).
func synthAssign(s *seeder, rng *rand.Rand, keys *synKeys, home map[int][]int, taskKey string, anchor int) {
	nAssign := 1
	if rng.Float64() < 0.3 {
		nAssign = 2
	}
	candidates := home[anchor]
	picked := make(map[int]struct{}, nAssign)
	for len(picked) < nAssign {
		var dev int
		if len(candidates) > 0 && rng.Float64() >= synCrossProb {
			dev = candidates[rng.Intn(len(candidates))]
		} else {
			dev = rng.Intn(len(keys.developers))
		}
		if _, dup := picked[dev]; dup {
			if len(keys.developers) <= len(picked) {
				break // not enough distinct developers to satisfy nAssign
			}
			continue
		}
		picked[dev] = struct{}{}
		s.edge(keys.developers[dev], taskKey, relAssignedTo,
			ps("role", "author"), ps("state", "active"))
	}
}

// synthTouch adds the TOUCHES edges for a task: synTouchMin..synTouchMax
// components, ~synTouchLocal of them drawn from the task's anchor module and
// the rest anywhere. Anchoring the touches in the assignment's anchor module is
// what makes ownership cluster by module.
func synthTouch(s *seeder, rng *rand.Rand, keys *synKeys, taskKey string, anchor, taskIdx int) {
	nTouch := synTouchMin + rng.Intn(synTouchMax-synTouchMin+1)
	nComps := len(keys.components)
	picked := make(map[int]struct{}, nTouch)
	for len(picked) < nTouch {
		c := keys.pickComponent(rng, anchor)
		if c < 0 {
			break // anchor module has no components and the graph is empty
		}
		if _, dup := picked[c]; dup {
			if nComps <= len(picked) {
				break
			}
			continue
		}
		picked[c] = struct{}{}
		s.edge(taskKey, keys.components[c], relTouches,
			ps("change_type", "add"),
			pi("churn", int64(10+rng.Intn(400))),
			ps("at", isoTouchDate(rng, taskIdx)))
	}
}

// pickComponent returns a component index: with probability synTouchLocal one
// from the anchor module, otherwise a uniform component. Returns -1 only when
// there are no components at all.
func (keys *synKeys) pickComponent(rng *rand.Rand, anchor int) int {
	nComps := len(keys.components)
	if nComps == 0 {
		return -1
	}
	if rng.Float64() < synTouchLocal {
		// Linear scan for a component in the anchor module is avoided by
		// reusing the round-robin layout: components with index ≡ anchor
		// (mod nModules) belong to the anchor module. Pick one of those.
		nModules := len(keys.modules)
		if nModules > 0 {
			count := (nComps - anchor + nModules - 1) / nModules
			if count > 0 {
				return anchor + rng.Intn(count)*nModules
			}
		}
	}
	return rng.Intn(nComps)
}

// isoTouchDate returns a deterministic ISO-8601 date for a synthetic TOUCHES
// edge, spread across 2026 by the task index plus a small seeded jitter, so the
// history query (Q8) can order by date meaningfully. Dates are anchored to a
// fixed year (never the wall clock) so the dataset is reproducible, and
// ISO-8601 strings sort chronologically — matching the base fixture's
// timestamp representation.
func isoTouchDate(rng *rand.Rand, taskIdx int) string {
	day := (taskIdx + rng.Intn(3)) % 365
	month := day/30 + 1
	dom := day%28 + 1
	return fmt.Sprintf("2026-%02d-%02dT16:00:00Z", month, dom)
}
